package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestTelegramLoginPostgres(t *testing.T) {
	databaseURL := os.Getenv("SHORTLOG_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set SHORTLOG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var server *httptest.Server
	var nonce, challenge, subject, mode string
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer": server.URL, "authorization_endpoint": server.URL + "/auth",
				"token_endpoint": server.URL + "/token", "jwks_uri": server.URL + "/jwks",
			})
		case "/jwks":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
				"kty": "RSA", "kid": "test", "use": "sig", "alg": "RS256",
				"n": base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
			}}})
		case "/token":
			client, secret, ok := r.BasicAuth()
			if !ok || client != "test-client" || secret != "test-secret" || r.FormValue("code") != "test-code" || r.FormValue("redirect_uri") != server.URL+"/v1/auth/telegram/callback" {
				http.Error(w, "invalid exchange", http.StatusBadRequest)
				return
			}
			verifierHash := sha256.Sum256([]byte(r.FormValue("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(verifierHash[:]) != challenge {
				http.Error(w, "invalid PKCE", http.StatusBadRequest)
				return
			}
			value := nonce
			if mode == "wrong nonce" {
				value = "unrelated"
			}
			claims, _ := json.Marshal(map[string]any{
				"iss": server.URL, "aud": "test-client", "sub": subject,
				"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(), "nonce": value,
			})
			if mode == "wrong audience" || mode == "multiple audiences" || mode == "expired" || mode == "wrong issuer" || mode == "old issuance" || mode == "future issuance" {
				payload := map[string]any{}
				_ = json.Unmarshal(claims, &payload)
				if mode == "wrong audience" {
					payload["aud"] = "someone-else"
				}
				if mode == "multiple audiences" {
					payload["aud"] = []string{"test-client", "someone-else"}
				}
				if mode == "expired" {
					payload["exp"] = time.Now().Add(-time.Minute).Unix()
				}
				if mode == "wrong issuer" {
					payload["iss"] = "https://not-telegram.example"
				}
				if mode == "old issuance" {
					payload["iat"] = time.Now().Add(-time.Hour).Unix()
				}
				if mode == "future issuance" {
					payload["iat"] = time.Now().Add(time.Hour).Unix()
				}
				claims, _ = json.Marshal(payload)
			}
			header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"test"}`))
			unsigned := header + "." + base64.RawURLEncoding.EncodeToString(claims)
			digest := sha256.Sum256([]byte(unsigned))
			sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
			if err != nil {
				t.Error(err)
				return
			}
			if mode == "bad signature" {
				sig[0] ^= 1
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "test", "token_type": "Bearer", "id_token": unsigned + "." + base64.RawURLEncoding.EncodeToString(sig)})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	svc := New(pool)
	login, err := newTelegramLogin(ctx, svc, TelegramConfig{
		ClientID: "test-client", ClientSecret: "test-secret", RedirectURI: server.URL + "/v1/auth/telegram/callback", EncryptionKey: secret,
	}, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	subject, err = randomURLToken()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanup, "DELETE FROM accounts WHERE id IN (SELECT account_id FROM login_identities WHERE provider='telegram' AND subject=$1)", subject); err != nil {
			t.Error(err)
		}
	})
	var attemptIDs []string
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, id := range attemptIDs {
			if _, err := pool.Exec(cleanup, "DELETE FROM telegram_login_attempts WHERE id=$1", id); err != nil {
				t.Error(err)
			}
		}
	})
	start := func() (TelegramStart, string) {
		t.Helper()
		result, err := login.Start(ctx, "192.0.2.21")
		if err != nil {
			t.Fatal(err)
		}
		attemptIDs = append(attemptIDs, result.AttemptID.String())
		u, err := url.Parse(result.AuthorizationURL)
		if err != nil || u.Host != strings.TrimPrefix(server.URL, "http://") || u.Query().Get("code_challenge_method") != "S256" || u.Query().Get("scope") != "openid profile" {
			t.Fatalf("authorization URL = %s, %v", result.AuthorizationURL, err)
		}
		nonce, challenge = u.Query().Get("nonce"), u.Query().Get("code_challenge")
		return result, u.Query().Get("state")
	}
	first, state := start()
	wrongState, err := randomURLToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := login.Callback(ctx, wrongState, "test-code"); err != ErrInvalidChallenge {
		t.Fatalf("wrong state accepted: %v", err)
	}
	if result, err := login.Poll(ctx, first.AttemptID, first.PollSecret, "test", nil); err != nil || result.Status != "pending" {
		t.Fatalf("unapproved poll: %+v, %v", result, err)
	}
	if err := login.Callback(ctx, state, "test-code"); err != nil {
		t.Fatal(err)
	}
	if err := login.Callback(ctx, state, "test-code"); err != ErrInvalidChallenge {
		t.Fatalf("replayed callback = %v", err)
	}
	if _, err := login.Poll(ctx, first.AttemptID, "wrong", "test", nil); err != ErrInvalidChallenge {
		t.Fatalf("wrong poll secret = %v", err)
	}
	if _, err := login.Poll(ctx, first.AttemptID, first.PollSecret, "test", nil); err != ErrProfileRequired {
		t.Fatalf("missing profile = %v", err)
	}
	result, err := login.Poll(ctx, first.AttemptID, first.PollSecret, "test", &NewAccountProfile{Username: "Telegram user", TimeZone: "UTC"})
	if err != nil || result.Status != "signed_in" || result.Token == "" {
		t.Fatalf("first sign-in: %+v, %v", result, err)
	}
	principal, err := svc.Authenticate(ctx, result.Token)
	if err != nil || principal.Username != "Telegram user" {
		t.Fatalf("principal: %+v, %v", principal, err)
	}
	if _, err := login.Poll(ctx, first.AttemptID, first.PollSecret, "test", nil); err != ErrInvalidChallenge {
		t.Fatalf("replayed poll = %v", err)
	}
	second, state := start()
	if err := login.Callback(ctx, state, "test-code"); err != nil {
		t.Fatal(err)
	}
	result, err = login.Poll(ctx, second.AttemptID, second.PollSecret, "test", nil)
	if err != nil || result.Status != "signed_in" {
		t.Fatalf("repeat sign-in: %+v, %v", result, err)
	}
	other, err := svc.Authenticate(ctx, result.Token)
	if err != nil || other.AccountID != principal.AccountID {
		t.Fatalf("repeat account: %+v, %v", other, err)
	}
	oldToken := result.Token
	if _, err := pool.Exec(ctx, "UPDATE accounts SET deletion_requested_at=now() WHERE id=$1", principal.AccountID); err != nil {
		t.Fatal(err)
	}
	third, state := start()
	if err := login.Callback(ctx, state, "test-code"); err != nil {
		t.Fatal(err)
	}
	result, err = login.Poll(ctx, third.AttemptID, third.PollSecret, "test", nil)
	if err != nil || result.Status != "restore_required" || result.Token != "" || result.RecoveryTicket == "" {
		t.Fatalf("restore offer: %+v, %v", result, err)
	}
	newToken, err := login.Restore(ctx, result.RecoveryTicket, "test")
	if err != nil || newToken == "" {
		t.Fatalf("restore: %v", err)
	}
	if _, err := svc.Authenticate(ctx, newToken); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, oldToken); err != ErrInvalidSession {
		t.Fatalf("old session unexpectedly active: %v", err)
	}
	if _, err := login.Restore(ctx, result.RecoveryTicket, "test"); err != ErrInvalidChallenge {
		t.Fatalf("replayed recovery: %v", err)
	}
	for _, invalid := range []string{"wrong nonce", "wrong audience", "multiple audiences", "expired", "wrong issuer", "old issuance", "future issuance", "bad signature"} {
		mode = invalid
		attempt, state := start()
		if err := login.Callback(ctx, state, "test-code"); err != ErrInvalidChallenge {
			t.Fatalf("%s token accepted: %v", invalid, err)
		}
		if result, err := login.Poll(ctx, attempt.AttemptID, attempt.PollSecret, "test", nil); err != nil || result.Status != "pending" {
			t.Fatalf("%s token approved: %+v, %v", invalid, result, err)
		}
	}
}
