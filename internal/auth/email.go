package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"shortlog-server/internal/db"
)

var ErrInvalidChallenge = errors.New("invalid or expired email challenge")
var ErrEmailRateLimit = errors.New("email login rate limited")
var ErrEmailUnavailable = errors.New("email delivery unavailable")

type CodeSender interface {
	SendCode(context.Context, string, string) error
}

type EmailLogin struct {
	service *Service
	sender  CodeSender
	key     []byte
}

func NewEmailLogin(service *Service, sender CodeSender, key []byte) *EmailLogin {
	return &EmailLogin{service: service, sender: sender, key: append([]byte(nil), key...)}
}

func normalizeEmail(value string) (string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))

	address, err := mail.ParseAddress(value)

	return value, err == nil && address.Address == value && len(value) <= 320 && len(value) >= 3 && !strings.ContainsAny(value, "\r\n")
}

func (s *EmailLogin) mac(parts ...string) []byte {
	m := hmac.New(sha256.New, s.key)

	for _, part := range parts {
		_, _ = m.Write([]byte(part))
		_, _ = m.Write([]byte{0})
	}

	return m.Sum(nil)
}

func (s *EmailLogin) Start(ctx context.Context, address, remoteIP string) (pgtype.UUID, error) {
	email, ok := normalizeEmail(address)
	if !ok {
		return pgtype.UUID{}, ErrInvalidChallenge
	}

	if s.sender == nil || len(s.key) < 32 {
		return pgtype.UUID{}, ErrEmailUnavailable
	}

	if err := s.service.queries.PruneEmailLoginLimits(ctx); err != nil {
		return pgtype.UUID{}, err
	}

	// Database upserts make rate limits effective across server instances.
	for _, limit := range []struct {
		key []byte
		max int32
	}{
		{s.mac("email", email), 5}, {s.mac("ip", remoteIP), 20},
	} {
		count, err := s.service.queries.CountEmailRequests(ctx, limit.key)

		if err != nil {
			return pgtype.UUID{}, err
		}

		if count > limit.max {
			return pgtype.UUID{}, ErrEmailRateLimit
		}
	}

	id := pgtype.UUID{Valid: true}
	if _, err := rand.Read(id.Bytes[:]); err != nil {
		return pgtype.UUID{}, err
	}

	id.Bytes[6] = (id.Bytes[6] & 0x0f) | 0x40
	id.Bytes[8] = (id.Bytes[8] & 0x3f) | 0x80
	n, err := rand.Int(rand.Reader, big.NewInt(100000000))
	if err != nil {
		return pgtype.UUID{}, err
	}

	code := fmt.Sprintf("%08d", n.Int64())
	if _, err := s.service.queries.CreateEmailChallenge(ctx, db.CreateEmailChallengeParams{
		ID: id, Email: email, CodeMac: s.mac("code", id.String(), email, code),
	}); err != nil {
		return pgtype.UUID{}, err
	}

	if err := s.sender.SendCode(ctx, email, code); err != nil {
		// Do not leave a redeemable challenge if delivery failed.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()

		if cleanupErr := s.service.queries.DeleteEmailChallenge(cleanup, id); cleanupErr != nil {
			return pgtype.UUID{}, fmt.Errorf("%w: challenge cleanup: %v", ErrEmailUnavailable, cleanupErr)
		}

		return pgtype.UUID{}, fmt.Errorf("%w: %v", ErrEmailUnavailable, err)
	}
	return id, nil
}

type EmailResult struct {
	Token          string
	RecoveryTicket string
}

