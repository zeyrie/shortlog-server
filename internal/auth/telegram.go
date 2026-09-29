package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/oauth2"
	"shortlog-server/internal/db"
)

const telegramIssuer = "https://oauth.telegram.org"

type TelegramConfig struct {
	ClientID, ClientSecret, RedirectURI string
	EncryptionKey                       []byte
}

type TelegramLogin struct {
	service  *Service
	config   oauth2.Config
	provider *oidc.Provider
	client   *http.Client
	key      []byte
}

// NewTelegramLogin pins the issuer to Telegram; discovery, keys, and tokens
// are never supplied by an HTTP caller.
func NewTelegramLogin(ctx context.Context, service *Service, config TelegramConfig) (*TelegramLogin, error) {
	return newTelegramLogin(ctx, service, config, telegramIssuer)
}

func newTelegramLogin(ctx context.Context, service *Service, config TelegramConfig, issuer string) (*TelegramLogin, error) {
	if config.ClientID == "" || config.ClientSecret == "" || config.RedirectURI == "" || len(config.EncryptionKey) != 32 {
		return nil, errors.New("incomplete Telegram OIDC configuration")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, client), issuer)
	if err != nil {
		return nil, fmt.Errorf("Telegram OIDC discovery: %w", err)
	}
	return &TelegramLogin{service: service, provider: provider, client: client, key: append([]byte(nil), config.EncryptionKey...),
		config: oauth2.Config{ClientID: config.ClientID, ClientSecret: config.ClientSecret, RedirectURL: config.RedirectURI,
			Scopes: []string{oidc.ScopeOpenID, "profile"}, Endpoint: oauth2.Endpoint{
				AuthURL: provider.Endpoint().AuthURL, TokenURL: provider.Endpoint().TokenURL, AuthStyle: oauth2.AuthStyleInHeader,
			}},
	}, nil
}

func randomURLToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func decodeURLToken(token string) ([]byte, error) {
	if len(token) != 43 {
		return nil, ErrInvalidChallenge
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 32 {
		return nil, ErrInvalidChallenge
	}
	return raw, nil
}

func (s *TelegramLogin) encryptVerifier(verifier string, stateHash []byte) ([]byte, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(verifier), stateHash), nil
}

func (s *TelegramLogin) decryptVerifier(data, stateHash []byte) (string, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || len(data) < gcm.NonceSize() {
		return "", ErrInvalidChallenge
	}
	plain, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], stateHash)
	if err != nil {
		return "", ErrInvalidChallenge
	}
	return string(plain), nil
}

type TelegramStart struct {
	AttemptID        pgtype.UUID
	PollSecret       string
	AuthorizationURL string
}

func (s *TelegramLogin) Start(ctx context.Context, remoteIP string) (TelegramStart, error) {
	// Keep the unauthenticated start endpoint bounded across server instances.
	m := hmac.New(sha256.New, s.key)
	_, _ = m.Write([]byte("telegram-start-ip:" + remoteIP))
	count, err := s.service.queries.CountAuthRequests(ctx, m.Sum(nil))
	if err != nil {
		return TelegramStart{}, err
	}
	if count > 20 {
		return TelegramStart{}, ErrLoginRateLimit
	}
	if err := s.service.queries.PruneTelegramAttempts(ctx); err != nil {
		return TelegramStart{}, err
	}
	state, err := randomURLToken()
	if err != nil {
		return TelegramStart{}, err
	}
	verifier, err := randomURLToken()
	if err != nil {
		return TelegramStart{}, err
	}
	nonce, err := randomURLToken()
	if err != nil {
		return TelegramStart{}, err
	}
	poll, err := randomURLToken()
	if err != nil {
		return TelegramStart{}, err
	}
	stateHash := sha256.Sum256([]byte(state))
	pollHash := sha256.Sum256([]byte(poll))
	ciphertext, err := s.encryptVerifier(verifier, stateHash[:])
	if err != nil {
		return TelegramStart{}, err
	}
	id, err := s.service.queries.CreateTelegramAttempt(ctx, db.CreateTelegramAttemptParams{
		StateHash: stateHash[:], PkceVerifierCiphertext: ciphertext, Nonce: nonce, PollSecretHash: pollHash[:],
	})
	if err != nil {
		return TelegramStart{}, err
	}
	return TelegramStart{AttemptID: id, PollSecret: poll,
		AuthorizationURL: s.config.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("nonce", nonce)),
	}, nil
}

