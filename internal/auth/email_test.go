package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type testSender struct {
	email, code string
	err         error
}

func (s *testSender) SendCode(_ context.Context, email, code string) error {
	s.email, s.code = email, code
	return s.err
}

func TestEmailLoginPostgres(t *testing.T) {
	url := os.Getenv("SHORTLOG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set SHORTLOG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	sender := &testSender{}
	svc := New(pool)
	login := NewEmailLogin(svc, sender, key)
	profile := &NewAccountProfile{Username: "Ari", TimeZone: "Asia/Kolkata"}
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	email := hex.EncodeToString(random) + "@example.org"
	// Remove every row created for this address; rate-limit keys are ephemeral.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanupCtx, "DELETE FROM accounts WHERE id IN (SELECT account_id FROM login_identities WHERE provider='email' AND subject=$1)", email); err != nil {
			t.Error(err)
		}
		if _, err := pool.Exec(cleanupCtx, "DELETE FROM email_login_challenges WHERE email=$1", email); err != nil {
			t.Error(err)
		}
	})
	id, err := login.Start(ctx, "  "+email+"  ", "192.0.2.13")
	if err != nil {
		t.Fatal(err)
	}
	if sender.email != email || len(sender.code) != 8 {
		t.Fatalf("unexpected delivery: %q %q", sender.email, sender.code)
	}
	wrong := "00000000"
	if sender.code == wrong {
		wrong = "11111111"
	}
	if _, err := login.Verify(ctx, id, wrong, "test", profile); !errors.Is(err, ErrInvalidChallenge) {
		t.Fatalf("wrong code: %v", err)
	}
	if _, err := login.Verify(ctx, id, sender.code, "test", nil); !errors.Is(err, ErrProfileRequired) {
		t.Fatalf("new account without profile: %v", err)
	}
	result, err := login.Verify(ctx, id, sender.code, "test", profile)
	if err != nil || result.Token == "" || result.RecoveryTicket != "" {
		t.Fatalf("verify: %+v %v", result, err)
	}
	firstPrincipal, err := svc.Authenticate(ctx, result.Token)
	if err != nil {
		t.Fatal(err)
	}
	if firstPrincipal.Username.String != "Ari" || firstPrincipal.TimeZone != "Asia/Kolkata" {
		t.Fatalf("new account profile = %+v", firstPrincipal)
	}
	oldToken := result.Token
	if _, err := login.Verify(ctx, id, sender.code, "test", profile); !errors.Is(err, ErrInvalidChallenge) {
		t.Fatalf("reused code: %v", err)
	}
	account, err := svc.resolveVerifiedIdentity(ctx, "email", email, nil)
	if err != nil {
		t.Fatal(err)
	}
	if account.ID != firstPrincipal.AccountID {
		t.Fatalf("first sign-in account = %s, identity account = %s", firstPrincipal.AccountID, account.ID)
	}
	secondID, err := login.Start(ctx, strings.ToUpper(email), "192.0.2.13")
	if err != nil {
		t.Fatal(err)
	}
	result, err = login.Verify(ctx, secondID, sender.code, "test", nil)
	if err != nil || result.Token == "" || result.RecoveryTicket != "" {
		t.Fatalf("repeat sign-in: %+v %v", result, err)
	}
	repeatToken := result.Token
	repeatPrincipal, err := svc.Authenticate(ctx, repeatToken)
	if err != nil || repeatPrincipal.AccountID != account.ID || repeatPrincipal.SessionID == firstPrincipal.SessionID {
		t.Fatalf("repeat sign-in principal = %+v, error = %v", repeatPrincipal, err)
	}
	var identityCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM login_identities WHERE provider='email' AND subject=$1", email).Scan(&identityCount); err != nil || identityCount != 1 {
		t.Fatalf("email identity count = %d, error = %v", identityCount, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE accounts SET deletion_requested_at=now() WHERE id=$1", account.ID); err != nil {
		t.Fatal(err)
	}
	thirdID, err := login.Start(ctx, email, "192.0.2.13")
	if err != nil {
		t.Fatal(err)
	}
	result, err = login.Verify(ctx, thirdID, sender.code, "test", nil)
	if err != nil || result.Token != "" || result.RecoveryTicket == "" {
		t.Fatalf("restore offered: %+v %v", result, err)
	}
	if _, err := svc.Authenticate(ctx, oldToken); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("pending-deletion account still authenticated: %v", err)
	}
	restored, err := login.Restore(ctx, result.RecoveryTicket, "test")
	if err != nil || restored == "" {
		t.Fatalf("restore: %v", err)
	}
	if _, err := svc.Authenticate(ctx, restored); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, oldToken); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("old token still active after restore: %v", err)
	}
	if _, err := svc.Authenticate(ctx, repeatToken); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("old token still active after restore: %v", err)
	}
	if _, err := login.Restore(ctx, result.RecoveryTicket, "test"); !errors.Is(err, ErrInvalidChallenge) {
		t.Fatalf("reused ticket: %v", err)
	}
}

