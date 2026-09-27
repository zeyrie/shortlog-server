package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestValidateNewAccountProfile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile *NewAccountProfile
		want    error
	}{
		{"missing", nil, ErrProfileRequired},
		{"missing zone", &NewAccountProfile{Username: "Ari"}, ErrProfileRequired},
		{"blank username", &NewAccountProfile{Username: "  ", TimeZone: "UTC"}, ErrInvalidProfile},
		{"too long", &NewAccountProfile{Username: strings.Repeat("a", 81), TimeZone: "UTC"}, ErrInvalidProfile},
		{"control character", &NewAccountProfile{Username: "Ari\nAdmin", TimeZone: "UTC"}, ErrInvalidProfile},
		{"invalid zone", &NewAccountProfile{Username: "Ari", TimeZone: "Mars/Base"}, ErrInvalidProfile},
		{"local zone", &NewAccountProfile{Username: "Ari", TimeZone: "Local"}, ErrInvalidProfile},
		{"valid", &NewAccountProfile{Username: " Ari ", TimeZone: "Asia/Kolkata"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile, err := validateNewAccountProfile(tc.profile)
			if !errors.Is(err, tc.want) || (tc.want == nil && (profile.Username != "Ari" || profile.TimeZone != "Asia/Kolkata")) {
				t.Fatalf("validate profile = %+v, %v; want %v", profile, err, tc.want)
			}
		})
	}
}

func TestSessionLifecyclePostgres(t *testing.T) {
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
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	svc := New(pool)
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	subject := hex.EncodeToString(random)
	account, err := svc.resolveVerifiedIdentity(ctx, "telegram", subject, &NewAccountProfile{Username: "Tester", TimeZone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanupCtx, "DELETE FROM accounts WHERE id = $1", account.ID); err != nil {
			t.Errorf("cleanup account: %v", err)
		}
	})
	same, err := svc.resolveVerifiedIdentity(ctx, "telegram", subject, nil)
	if err != nil || same.ID != account.ID {
		t.Fatalf("same identity resolved to %v, %v; want %v", same.ID, err, account.ID)
	}

	token, first, err := svc.issueSessionForAccount(ctx, account.ID, "Shortlog TUI/macOS")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 32 {
		t.Fatalf("session token has wrong format: %v", err)
	}
	digest := sha256.Sum256(raw)
	if !bytes.Equal(first.TokenHash, digest[:]) {
		t.Fatal("session did not store the token's SHA-256 digest")
	}
	secondToken, second, err := svc.issueSessionForAccount(ctx, account.ID, "Shortlog TUI/Linux")
	if err != nil {
		t.Fatal(err)
	}
	if secondToken == token {
		t.Fatal("distinct sessions received the same token")
	}
	principal, err := svc.Authenticate(ctx, token)
	if err != nil || principal.AccountID != account.ID || principal.SessionID != first.ID {
		t.Fatalf("authenticate = %+v, %v", principal, err)
	}
	listed, err := svc.ListSessions(ctx, account.ID)
	if err != nil || len(listed) != 2 {
		t.Fatalf("list sessions = %d, %v; want 2", len(listed), err)
	}

	revoked, err := svc.RevokeSession(ctx, account.ID, first.ID)
	if err != nil || !revoked {
		t.Fatalf("revoke = %v, %v", revoked, err)
	}
	if _, err := svc.Authenticate(ctx, token); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("revoked token error = %v", err)
	}
	other, err := svc.resolveVerifiedIdentity(ctx, "telegram", subject+"-other", &NewAccountProfile{Username: "Other", TimeZone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanupCtx, "DELETE FROM accounts WHERE id = $1", other.ID); err != nil {
			t.Errorf("cleanup other account: %v", err)
		}
	})
	if revoked, err := svc.RevokeSession(ctx, other.ID, second.ID); err != nil || revoked {
		t.Fatalf("another account revoked a session: revoked=%v err=%v", revoked, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET last_used_at = now() - interval '23 hours' WHERE id = $1", second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, secondToken); err != nil {
		t.Fatal(err)
	}
	var lastUsed time.Time
	if err := pool.QueryRow(ctx, "SELECT last_used_at FROM sessions WHERE id = $1", second.ID).Scan(&lastUsed); err != nil {
		t.Fatal(err)
	}
	if time.Since(lastUsed) < 22*time.Hour {
		t.Fatalf("recently used session was touched early: %v", lastUsed)
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET last_used_at = now() - interval '25 hours' WHERE id = $1", second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, secondToken); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT last_used_at FROM sessions WHERE id = $1", second.ID).Scan(&lastUsed); err != nil {
		t.Fatal(err)
	}
	if time.Since(lastUsed) > time.Minute {
		t.Fatalf("daily session touch did not update last_used_at: %v", lastUsed)
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET last_used_at = now() - interval '30 days 23 hours' WHERE id = $1", second.ID); err != nil {
		t.Fatal(err)
	}
	listed, err = svc.ListSessions(ctx, account.ID)
	if err != nil || len(listed) != 1 || listed[0].ID != second.ID {
		t.Fatalf("session inside 31-day grace not listed: %+v, %v", listed, err)
	}
	if _, err := svc.Authenticate(ctx, secondToken); err != nil {
		t.Fatalf("session inside 31-day grace was rejected: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET last_used_at = now() - interval '31 days 1 minute' WHERE id = $1", second.ID); err != nil {
		t.Fatal(err)
	}
	listed, err = svc.ListSessions(ctx, account.ID)
	if err != nil || len(listed) != 0 {
		t.Fatalf("expired session still listed: %+v, %v", listed, err)
	}
	if _, err := svc.Authenticate(ctx, secondToken); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("expired token error = %v", err)
	}
	if _, err := svc.Authenticate(ctx, "not-a-token"); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("malformed token error = %v", err)
	}

	activeToken, _, err := svc.issueSessionForAccount(ctx, account.ID, "Shortlog TUI/macOS")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE accounts SET deletion_requested_at = now() WHERE id = $1", account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, activeToken); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("deleted-account token error = %v", err)
	}
	if _, _, err := svc.issueSessionForAccount(ctx, account.ID, "Shortlog TUI/macOS"); !errors.Is(err, ErrAccountUnavailable) {
		t.Fatalf("deleted-account issue error = %v", err)
	}
	if _, err := svc.resolveVerifiedIdentity(ctx, "telegram", subject, nil); !errors.Is(err, ErrAccountUnavailable) {
		t.Fatalf("deleted-account identity error = %v", err)
	}
}
