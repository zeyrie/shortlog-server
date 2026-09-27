package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"shortlog-server/internal/apierror"
	"shortlog-server/internal/auth"
	"shortlog-server/internal/db"
	"shortlog-server/internal/notes"
)

type noteManager interface {
	Create(context.Context, pgtype.UUID, string, pgtype.UUID) (db.Note, error)
	List(context.Context, pgtype.UUID, pgtype.UUID, int, string) (notes.Page, error)
	Get(context.Context, pgtype.UUID, pgtype.UUID) (db.Note, error)
	Patch(context.Context, pgtype.UUID, pgtype.UUID, notes.PatchInput) (db.Note, error)
	Delete(context.Context, pgtype.UUID, pgtype.UUID) error
}

type noteJSON struct {
	ID        string    `json:"id"`
	Content   string    `json:"content"`
	ProjectID *string   `json:"project_id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func noteResponse(note db.Note) noteJSON {
	result := noteJSON{ID: note.ID.String(), Content: note.Content, CreatedAt: note.CreatedAt.Time, UpdatedAt: note.UpdatedAt.Time}
	if note.ProjectID.Valid {
		id := note.ProjectID.String()
		result.ProjectID = &id
	}
	return result
}

func noteError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, notes.ErrInvalid):
		apierror.Write(w, apierror.InvalidRequest)
	case errors.Is(err, notes.ErrNotFound):
		apierror.Write(w, apierror.NotFound)
	case errors.Is(err, notes.ErrArchived):
		apierror.Write(w, apierror.Conflict)
	default:
		serverError(w, r, err)
	}
}

func noteID(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	var id pgtype.UUID
	if err := id.Scan(r.PathValue("id")); err != nil || !id.Valid {
		apierror.Write(w, apierror.NotFound)
		return pgtype.UUID{}, false
	}
	return id, true
}

func decodeNoteFields(w http.ResponseWriter, r *http.Request, create bool) (notes.PatchInput, bool) {
	var fields map[string]json.RawMessage
	if !decodeInputLimit(w, r, &fields, 128<<10) {
		return notes.PatchInput{}, false
	}
	if len(fields) == 0 {
		apierror.Write(w, apierror.InvalidRequest)
		return notes.PatchInput{}, false
	}
	var input notes.PatchInput
	for field, raw := range fields {
		switch field {
		case "content":
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				apierror.Write(w, apierror.InvalidRequest)
				return notes.PatchInput{}, false
			}
			var content string
			if err := json.Unmarshal(raw, &content); err != nil {
				apierror.Write(w, apierror.InvalidRequest)
				return notes.PatchInput{}, false
			}
			input.Content = &content
		case "project_id":
			input.ProjectSet = true
			if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				var id string
				if err := json.Unmarshal(raw, &id); err != nil || input.ProjectID.Scan(id) != nil || !input.ProjectID.Valid {
					apierror.Write(w, apierror.InvalidRequest)
					return notes.PatchInput{}, false
				}
			}
		default:
			apierror.Write(w, apierror.InvalidRequest)
			return notes.PatchInput{}, false
		}
	}
	if (create && input.Content == nil) || (!create && input.Content == nil && !input.ProjectSet) {
		apierror.Write(w, apierror.InvalidRequest)
		return notes.PatchInput{}, false
	}
	return input, true
}

func listNotes(w http.ResponseWriter, r *http.Request, p auth.Principal, store noteManager, projectID pgtype.UUID) {
	query := r.URL.Query()
	for key, values := range query {
		if (key != "limit" && key != "cursor") || len(values) != 1 || values[0] == "" {
			apierror.Write(w, apierror.InvalidRequest)
			return
		}
	}
	limit := 50
	if _, present := query["limit"]; present {
		var err error
		limit, err = strconv.Atoi(query.Get("limit"))
		if err != nil || limit < 1 || limit > 100 {
			apierror.Write(w, apierror.InvalidRequest)
			return
		}
	}
	page, err := store.List(r.Context(), p.AccountID, projectID, limit, query.Get("cursor"))
	if err != nil {
		noteError(w, r, err)
		return
	}
	items := make([]noteJSON, 0, len(page.Items))
	for _, note := range page.Items {
		items = append(items, noteResponse(note))
	}
	var next *string
	if page.NextCursor != "" {
		next = &page.NextCursor
	}
	writeJSON(w, http.StatusOK, struct {
		Items      []noteJSON `json:"items"`
		NextCursor *string    `json:"next_cursor"`
	}{items, next})
}

func registerNoteRoutes(mux *http.ServeMux, sessions sessionManager, store noteManager) {
	mux.HandleFunc("/v1/notes", onlyMethods(withSession(sessions, func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
		if r.Method == http.MethodGet {
			listNotes(w, r, p, store, pgtype.UUID{})
			return
		}
		input, ok := decodeNoteFields(w, r, true)
		if !ok {
			return
		}
		note, err := store.Create(r.Context(), p.AccountID, *input.Content, input.ProjectID)
		if err != nil {
			noteError(w, r, err)
			return
		}
		w.Header().Set("Location", "/v1/notes/"+note.ID.String())
		writeJSON(w, http.StatusCreated, noteResponse(note))
	}), http.MethodGet, http.MethodPost))

	mux.HandleFunc("/v1/projects/{id}/notes", onlyMethod(http.MethodGet, withSession(sessions, func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
		id, ok := projectID(w, r)
		if !ok {
			return
		}
		listNotes(w, r, p, store, id)
	})))

	mux.HandleFunc("/v1/notes/{id}", onlyMethods(withSession(sessions, func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
		id, ok := noteID(w, r)
		if !ok {
			return
		}
		switch r.Method {
		case http.MethodGet:
			note, err := store.Get(r.Context(), p.AccountID, id)
			if err != nil {
				noteError(w, r, err)
				return
			}
			writeJSON(w, http.StatusOK, noteResponse(note))
		case http.MethodPatch:
			input, ok := decodeNoteFields(w, r, false)
			if !ok {
				return
			}
			note, err := store.Patch(r.Context(), p.AccountID, id, input)
			if err != nil {
				noteError(w, r, err)
				return
			}
			writeJSON(w, http.StatusOK, noteResponse(note))
		case http.MethodDelete:
			if err := store.Delete(r.Context(), p.AccountID, id); err != nil {
				noteError(w, r, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}
	}), http.MethodGet, http.MethodPatch, http.MethodDelete))
}
