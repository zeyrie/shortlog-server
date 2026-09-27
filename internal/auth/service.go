package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
	_ "time/tzdata"
	"unicode"
	"unicode/utf8"

	"shortlog-server/internal/db"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalidSession     = errors.New("invalid session")
	ErrAccountUnavailable = errors.New("account unavailable")
	ErrProfileRequired    = errors.New("account profile required")
	ErrInvalidProfile     = errors.New("invalid account profile")
)

type NewAccountProfile struct {
	Username string
	TimeZone string
}

func validateNewAccountProfile(profile *NewAccountProfile) (NewAccountProfile, error) {
	if profile == nil || profile.Username == "" || profile.TimeZone == "" {
		return NewAccountProfile{}, ErrProfileRequired
	}
	username := strings.TrimSpace(profile.Username)
	if username == "" || utf8.RuneCountInString(username) > 80 || strings.ContainsFunc(username, unicode.IsControl) {
		return NewAccountProfile{}, ErrInvalidProfile
	}
	if len(profile.TimeZone) > 128 || profile.TimeZone == "Local" || strings.TrimSpace(profile.TimeZone) != profile.TimeZone {
		return NewAccountProfile{}, ErrInvalidProfile
	}
	if _, err := time.LoadLocation(profile.TimeZone); err != nil {
		return NewAccountProfile{}, ErrInvalidProfile
	}
	return NewAccountProfile{Username: username, TimeZone: profile.TimeZone}, nil
}

type Service struct {
	pool    *pgxpool.Pool
	queries *db.Queries
}

func New(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, queries: db.New(pool)}
}

type Principal struct {
	SessionID       pgtype.UUID
	AccountID       pgtype.UUID
	Username        string
	TimeZone        string
	AccountCreated  pgtype.Timestamptz
	AuthenticatedAt pgtype.Timestamptz
}

// resolveVerifiedIdentity accepts only a provider-verified subject; never call
// it with a subject claimed directly by an HTTP request.
func (s *Service) resolveVerifiedIdentity(ctx context.Context, provider, subject string, profile *NewAccountProfile) (db.Account, error) {
	if !validIdentity(provider, subject) {
		return db.Account{}, errors.New("invalid verified identity")
	}

	lookup := db.GetLoginIdentityParams{Provider: provider, Subject: subject}
	identity, err := s.queries.GetLoginIdentity(ctx, lookup)
	if err == nil {
		return s.activeAccount(ctx, identity.AccountID)
	}

	if !errors.Is(err, pgx.ErrNoRows) {
		return db.Account{}, err
	}
	newProfile, err := validateNewAccountProfile(profile)
	if err != nil {
		return db.Account{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return db.Account{}, err
	}
	defer tx.Rollback(ctx)

	queries := s.queries.WithTx(tx)
	account, err := queries.CreateAccount(ctx, db.CreateAccountParams{
		Username: newProfile.Username, TimeZone: newProfile.TimeZone,
	})
	if err != nil {
		return db.Account{}, err
	}

	_, err = queries.CreateLoginIdentity(ctx, db.CreateLoginIdentityParams{
		AccountID: account.ID, Provider: provider, Subject: subject,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.ConstraintName == "login_identities_one_account_per_subject" {
			// A concurrent sign-in created this identity first. Release the failed
			// transaction before reading the now-committed winner.
			_ = tx.Rollback(ctx)
			identity, lookupErr := s.queries.GetLoginIdentity(ctx, lookup)
			if lookupErr != nil {
				return db.Account{}, lookupErr
			}
			return s.activeAccount(ctx, identity.AccountID)
		}
		return db.Account{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return db.Account{}, err
	}

	return account, nil
}

func validIdentity(provider, subject string) bool {
	if subject == "" || len(subject) > 512 {
		return false
	}

	switch provider {
	case "email":
		return subject == strings.ToLower(subject) && len(subject) <= 320
	case "telegram", "google", "apple":
		return true
	default:
		return false
	}
}

func (s *Service) activeAccount(ctx context.Context, id pgtype.UUID) (db.Account, error) {
	account, err := s.queries.GetAccount(ctx, id)
	if err != nil {
		return db.Account{}, err
	}

	if account.DeletionRequestedAt.Valid {
		return db.Account{}, ErrAccountUnavailable // A later login flow offers restoration instead.
	}

	return account, nil
}

// issueSessionForAccount returns 32 CSPRNG bytes encoded as an unpadded base64url token.
// Because the token has 256 bits of entropy, SHA-256 (rather than a password
// KDF) is appropriate for its stored verifier. Only the digest is persisted;
// callers must deliver the token over an authenticated TLS response.
func (s *Service) issueSessionForAccount(ctx context.Context, accountID pgtype.UUID, userAgent string) (string, db.Session, error) {
	return issueSession(ctx, s.queries, accountID, userAgent)
}

func issueSession(ctx context.Context, queries *db.Queries, accountID pgtype.UUID, userAgent string) (string, db.Session, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", db.Session{}, fmt.Errorf("generate session token: %w", err)
	}

	token := base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256(raw)

	userAgent = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(userAgent, "")))

	if userAgent == "" {
		userAgent = "Unknown device"
	}
	userAgent = limitRunes(userAgent, 512)

	session, err := queries.CreateSession(ctx, db.CreateSessionParams{
		AccountID: accountID, TokenHash: hash[:], UserAgent: userAgent,
		DeviceLabel: limitRunes(userAgent, 128),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", db.Session{}, ErrAccountUnavailable
	}

	if err != nil {
		return "", db.Session{}, err
	}

	return token, session, nil
}

func limitRunes(value string, max int) string {
	if utf8.RuneCountInString(value) <= max {
		return value
	}
	return string([]rune(value)[:max])
}

func (s *Service) Authenticate(ctx context.Context, token string) (Principal, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)

	if err != nil || len(raw) != 32 {
		return Principal{}, ErrInvalidSession
	}

	hash := sha256.Sum256(raw)
	row, err := s.queries.AuthenticateSession(ctx, hash[:])
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrInvalidSession
	}

	if err != nil {
		return Principal{}, err
	}
	// A day of write coalescing paired with a 31-day persisted expiry
	// guarantees at least 30 days after the actual last authenticated use.
	if !row.LastUsedAt.Time.After(row.CheckedAt.Time.Add(-24 * time.Hour)) {
		updated, err := s.queries.TouchSession(ctx, row.SessionID)
		if err != nil {
			return Principal{}, err
		}
		if updated == 0 {
			// A concurrent request may have touched the row, or it may have
			// been revoked. Recheck before accepting the bearer token.
			row, err = s.queries.AuthenticateSession(ctx, hash[:])
			if errors.Is(err, pgx.ErrNoRows) {
				return Principal{}, ErrInvalidSession
			}
			if err != nil {
				return Principal{}, err
			}
		}
	}

	return Principal{
		SessionID: row.SessionID, AccountID: row.AccountID,
		Username: row.Username, TimeZone: row.TimeZone,
		AccountCreated: row.CreatedAt, AuthenticatedAt: row.AuthenticatedAt,
	}, nil
}

