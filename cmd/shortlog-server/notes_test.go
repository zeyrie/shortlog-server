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

	"github.com/jackc/pgx/v5/pgxpool"
	"shortlog-server/internal/auth"
	"shortlog-server/internal/db"
	"shortlog-server/internal/notes"
	"shortlog-server/internal/projects"
)

func TestNoteRoutesPostgres(t *testing.T) {
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
	newUser := func() string {
		t.Helper()
		account, err := query.CreateAccount(ctx, db.CreateAccountParams{Username: "Notes test", TimeZone: "UTC"})
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
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(raw)
		if _, err := query.CreateSession(ctx, db.CreateSessionParams{AccountID: account.ID, TokenHash: hash[:], UserAgent: "notes-test", DeviceLabel: "notes-test"}); err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	token, other := newUser(), newUser()
	handler := newHandler(pool, auth.New(pool), nil, nil, projects.New(pool), notes.New(pool))
	request := func(method, path, bearer, body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s: %d %s (want %d)", method, path, w.Code, w.Body.String(), status)
		}
		return w
	}
	var decodeNote = func(w *httptest.ResponseRecorder) noteJSON {
		t.Helper()
		var n noteJSON
		if err := json.Unmarshal(w.Body.Bytes(), &n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	var decodePage = func(w *httptest.ResponseRecorder) struct {
		Items      []noteJSON `json:"items"`
		NextCursor *string    `json:"next_cursor"`
	} {
		t.Helper()
		var p struct {
			Items      []noteJSON `json:"items"`
			NextCursor *string    `json:"next_cursor"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	request(http.MethodGet, "/v1/notes", "", "", http.StatusUnauthorized)
	for _, body := range []string{`{}`, `{"content":null}`, `{"content":"  \n "}`, `{"content":false}`, `{"content":"x","project_id":"no"}`, `{"content":"x","other":2}`} {
		request(http.MethodPost, "/v1/notes", token, body, http.StatusBadRequest)
	}
	for _, path := range []string{"/v1/notes?limit=0", "/v1/notes?limit=101", "/v1/notes?limit=no", "/v1/notes?limit=1&limit=2", "/v1/notes?cursor=", "/v1/notes?unknown=1"} {
		request(http.MethodGet, path, token, "", http.StatusBadRequest)
	}
	project := decodeProjectTest(t, request(http.MethodPost, "/v1/projects", token, `{"name":"Work"}`, http.StatusCreated))
	secondProject := decodeProjectTest(t, request(http.MethodPost, "/v1/projects", token, `{"name":"Second"}`, http.StatusCreated))
	otherProject := decodeProjectTest(t, request(http.MethodPost, "/v1/projects", other, `{"name":"Other"}`, http.StatusCreated))
	request(http.MethodPost, "/v1/notes", token, `{"content":"x","project_id":"`+otherProject+`"}`, http.StatusNotFound)
	request(http.MethodGet, "/v1/projects/"+otherProject+"/notes", token, "", http.StatusNotFound)
	request(http.MethodGet, "/v1/projects/"+project+"/notes", token, "", http.StatusOK)
	ids := make([]string, 0, 4)
	for _, content := range []string{"one", "two", "three", "four"} {
		w := request(http.MethodPost, "/v1/notes", token, `{"content":"`+content+`"}`, http.StatusCreated)
		n := decodeNote(w)
		if n.Content != content || n.ProjectID != nil || w.Header().Get("Location") != "/v1/notes/"+n.ID {
			t.Fatalf("created note: %s", w.Body.String())
		}
		ids = append(ids, n.ID)
	}
	// Ties at the database timestamp precision must still page via the UUID tie-breaker.
	if _, err := pool.Exec(ctx, "UPDATE notes SET created_at='2026-01-01T00:00:00Z' WHERE id=ANY($1::uuid[])", ids); err != nil {
		t.Fatal(err)
	}
	page1 := decodePage(request(http.MethodGet, "/v1/notes?limit=2", token, "", http.StatusOK))
	if len(page1.Items) != 2 || page1.NextCursor == nil {
		t.Fatalf("page 1: %+v", page1)
	}
	request(http.MethodGet, "/v1/notes?cursor=broken", token, "", http.StatusBadRequest)
	request(http.MethodGet, "/v1/projects/"+project+"/notes?cursor="+*page1.NextCursor, token, "", http.StatusBadRequest)
	request(http.MethodGet, "/v1/notes?cursor="+*page1.NextCursor, other, "", http.StatusBadRequest)
	// Deleting the anchor and inserting a newer note do not disturb the next page.
	request(http.MethodDelete, "/v1/notes/"+page1.Items[1].ID, token, "", http.StatusNoContent)
	request(http.MethodPost, "/v1/notes", token, `{"content":"new"}`, http.StatusCreated)
	page2 := decodePage(request(http.MethodGet, "/v1/notes?limit=2&cursor="+*page1.NextCursor, token, "", http.StatusOK))
	if len(page2.Items) != 2 || page2.NextCursor != nil {
		t.Fatalf("page 2: %+v", page2)
	}
	seen := map[string]bool{}
	for _, n := range append(page1.Items, page2.Items...) {
		if seen[n.ID] {
			t.Fatalf("repeated note %s", n.ID)
		}
		seen[n.ID] = true
	}
	projectNote := decodeNote(request(http.MethodPost, "/v1/notes", token, `{"content":"project note","project_id":"`+project+`"}`, http.StatusCreated))
	secondNote := decodeNote(request(http.MethodPost, "/v1/notes", token, `{"content":"another","project_id":"`+project+`"}`, http.StatusCreated))
	projectPage := decodePage(request(http.MethodGet, "/v1/projects/"+project+"/notes?limit=1", token, "", http.StatusOK))
	if len(projectPage.Items) != 1 || projectPage.NextCursor == nil {
		t.Fatalf("project page: %+v", projectPage)
	}
	request(http.MethodGet, "/v1/projects/"+secondProject+"/notes?cursor="+*projectPage.NextCursor, token, "", http.StatusBadRequest)
	lastProjectPage := decodePage(request(http.MethodGet, "/v1/projects/"+project+"/notes?limit=1&cursor="+*projectPage.NextCursor, token, "", http.StatusOK))
	if len(lastProjectPage.Items) != 1 || lastProjectPage.NextCursor != nil || lastProjectPage.Items[0].ID == projectPage.Items[0].ID {
		t.Fatalf("project last page: %+v", lastProjectPage)
	}
	request(http.MethodGet, "/v1/notes/"+projectNote.ID, other, "", http.StatusNotFound)
	request(http.MethodPatch, "/v1/notes/"+projectNote.ID, other, `{"content":"stolen"}`, http.StatusNotFound)
	request(http.MethodDelete, "/v1/notes/"+projectNote.ID, other, "", http.StatusNotFound)
	for _, body := range []string{`{}`, `{"content":null}`, `{"content":""}`, `{"project_id":"bad"}`, `{"nope":1}`} {
		request(http.MethodPatch, "/v1/notes/"+projectNote.ID, token, body, http.StatusBadRequest)
	}
	moved := decodeNote(request(http.MethodPatch, "/v1/notes/"+page2.Items[0].ID, token, `{"project_id":"`+project+`"}`, http.StatusOK))
	if moved.ProjectID == nil || *moved.ProjectID != project || moved.Content != page2.Items[0].Content {
		t.Fatalf("move: %+v", moved)
	}
	request(http.MethodGet, "/v1/projects/"+project+"/notes", token, "", http.StatusOK)
	request(http.MethodPost, "/v1/projects/"+project+"/archive", token, "", http.StatusNoContent)
	archivedPage := decodePage(request(http.MethodGet, "/v1/projects/"+project+"/notes", token, "", http.StatusOK))
	if len(archivedPage.Items) != 3 {
		t.Fatalf("archived list: %+v", archivedPage)
	}
	request(http.MethodGet, "/v1/notes/"+projectNote.ID, token, "", http.StatusOK)
	request(http.MethodPatch, "/v1/notes/"+projectNote.ID, token, `{"content":"edit"}`, http.StatusConflict)
	request(http.MethodPatch, "/v1/notes/"+projectNote.ID, token, `{"project_id":null}`, http.StatusConflict)
	request(http.MethodPatch, "/v1/notes/"+secondNote.ID, token, `{"project_id":"`+secondProject+`"}`, http.StatusConflict)
	request(http.MethodDelete, "/v1/notes/"+projectNote.ID, token, "", http.StatusConflict)
	request(http.MethodPatch, "/v1/notes/"+page2.Items[1].ID, token, `{"project_id":"`+project+`"}`, http.StatusConflict)
	request(http.MethodPost, "/v1/notes", token, `{"content":"no","project_id":"`+project+`"}`, http.StatusConflict)
	request(http.MethodPost, "/v1/projects/"+project+"/unarchive", token, "", http.StatusNoContent)
	updated := decodeNote(request(http.MethodPatch, "/v1/notes/"+projectNote.ID, token, `{"content":"new content","project_id":null}`, http.StatusOK))
	if updated.Content != "new content" || updated.ProjectID != nil || updated.CreatedAt != projectNote.CreatedAt || updated.UpdatedAt.Before(projectNote.UpdatedAt) {
		t.Fatalf("edit/move: %+v", updated)
	}
	request(http.MethodDelete, "/v1/notes/"+projectNote.ID, token, "", http.StatusNoContent)
	request(http.MethodDelete, "/v1/notes/"+projectNote.ID, token, "", http.StatusNotFound)
	request(http.MethodGet, "/v1/notes/"+projectNote.ID, token, "", http.StatusNotFound)
	request(http.MethodDelete, "/v1/notes/bad", token, "", http.StatusNotFound)
}

func decodeProjectTest(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var project struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &project); err != nil {
		t.Fatal(err)
	}
	return project.ID
}
