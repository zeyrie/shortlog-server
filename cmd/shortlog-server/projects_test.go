package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"shortlog-server/internal/auth"
	"shortlog-server/internal/db"
	"shortlog-server/internal/projects"
)

func TestProjectRoutesPostgres(t *testing.T) {
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
	query := db.New(pool)
	newAccount := func() pgtype.UUID {
		t.Helper()
		account, err := query.CreateAccount(ctx, db.CreateAccountParams{Username: "Project API test", TimeZone: "UTC"})
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
		return account.ID
	}
	newToken := func(accountID pgtype.UUID) string {
		t.Helper()
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(raw)
		if _, err := query.CreateSession(ctx, db.CreateSessionParams{AccountID: accountID, TokenHash: hash[:], UserAgent: "project-test", DeviceLabel: "project-test"}); err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	owner := newAccount()
	other := newAccount()
	token, otherToken := newToken(owner), newToken(other)
	handler := newHandler(pool, auth.New(pool), nil, nil, projects.New(pool), nil)
	request := func(method, path, bearer, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	assert := func(w *httptest.ResponseRecorder, status int) {
		t.Helper()
		if w.Code != status {
			t.Fatalf("response = %d %s, want %d", w.Code, w.Body.String(), status)
		}
	}
	assert(request(http.MethodGet, "/v1/projects", "", ""), http.StatusUnauthorized)
	list := request(http.MethodGet, "/v1/projects", token, "")
	assert(list, http.StatusOK)
	if strings.TrimSpace(list.Body.String()) != "[]" {
		t.Fatalf("empty list = %s", list.Body.String())
	}
	assert(request(http.MethodGet, "/v1/projects?status=all", token, ""), http.StatusBadRequest)
	assert(request(http.MethodGet, "/v1/projects?unknown=1", token, ""), http.StatusBadRequest)
	assert(request(http.MethodPost, "/v1/projects", token, `{"name":"  "}`), http.StatusBadRequest)
	assert(request(http.MethodPost, "/v1/projects", token, `{"name":"Work","unknown":true}`), http.StatusBadRequest)
	created := request(http.MethodPost, "/v1/projects", token, `{"name":"  Work  ","description":"  Overview  "}`)
	assert(created, http.StatusCreated)
	var project struct {
		ID          string     `json:"id"`
		Name        string     `json:"name"`
		Description *string    `json:"description"`
		ArchivedAt  *time.Time `json:"archived_at"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &project); err != nil {
		t.Fatal(err)
	}
	if project.ID == "" || project.Name != "Work" || project.Description == nil || *project.Description != "Overview" || project.ArchivedAt != nil || created.Header().Get("Location") != "/v1/projects/"+project.ID {
		t.Fatalf("created = %s, Location = %q", created.Body.String(), created.Header().Get("Location"))
	}
	path := "/v1/projects/" + project.ID
	assert(request(http.MethodGet, path, otherToken, ""), http.StatusNotFound)
	assert(request(http.MethodPatch, path, otherToken, `{"name":"Stolen"}`), http.StatusNotFound)
	assert(request(http.MethodPost, path+"/archive", otherToken, ""), http.StatusNotFound)
	assert(request(http.MethodGet, "/v1/projects/invalid", token, ""), http.StatusNotFound)
	assert(request(http.MethodPatch, path, token, `{}`), http.StatusBadRequest)
	assert(request(http.MethodPatch, path, token, `{"name":null}`), http.StatusBadRequest)
	assert(request(http.MethodPatch, path, token, `{"description":17}`), http.StatusBadRequest)
	assert(request(http.MethodPatch, path, token, `{"unknown":true}`), http.StatusBadRequest)
	renamed := request(http.MethodPatch, path, token, `{"name":"Updated"}`)
	assert(renamed, http.StatusOK)
	if !strings.Contains(renamed.Body.String(), `"description":"Overview"`) {
		t.Fatalf("partial patch lost description: %s", renamed.Body.String())
	}
	assert(request(http.MethodPost, path+"/archive", token, ""), http.StatusNoContent)
	assert(request(http.MethodPost, path+"/archive", token, ""), http.StatusNoContent)
	assert(request(http.MethodPatch, path, token, `{"name":"Cannot edit"}`), http.StatusConflict)
	archived := request(http.MethodGet, "/v1/projects?status=archived", token, "")
	assert(archived, http.StatusOK)
	if !strings.Contains(archived.Body.String(), project.ID) {
		t.Fatalf("archived list = %s", archived.Body.String())
	}
	assert(request(http.MethodGet, path, token, ""), http.StatusOK)
	list = request(http.MethodGet, "/v1/projects", token, "")
	assert(list, http.StatusOK)
	if strings.TrimSpace(list.Body.String()) != "[]" {
		t.Fatalf("active list after archive = %s", list.Body.String())
	}
	assert(request(http.MethodPost, path+"/unarchive", token, ""), http.StatusNoContent)
	assert(request(http.MethodPost, path+"/unarchive", token, ""), http.StatusNoContent)
	cleared := request(http.MethodPatch, path, token, `{"description":null}`)
	assert(cleared, http.StatusOK)
	if !strings.Contains(cleared.Body.String(), `"description":null`) {
		t.Fatalf("cleared description = %s", cleared.Body.String())
	}
	assert(request(http.MethodDelete, path, token, ""), http.StatusMethodNotAllowed)
	unicodeDescription := strings.Repeat("界", 4000)
	unicodeBody, err := json.Marshal(map[string]string{"name": "Unicode", "description": unicodeDescription})
	if err != nil {
		t.Fatal(err)
	}
	assert(request(http.MethodPost, "/v1/projects", token, string(unicodeBody)), http.StatusCreated)
}