func (s *EmailLogin) Verify(ctx context.Context, id pgtype.UUID, code, userAgent string, profile *NewAccountProfile) (EmailResult, error) {
	if !id.Valid || len(code) != 8 {
		return EmailResult{}, ErrInvalidChallenge
	}

	for _, ch := range code {
		if ch < '0' || ch > '9' {
			return EmailResult{}, ErrInvalidChallenge
		}
	}

	tx, err := s.service.pool.Begin(ctx)
	if err != nil {
		return EmailResult{}, err
	}
	defer tx.Rollback(ctx)

	queries := s.service.queries.WithTx(tx)
	challenge, err := queries.LockEmailChallenge(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return EmailResult{}, ErrInvalidChallenge
	}

	if err != nil {
		return EmailResult{}, err
	}

	if challenge.CompletedAt.Valid || challenge.AttemptCount >= 5 || !time.Now().Before(challenge.ExpiresAt.Time) {
		return EmailResult{}, ErrInvalidChallenge
	}

	if !hmac.Equal(challenge.CodeMac, s.mac("code", id.String(), challenge.Email, code)) {
		if err := queries.FailEmailAttempt(ctx, id); err != nil {
			return EmailResult{}, err
		}

		if err := tx.Commit(ctx); err != nil {
			return EmailResult{}, err
		}

		return EmailResult{}, ErrInvalidChallenge
	}

	account, err := s.service.resolveVerifiedIdentity(ctx, "email", challenge.Email, profile)
	if err != nil && !errors.Is(err, ErrAccountUnavailable) {
		return EmailResult{}, err
	}

	ticket := make([]byte, 32)
	if _, err := rand.Read(ticket); err != nil {
		return EmailResult{}, err
	}

	ticketHash := sha256.Sum256(ticket)

	var result EmailResult
	if errors.Is(err, ErrAccountUnavailable) {
		identity, lookupErr := s.service.queries.GetLoginIdentity(ctx, db.GetLoginIdentityParams{Provider: "email", Subject: challenge.Email})

		if lookupErr != nil {
			return EmailResult{}, lookupErr
		}

		pending, lookupErr := s.service.queries.GetAccount(ctx, identity.AccountID)
		if lookupErr != nil {
			return EmailResult{}, lookupErr
		}

		if !pending.DeletionRequestedAt.Valid || !pending.DeletionRequestedAt.Time.After(time.Now().Add(-30*24*time.Hour)) {
			return EmailResult{}, ErrInvalidChallenge
		}

		result.RecoveryTicket = base64.RawURLEncoding.EncodeToString(ticket)
	} else {
		result.Token, _, err = issueSession(ctx, queries, account.ID, userAgent)

		if err != nil {
			return EmailResult{}, err
		}
	}

	if err := queries.CompleteEmailChallenge(ctx, db.CompleteEmailChallengeParams{ID: id, CompletionTokenHash: ticketHash[:]}); err != nil {
		return EmailResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return EmailResult{}, err
	}

	return result, nil
}

func (s *EmailLogin) Restore(ctx context.Context, ticket, userAgent string) (string, error) {
	if len(ticket) != 43 {
		return "", ErrInvalidChallenge
	}

	raw, err := base64.RawURLEncoding.DecodeString(ticket)
	if err != nil || len(raw) != 32 {
		return "", ErrInvalidChallenge
	}

	hash := sha256.Sum256(raw)
	tx, err := s.service.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)

	queries := s.service.queries.WithTx(tx)
	challenge, err := queries.LockRecoveryTicket(ctx, hash[:])
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrInvalidChallenge
	}

	if err != nil {
		return "", err
	}

	accountID, err := queries.RestoreEmailAccount(ctx, challenge.Email)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrInvalidChallenge
	}

	if err != nil {
		return "", err
	}

	// Old devices must not silently regain access when the account is restored.
	if _, err := queries.RevokeAllSessions(ctx, accountID); err != nil {
		return "", err
	}
	token, _, err := issueSession(ctx, queries, accountID, userAgent)
	if err != nil {
		return "", err
	}

	replacement := make([]byte, 32)
	if _, err := rand.Read(replacement); err != nil {
		return "", err
	}

	count, err := queries.ConsumeRecoveryTicket(ctx, db.ConsumeRecoveryTicketParams{
		CompletionTokenHash: hash[:], CompletionTokenHash_2: replacement,
	})
	if err != nil {
		return "", err
	}

	if count != 1 {
		return "", ErrInvalidChallenge
	}

	if err := tx.Commit(ctx); err != nil {
		return "", err
	}

	return token, nil
}
