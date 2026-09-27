package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"shortlog-server/internal/db"
	"shortlog-server/internal/notes"
	"shortlog-server/internal/projects"
)

func TestAccountDeletionAndPurgePostgres(t *testing.T) {
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
	queries := db.New(pool)
	svc := New(pool)
	account, err := queries.CreateAccount(ctx, db.CreateAccountParams{Username: "Deletion test", TimeZone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanup, "DELETE FROM accounts WHERE id=$1", account.ID); err != nil {
			t.Error(err)
		}
	})
	unique := make([]byte, 12)
	if _, err := rand.Read(unique); err != nil {
		t.Fatal(err)
	}
	email := hex.EncodeToString(unique) + "@example.org"
	telegram := hex.EncodeToString(unique)
	for _, identity := range []struct{ provider, subject string }{{"email", email}, {"telegram", telegram}} {
		if _, err := queries.CreateLoginIdentity(ctx, db.CreateLoginIdentityParams{
			AccountID: account.ID, Provider: identity.provider, Subject: identity.subject,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mac := sha256.Sum256(unique)
	pollHash := sha256.Sum256(mac[:])
	if _, err := pool.Exec(ctx, `INSERT INTO email_login_challenges (email, purpose, code_mac, expires_at)
		VALUES ($1, 'sign_in', $2, now() + interval '10 minutes')`, email, mac[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO telegram_login_attempts
		(state_hash, pkce_verifier_ciphertext, nonce, poll_secret_hash, purpose, expires_at, telegram_subject, approved_at)
		VALUES ($1, $2, $3, $4, 'sign_in', now() + interval '10 minutes', $5, now())`,
		mac[:], make([]byte, 80), "0123456789abcdef", pollHash[:], telegram); err != nil {
		t.Fatal(err)
	}
	project, err := projects.New(pool).Create(ctx, account.ID, projects.CreateInput{Name: "Private"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := notes.New(pool).Create(ctx, account.ID, "private inbox", project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := notes.New(pool).Create(ctx, account.ID, "private Inbox", db.Note{}.ProjectID); err != nil {
		t.Fatal(err)
	}
	token, _, err := svc.issueSessionForAccount(ctx, account.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	until, err := svc.RequestDeletion(ctx, account.ID)
	if err != nil || until.Before(time.Now().Add(29*24*time.Hour)) {
		t.Fatalf("deletion deadline %v, %v", until, err)
	}
	if _, err := svc.Authenticate(ctx, token); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("old token: %v", err)
	}
	if _, err := svc.RequestDeletion(ctx, account.ID); !errors.Is(err, ErrAccountUnavailable) {
		t.Fatalf("repeat deletion: %v", err)
	}
	if _, _, err := svc.issueSessionForAccount(ctx, account.ID, "test"); !errors.Is(err, ErrAccountUnavailable) {
		t.Fatalf("new session: %v", err)
	}
	if count, err := svc.PurgeExpiredAccounts(ctx, 100); err != nil || count != 0 {
		t.Fatalf("premature purge: %d, %v", count, err)
	}
	// Expire only this test account; never adjust the purge predicate itself.
	if _, err := pool.Exec(ctx, "UPDATE accounts SET deletion_requested_at = now() - interval '30 days' - interval '1 second' WHERE id=$1", account.ID); err != nil {
		t.Fatal(err)
	}
	if count, err := svc.PurgeExpiredAccounts(ctx, 100); err != nil || count != 1 {
		t.Fatalf("purge: %d, %v", count, err)
	}
	if count, err := svc.PurgeExpiredAccounts(ctx, 100); err != nil || count != 0 {
		t.Fatalf("repeat purge: %d, %v", count, err)
	}
	for _, item := range []struct {
		table, predicate string
		value            any
	}{
		{"accounts", "id", account.ID}, {"login_identities", "account_id", account.ID},
		{"sessions", "account_id", account.ID}, {"projects", "account_id", account.ID},
		{"notes", "account_id", account.ID}, {"email_login_challenges", "email", email},
		{"telegram_login_attempts", "telegram_subject", telegram},
	} {
		var count int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+item.table+" WHERE "+item.predicate+"=$1", item.value).Scan(&count); err != nil || count != 0 {
			t.Fatalf("remaining %s rows: %d, %v", item.table, count, err)
		}
	}
	if _, err := queries.GetAccount(ctx, account.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("purged account: %v", err)
	}
}