func TestEmailLoginRateLimitPostgres(t *testing.T) {
	url := os.Getenv("SHORTLOG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set SHORTLOG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	login := NewEmailLogin(New(pool), &testSender{}, key)
	email := hex.EncodeToString(key[:8]) + "@example.org"
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM email_login_challenges WHERE email=$1", email)
	})
	for i := 0; i < 5; i++ {
		if _, err := login.Start(ctx, email, "192.0.2.14"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := login.Start(ctx, email, "192.0.2.14"); !errors.Is(err, ErrEmailRateLimit) {
		t.Fatalf("rate limit: %v", err)
	}
}

func TestEmailCodeAttemptLimitPostgres(t *testing.T) {
	url := os.Getenv("SHORTLOG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set SHORTLOG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	sender := &testSender{}
	login := NewEmailLogin(New(pool), sender, key)
	email := hex.EncodeToString(key[:8]) + "@example.org"
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM email_login_challenges WHERE email=$1", email)
	})
	id, err := login.Start(ctx, email, "192.0.2.15")
	if err != nil {
		t.Fatal(err)
	}
	wrong := "00000000"
	if sender.code == wrong {
		wrong = "11111111"
	}
	for i := 0; i < 5; i++ {
		if _, err := login.Verify(ctx, id, wrong, "test", nil); !errors.Is(err, ErrInvalidChallenge) {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	if _, err := login.Verify(ctx, id, sender.code, "test", nil); !errors.Is(err, ErrInvalidChallenge) {
		t.Fatalf("correct code after attempt limit: %v", err)
	}
}

func TestEmailCodeConcurrentRedeemPostgres(t *testing.T) {
	url := os.Getenv("SHORTLOG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set SHORTLOG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	sender := &testSender{}
	login := NewEmailLogin(New(pool), sender, key)
	email := hex.EncodeToString(key[:8]) + "@example.org"
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM accounts WHERE id IN (SELECT account_id FROM login_identities WHERE provider='email' AND subject=$1)", email)
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM email_login_challenges WHERE email=$1", email)
	})
	id, err := login.Start(ctx, email, "192.0.2.16")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = login.Verify(ctx, id, sender.code, "test", &NewAccountProfile{Username: "Ari", TimeZone: "UTC"})
		}(i)
	}
	wg.Wait()
	if !((results[0] == nil && errors.Is(results[1], ErrInvalidChallenge)) ||
		(results[1] == nil && errors.Is(results[0], ErrInvalidChallenge))) {
		t.Fatalf("concurrent redemption: %v", results)
	}
}

func TestConcurrentFirstEmailSignInPostgres(t *testing.T) {
	url := os.Getenv("SHORTLOG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set SHORTLOG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	email := hex.EncodeToString(key[:8]) + "@example.org"
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanupCtx, "DELETE FROM accounts WHERE id IN (SELECT account_id FROM login_identities WHERE provider='email' AND subject=$1)", email); err != nil {
			t.Error(err)
		}
		if _, err := pool.Exec(cleanupCtx, "DELETE FROM email_login_challenges WHERE email=$1", email); err != nil {
			t.Error(err)
		}
	})
	sender := &testSender{}
	svc := New(pool)
	login := NewEmailLogin(svc, sender, key)
	var ids [2]pgtype.UUID
	var codes [2]string
	for i := range ids {
		ids[i], err = login.Start(ctx, email, "192.0.2.20")
		if err != nil {
			t.Fatal(err)
		}
		codes[i] = sender.code
	}
	var wg sync.WaitGroup
	var results [2]EmailResult
	var errs [2]error
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = login.Verify(ctx, ids[i], codes[i], "test", &NewAccountProfile{Username: "Ari", TimeZone: "UTC"})
		}(i)
	}
	wg.Wait()
	var firstID pgtype.UUID
	for i := range results {
		if errs[i] != nil || results[i].Token == "" {
			t.Fatalf("concurrent sign-in %d: %+v, %v", i, results[i], errs[i])
		}
		principal, err := svc.Authenticate(ctx, results[i].Token)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			firstID = principal.AccountID
		} else if principal.AccountID != firstID {
			t.Fatalf("concurrent sign-ins created different accounts: %s and %s", firstID, principal.AccountID)
		}
	}
}