func (s *Service) ListSessions(ctx context.Context, accountID pgtype.UUID) ([]db.ListActiveSessionsRow, error) {
	return s.queries.ListActiveSessions(ctx, accountID)
}

func (s *Service) UpdateProfile(ctx context.Context, accountID pgtype.UUID, profile NewAccountProfile) (db.Account, error) {
	validated, err := validateNewAccountProfile(&profile)
	if err != nil {
		return db.Account{}, err
	}
	account, err := s.queries.UpdateAccountProfile(ctx, db.UpdateAccountProfileParams{
		ID: accountID, Username: validated.Username, TimeZone: validated.TimeZone,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Account{}, ErrAccountUnavailable
	}
	return account, err
}

func (s *Service) RevokeSession(ctx context.Context, accountID, sessionID pgtype.UUID) (bool, error) {
	count, err := s.queries.RevokeSession(ctx, db.RevokeSessionParams{ID: sessionID, AccountID: accountID})
	return count != 0, err
}

func (s *Service) RevokeAllSessions(ctx context.Context, accountID pgtype.UUID) (int64, error) {
	return s.queries.RevokeAllSessions(ctx, accountID)
}

// RequestDeletion starts the recovery window and revokes every device in the
// same transaction. Existing sessions cannot be used while deletion is pending.
func (s *Service) RequestDeletion(ctx context.Context, accountID pgtype.UUID) (time.Time, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return time.Time{}, err
	}
	defer tx.Rollback(ctx)
	queries := s.queries.WithTx(tx)
	requested, err := queries.RequestAccountDeletion(ctx, accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, ErrAccountUnavailable
	}
	if err != nil {
		return time.Time{}, err
	}
	if _, err := queries.RevokeAllSessions(ctx, accountID); err != nil {
		return time.Time{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return time.Time{}, err
	}
	return requested.Time.Add(30 * 24 * time.Hour), nil
}

// PurgeExpiredAccounts deletes up to limit expired accounts per invocation.
// Call from a scheduled job; never purge on ordinary server startup. Row locks
// serialize erasure against a concurrent restoration.
func (s *Service) PurgeExpiredAccounts(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 100 {
		return 0, errors.New("invalid purge batch size")
	}
	for purged := 0; purged < limit; purged++ {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return purged, err
		}
		queries := s.queries.WithTx(tx)
		id, err := queries.LockExpiredAccount(ctx)
		if errors.Is(err, pgx.ErrNoRows) {
			_ = tx.Rollback(ctx)
			return purged, nil
		}
		if err == nil {
			err = queries.DeleteAccountEmailChallenges(ctx, id)
		}
		if err == nil {
			err = queries.DeleteAccountTelegramAttempts(ctx, id)
		}
		var count int64
		if err == nil {
			count, err = queries.PurgeExpiredAccount(ctx, id)
		}
		if err == nil && count != 1 {
			err = errors.New("expired account disappeared during purge")
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return purged, err
		}
		if err := tx.Commit(ctx); err != nil {
			return purged, err
		}
	}
	return limit, nil
}