// Callback consumes state before exchanging the code. Neither the code nor
// Telegram's ID token is returned to the browser or the polling client.
func (s *TelegramLogin) Callback(ctx context.Context, state, code string) error {
	if _, err := decodeURLToken(state); err != nil {
		return ErrInvalidChallenge
	}
	if code == "" || len(code) > 2048 {
		return ErrInvalidChallenge
	}
	stateHash := sha256.Sum256([]byte(state))
	attempt, err := s.service.queries.ClaimTelegramCallback(ctx, stateHash[:])
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidChallenge
	}
	if err != nil {
		return err
	}
	verifier, err := s.decryptVerifier(attempt.PkceVerifierCiphertext, stateHash[:])
	if err != nil {
		return err
	}
	ctx = oidc.ClientContext(ctx, s.client)
	tokens, err := s.config.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return ErrInvalidChallenge
	}
	rawIDToken, ok := tokens.Extra("id_token").(string)
	if !ok || len(rawIDToken) > 16384 {
		return ErrInvalidChallenge
	}
	verified, err := s.provider.VerifierContext(ctx, &oidc.Config{
		ClientID: s.config.ClientID, SupportedSigningAlgs: []string{"RS256"},
	}).Verify(ctx, rawIDToken)
	now := time.Now()
	if err != nil || verified.Nonce != attempt.Nonce || verified.Subject == "" || len(verified.Subject) > 512 ||
		len(verified.Audience) != 1 || verified.Audience[0] != s.config.ClientID ||
		verified.IssuedAt.IsZero() || verified.IssuedAt.After(now.Add(2*time.Minute)) || verified.IssuedAt.Before(now.Add(-12*time.Minute)) {
		return ErrInvalidChallenge
	}
	count, err := s.service.queries.ApproveTelegramAttempt(ctx, db.ApproveTelegramAttemptParams{
		ID: attempt.ID, TelegramSubject: pgtype.Text{String: verified.Subject, Valid: true},
	})
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrInvalidChallenge
	}
	return nil
}

type TelegramResult struct {
	Status         string
	Token          string
	RecoveryTicket string
}

func (s *TelegramLogin) Poll(ctx context.Context, id pgtype.UUID, secret, userAgent string, profile *NewAccountProfile) (TelegramResult, error) {
	if !id.Valid {
		return TelegramResult{}, ErrInvalidChallenge
	}
	if _, err := decodeURLToken(secret); err != nil {
		return TelegramResult{}, err
	}
	hash := sha256.Sum256([]byte(secret))
	tx, err := s.service.pool.Begin(ctx)
	if err != nil {
		return TelegramResult{}, err
	}
	defer tx.Rollback(ctx)
	queries := s.service.queries.WithTx(tx)
	attempt, err := queries.LockTelegramAttempt(ctx, db.LockTelegramAttemptParams{ID: id, PollSecretHash: hash[:]})
	if errors.Is(err, pgx.ErrNoRows) {
		return TelegramResult{}, ErrInvalidChallenge
	}
	if err != nil {
		return TelegramResult{}, err
	}
	if attempt.CompletedAt.Valid || !time.Now().Before(attempt.ExpiresAt.Time) {
		return TelegramResult{}, ErrInvalidChallenge
	}
	if !attempt.ApprovedAt.Valid {
		return TelegramResult{Status: "pending"}, nil
	}
	if !attempt.TelegramSubject.Valid {
		return TelegramResult{}, ErrInvalidChallenge
	}
	account, err := s.service.resolveVerifiedIdentity(ctx, "telegram", attempt.TelegramSubject.String, profile)
	if err != nil && !errors.Is(err, ErrAccountUnavailable) {
		return TelegramResult{}, err
	}
	var result TelegramResult
	var recoveryHash []byte
	if errors.Is(err, ErrAccountUnavailable) {
		identity, lookupErr := s.service.queries.GetLoginIdentity(ctx, db.GetLoginIdentityParams{Provider: "telegram", Subject: attempt.TelegramSubject.String})
		if lookupErr != nil {
			return TelegramResult{}, lookupErr
		}
		pending, lookupErr := s.service.queries.GetAccount(ctx, identity.AccountID)
		if lookupErr != nil {
			return TelegramResult{}, lookupErr
		}
		if !pending.DeletionRequestedAt.Valid || !pending.DeletionRequestedAt.Time.After(time.Now().Add(-30*24*time.Hour)) {
			return TelegramResult{}, ErrInvalidChallenge
		}
		ticket, err := randomURLToken()
		if err != nil {
			return TelegramResult{}, err
		}
		sum := sha256.Sum256([]byte(ticket))
		recoveryHash = sum[:]
		result = TelegramResult{Status: "restore_required", RecoveryTicket: ticket}
	} else {
		token, _, err := issueSession(ctx, queries, account.ID, userAgent)
		if err != nil {
			return TelegramResult{}, err
		}
		result = TelegramResult{Status: "signed_in", Token: token}
	}
	count, err := queries.CompleteTelegramAttempt(ctx, db.CompleteTelegramAttemptParams{ID: id, RecoveryTicketHash: recoveryHash})
	if err != nil {
		return TelegramResult{}, err
	}
	if count != 1 {
		return TelegramResult{}, ErrInvalidChallenge
	}
	if err := tx.Commit(ctx); err != nil {
		return TelegramResult{}, err
	}
	return result, nil
}

func (s *TelegramLogin) Restore(ctx context.Context, ticket, userAgent string) (string, error) {
	if _, err := decodeURLToken(ticket); err != nil {
		return "", err
	}
	hash := sha256.Sum256([]byte(ticket))
	tx, err := s.service.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	queries := s.service.queries.WithTx(tx)
	attempt, err := queries.LockTelegramRecovery(ctx, hash[:])
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrInvalidChallenge
	}
	if err != nil {
		return "", err
	}
	if !attempt.TelegramSubject.Valid {
		return "", ErrInvalidChallenge
	}
	accountID, err := queries.RestoreTelegramAccount(ctx, attempt.TelegramSubject.String)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrInvalidChallenge
	}
	if err != nil {
		return "", err
	}
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
	count, err := queries.ConsumeTelegramRecovery(ctx, db.ConsumeTelegramRecoveryParams{
		RecoveryTicketHash: hash[:], RecoveryTicketHash_2: replacement,
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
