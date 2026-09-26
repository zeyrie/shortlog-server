package main

import (
	"context"
	"crypto/rand"
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

func TestAPIErrorResponses(t *testing.T) {
	handler := newHandler(fakeDatabase{err: errors.New("database secret")}, fakeSessions{err: errors.New("session secret")})
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
	handler := newHandler(fakeDatabase{}, fakeSessions{waitForCancel: true})
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
	newHandler(fakeDatabase{err: errors.New("offline")}, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
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
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	account, err := svc.ResolveVerifiedIdentity(ctx, "telegram", hex.EncodeToString(random))
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
	token, session, err := svc.IssueSession(ctx, account.ID, "Shortlog TUI/macOS")
	if err != nil {
		t.Fatal(err)
	}
	handler := newHandler(pool, svc)
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
	secondToken, _, err := svc.IssueSession(ctx, account.ID, "Shortlog TUI/Linux")
	if err != nil {
		t.Fatal(err)
	}
	if got := request(http.MethodPost, "/v1/sessions/revoke-all", secondToken).Code; got != http.StatusNoContent {
		t.Fatalf("revoke-all status = %d", got)
	}
	if got := request(http.MethodGet, "/v1/me", secondToken).Code; got != http.StatusUnauthorized {
		t.Fatalf("revoke-all /me status = %d", got)
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
			newHandler(fakeDatabase{err: test.err}, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if response.Code != test.want {
				t.Fatalf("ready status = %d, want %d", response.Code, test.want)
			}
		})
	}
}
