package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"shortlog-server/internal/apierror"
	"shortlog-server/internal/auth"
	"shortlog-server/internal/db"
)

type fakeDatabase struct{ err error }

func (f fakeDatabase) Ping(context.Context) error { return f.err }

type fakeSessions struct {
	err           error
	waitForCancel bool
}

func (f fakeSessions) Authenticate(ctx context.Context, _ string) (auth.Principal, error) {
	if f.waitForCancel {
		<-ctx.Done()
		return auth.Principal{}, ctx.Err()
	}
	return auth.Principal{}, f.err
}
func (fakeSessions) ListSessions(context.Context, pgtype.UUID) ([]db.ListActiveSessionsRow, error) {
	return nil, nil
}
func (fakeSessions) RevokeSession(context.Context, pgtype.UUID, pgtype.UUID) (bool, error) {
	return false, nil
}
func (fakeSessions) RevokeAllSessions(context.Context, pgtype.UUID) (int64, error) {
	return 0, nil
}
func (fakeSessions) UpdateProfile(context.Context, pgtype.UUID, auth.NewAccountProfile) (db.Account, error) {
	return db.Account{}, nil
}

func TestAPIErrorResponses(t *testing.T) {
	handler := newHandler(fakeDatabase{err: errors.New("database secret")}, fakeSessions{err: errors.New("session secret")}, nil, nil, nil)
	for _, tc := range []struct {
		name, method, path, authorization string
		status                            int
		code                              apierror.Code
	}{
		{"missing token", http.MethodGet, "/v1/me", "", http.StatusUnauthorized, apierror.Unauthorized},
		{"invalid token", http.MethodGet, "/v1/me", "Bearer invalid", http.StatusUnauthorized, apierror.Unauthorized},
		{"database error", http.MethodGet, "/readyz", "", http.StatusServiceUnavailable, apierror.ServiceUnavailable},
		{"unknown route", http.MethodGet, "/v1/unknown", "", http.StatusNotFound, apierror.NotFound},
		{"unsupported method", http.MethodPost, "/v1/me", "", http.StatusMethodNotAllowed, apierror.MethodNotAllowed},
		{"internal failure", http.MethodGet, "/v1/me", "Bearer " + strings.Repeat("a", 43), http.StatusInternalServerError, apierror.Internal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.authorization != "" {
				r.Header.Set("Authorization", tc.authorization)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			var body struct {
				Error struct {
					Code apierror.Code `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if w.Code != tc.status || body.Error.Code != tc.code {
				t.Fatalf("response = %d %s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "secret") {
				t.Fatal("internal error leaked to response")
			}
		})
	}
}

func TestProtectedRequestDeadline(t *testing.T) {
	handler := newHandler(fakeDatabase{}, fakeSessions{waitForCancel: true}, nil, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	r := httptest.NewRequest(http.MethodGet, "/v1/me", nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 43))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("deadline response = %d %s, want 503", w.Code, w.Body.String())
	}
}

func TestHealth(t *testing.T) {
	response := httptest.NewRecorder()
	newHandler(fakeDatabase{err: errors.New("offline")}, nil, nil, nil, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("health status = %d, want %d", response.Code, http.StatusOK)
	}
}

func TestProtectedRoutesPostgres(t *testing.T) {
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
	svc := auth.New(pool)
	account, err := db.New(pool).CreateAccount(ctx, db.CreateAccountParams{Username: "Test account", TimeZone: "UTC"})
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
	newTestSession := func(label string) (string, db.Session, error) {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return "", db.Session{}, err
		}
		hash := sha256.Sum256(raw)
		session, err := db.New(pool).CreateSession(ctx, db.CreateSessionParams{
			AccountID: account.ID, TokenHash: hash[:], UserAgent: label, DeviceLabel: label,
		})
		return base64.RawURLEncoding.EncodeToString(raw), session, err
	}
	token, session, err := newTestSession("Shortlog TUI/macOS")
	if err != nil {
		t.Fatal(err)
	}
	handler := newHandler(pool, svc, nil, nil, nil)
	request := func(method, path, bearer string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, nil)
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if got := request(http.MethodGet, "/v1/me", "").Code; got != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /me status = %d", got)
	}
	me := request(http.MethodGet, "/v1/me", token)
	if me.Code != http.StatusOK || !strings.Contains(me.Body.String(), account.ID.String()) {
		t.Fatalf("/me = %d %s", me.Code, me.Body.String())
	}
	profileRequest := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPatch, "/v1/me", bytes.NewBufferString(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if got := profileRequest(`{"username":"Tester","time_zone":"Mars/Base"}`).Code; got != http.StatusBadRequest {
		t.Fatalf("invalid profile status = %d", got)
	}
	updated := profileRequest(`{"username":" Tester ","time_zone":"Asia/Kolkata"}`)
	if updated.Code != http.StatusOK || !strings.Contains(updated.Body.String(), `"username":"Tester"`) {
		t.Fatalf("updated profile = %d %s", updated.Code, updated.Body.String())
	}
	me = request(http.MethodGet, "/v1/me", token)
	if !strings.Contains(me.Body.String(), `"time_zone":"Asia/Kolkata"`) {
		t.Fatalf("/me after profile update = %d %s", me.Code, me.Body.String())
	}
	listed := request(http.MethodGet, "/v1/sessions", token)
	var sessions []struct {
		ID      string `json:"id"`
		Current bool   `json:"current"`
	}
	if listed.Code != http.StatusOK || json.Unmarshal(listed.Body.Bytes(), &sessions) != nil || len(sessions) != 1 || !sessions[0].Current {
		t.Fatalf("/sessions = %d %s", listed.Code, listed.Body.String())
	}
	if got := request(http.MethodDelete, "/v1/sessions/invalid", token).Code; got != http.StatusNotFound {
		t.Fatalf("invalid session ID status = %d", got)
	}
	if got := request(http.MethodDelete, "/v1/sessions/"+session.ID.String(), token).Code; got != http.StatusNoContent {
		t.Fatalf("revoke status = %d", got)
	}
	if got := request(http.MethodGet, "/v1/me", token).Code; got != http.StatusUnauthorized {
		t.Fatalf("revoked /me status = %d", got)
	}
	secondToken, _, err := newTestSession("Shortlog TUI/Linux")
	if err != nil {
		t.Fatal(err)
	}
	if got := request(http.MethodPost, "/v1/sessions/revoke-all", secondToken).Code; got != http.StatusNoContent {
		t.Fatalf("revoke-all status = %d", got)
	}
	if got := request(http.MethodGet, "/v1/me", secondToken).Code; got != http.StatusUnauthorized {
		t.Fatalf("revoke-all /me status = %d", got)
	}
	logoutToken, _, err := newTestSession("Shortlog TUI/logout")
	if err != nil {
		t.Fatal(err)
	}
	otherToken, _, err := newTestSession("Shortlog TUI/other")
	if err != nil {
		t.Fatal(err)
	}
	if got := request(http.MethodPost, "/v1/auth/logout", logoutToken).Code; got != http.StatusNoContent {
		t.Fatalf("logout status = %d", got)
	}
	if got := request(http.MethodGet, "/v1/me", logoutToken).Code; got != http.StatusUnauthorized {
		t.Fatalf("logged-out /me status = %d", got)
	}
	if got := request(http.MethodGet, "/v1/me", otherToken).Code; got != http.StatusOK {
		t.Fatalf("other device after logout /me status = %d", got)
	}
}

func TestReady(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want int
	}{
		{name: "connected", want: http.StatusOK},
		{name: "disconnected", err: errors.New("offline"), want: http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			newHandler(fakeDatabase{err: test.err}, nil, nil, nil, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if response.Code != test.want {
				t.Fatalf("ready status = %d, want %d", response.Code, test.want)
			}
		})
	}
}

type capturedCode struct{ code string }

func (c *capturedCode) SendCode(_ context.Context, _, code string) error { c.code = code; return nil }

func TestEmailHTTPPostgres(t *testing.T) {
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
	svc := auth.New(pool)
	sender := &capturedCode{}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	handler := newHandler(pool, svc, auth.NewEmailLogin(svc, sender, key), nil, nil)
	email := hex.EncodeToString(key[:8]) + "@example.org"
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM accounts WHERE id IN (SELECT account_id FROM login_identities WHERE provider='email' AND subject=$1)", email)
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM email_login_challenges WHERE email=$1", email)
	})
	request := func(path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	start := request("/v1/auth/email/start", `{"email":"`+email+`"}`)
	var challenge struct {
		ChallengeID string `json:"challenge_id"`
	}
	if start.Code != http.StatusAccepted || json.Unmarshal(start.Body.Bytes(), &challenge) != nil || challenge.ChallengeID == "" {
		t.Fatalf("start: %d %s", start.Code, start.Body.String())
	}
	bareCode := `{"challenge_id":"` + challenge.ChallengeID + `","code":"` + sender.code + `"}`
	missingProfile := request("/v1/auth/email/verify", bareCode)
	if missingProfile.Code != http.StatusUnprocessableEntity || !strings.Contains(missingProfile.Body.String(), `"profile_required"`) {
		t.Fatalf("new account without profile: %d %s", missingProfile.Code, missingProfile.Body.String())
	}
	invalidZone := request("/v1/auth/email/verify", `{"challenge_id":"`+challenge.ChallengeID+`","code":"`+sender.code+`","username":"Ari","time_zone":"Mars/Base"}`)
	if invalidZone.Code != http.StatusBadRequest {
		t.Fatalf("new account with invalid time zone: %d %s", invalidZone.Code, invalidZone.Body.String())
	}
	verify := request("/v1/auth/email/verify", `{"challenge_id":"`+challenge.ChallengeID+`","code":"`+sender.code+`","username":"Ari","time_zone":"Asia/Kolkata"}`)
	var result struct {
		Token string `json:"token"`
	}
	if verify.Code != http.StatusOK || json.Unmarshal(verify.Body.Bytes(), &result) != nil || result.Token == "" {
		t.Fatalf("verify: %d %s", verify.Code, verify.Body.String())
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	r.Header.Set("Authorization", "Bearer "+result.Token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"username":"Ari"`) || !strings.Contains(w.Body.String(), `"time_zone":"Asia/Kolkata"`) {
		t.Fatalf("/me: %d %s", w.Code, w.Body.String())
	}
	if request("/v1/auth/email/verify", `{"challenge_id":"`+challenge.ChallengeID+`","code":"`+sender.code+`"}`).Code != http.StatusBadRequest {
		t.Fatal("code was accepted twice")
	}
}

type fakeTelegram struct{ id pgtype.UUID }

func (f fakeTelegram) Start(context.Context, string) (auth.TelegramStart, error) {
	return auth.TelegramStart{AttemptID: f.id, PollSecret: strings.Repeat("a", 43), AuthorizationURL: "https://oauth.telegram.org/auth"}, nil
}
func (fakeTelegram) Callback(_ context.Context, state, code string) error {
	if state != "valid" || code != "approved" {
		return auth.ErrInvalidChallenge
	}
	return nil
}
func (f fakeTelegram) Poll(_ context.Context, id pgtype.UUID, secret, _ string, profile *auth.NewAccountProfile) (auth.TelegramResult, error) {
	if id != f.id || secret != strings.Repeat("a", 43) {
		return auth.TelegramResult{}, auth.ErrInvalidChallenge
	}
	if profile == nil {
		return auth.TelegramResult{Status: "pending"}, nil
	}
	return auth.TelegramResult{Status: "signed_in", Token: "test-session"}, nil
}
func (fakeTelegram) Restore(context.Context, string, string) (string, error) {
	return "restored-session", nil
}

func TestTelegramHTTP(t *testing.T) {
	var id pgtype.UUID
	if err := id.Scan("94bc4842-25b9-4c54-8c9d-47a9050e620c"); err != nil {
		t.Fatal(err)
	}
	handler := newHandler(fakeDatabase{}, nil, nil, fakeTelegram{id: id}, nil)
	request := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	started := request(http.MethodPost, "/v1/auth/telegram/start", "")
	if started.Code != http.StatusCreated || !strings.Contains(started.Body.String(), id.String()) || !strings.Contains(started.Body.String(), "authorization_url") {
		t.Fatalf("start = %d %s", started.Code, started.Body.String())
	}
	if got := request(http.MethodGet, "/v1/auth/telegram/callback?state=invalid&code=approved", "").Code; got != http.StatusBadRequest {
		t.Fatalf("invalid callback = %d", got)
	}
	callback := request(http.MethodGet, "/v1/auth/telegram/callback?state=valid&code=approved", "")
	if callback.Code != http.StatusSeeOther || callback.Header().Get("Location") != "/v1/auth/telegram/complete" {
		t.Fatalf("callback = %d %s", callback.Code, callback.Body.String())
	}
	complete := request(http.MethodGet, callback.Header().Get("Location"), "")
	if complete.Code != http.StatusOK || strings.Contains(complete.Body.String(), "test-session") {
		t.Fatalf("completion page = %d %s", complete.Code, complete.Body.String())
	}
	body := `{"attempt_id":"` + id.String() + `","poll_secret":"` + strings.Repeat("a", 43) + `"}`
	pending := request(http.MethodPost, "/v1/auth/telegram/poll", body)
	if pending.Code != http.StatusAccepted || !strings.Contains(pending.Body.String(), `"pending"`) {
		t.Fatalf("poll = %d %s", pending.Code, pending.Body.String())
	}
	approved := request(http.MethodPost, "/v1/auth/telegram/poll", `{"attempt_id":"`+id.String()+`","poll_secret":"`+strings.Repeat("a", 43)+`","username":"Ari","time_zone":"UTC"}`)
	if approved.Code != http.StatusOK || !strings.Contains(approved.Body.String(), `"test-session"`) {
		t.Fatalf("approved = %d %s", approved.Code, approved.Body.String())
	}
	if got := request(http.MethodPost, "/v1/auth/telegram/restore", `{"recovery_ticket":"test"}`).Code; got != http.StatusOK {
		t.Fatalf("restore = %d", got)
	}
	missing := newHandler(fakeDatabase{}, nil, nil, nil, nil)
	w := httptest.NewRecorder()
	missing.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/auth/telegram/start", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured Telegram = %d", w.Code)
	}
}
