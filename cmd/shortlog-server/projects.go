package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"shortlog-server/internal/apierror"
	"shortlog-server/internal/auth"
	"shortlog-server/internal/db"
	"shortlog-server/internal/projects"
)

type projectManager interface {
	Create(context.Context, pgtype.UUID, projects.CreateInput) (db.Project, error)
	List(context.Context, pgtype.UUID, bool) ([]db.Project, error)
	Get(context.Context, pgtype.UUID, pgtype.UUID) (db.Project, error)
	Patch(context.Context, pgtype.UUID, pgtype.UUID, projects.PatchInput) (db.Project, error)
	Archive(context.Context, pgtype.UUID, pgtype.UUID) error
	Unarchive(context.Context, pgtype.UUID, pgtype.UUID) error
}

type projectJSON struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Description *string    `json:"description"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	ArchivedAt  *time.Time `json:"archived_at"`
}

func projectResponse(project db.Project) projectJSON {
	result := projectJSON{ID: project.ID.String(), Name: project.Name,
		CreatedAt: project.CreatedAt.Time, UpdatedAt: project.UpdatedAt.Time}
	if project.Description.Valid {
		result.Description = &project.Description.String
	}
	if project.ArchivedAt.Valid {
		result.ArchivedAt = &project.ArchivedAt.Time
	}
	return result
}

func projectID(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	var id pgtype.UUID
	if err := id.Scan(r.PathValue("id")); err != nil || !id.Valid {
		apierror.Write(w, apierror.NotFound)
		return pgtype.UUID{}, false
	}
	return id, true
}

func projectError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, projects.ErrInvalid):
		apierror.Write(w, apierror.InvalidRequest)
	case errors.Is(err, projects.ErrNotFound):
		apierror.Write(w, apierror.NotFound)
	case errors.Is(err, projects.ErrArchived):
		apierror.Write(w, apierror.Conflict)
	default:
		serverError(w, r, err)
	}
}

func decodeProjectPatch(w http.ResponseWriter, r *http.Request) (projects.PatchInput, bool) {
	var fields map[string]json.RawMessage
	if !decodeInputLimit(w, r, &fields, 32<<10) {
		return projects.PatchInput{}, false
	}
	if len(fields) == 0 {
		apierror.Write(w, apierror.InvalidRequest)
		return projects.PatchInput{}, false
	}
	var input projects.PatchInput
	for field, raw := range fields {
		switch field {
		case "name":
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				apierror.Write(w, apierror.InvalidRequest)
				return projects.PatchInput{}, false
			}
			var name string
			if err := json.Unmarshal(raw, &name); err != nil {
				apierror.Write(w, apierror.InvalidRequest)
				return projects.PatchInput{}, false
			}
			input.Name = &name
		case "description":
			input.DescriptionSet = true
			if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				var description string
				if err := json.Unmarshal(raw, &description); err != nil {
					apierror.Write(w, apierror.InvalidRequest)
					return projects.PatchInput{}, false
				}
				input.Description = &description
			}
		default:
			apierror.Write(w, apierror.InvalidRequest)
			return projects.PatchInput{}, false
		}
	}
	return input, true
}

func registerProjectRoutes(mux *http.ServeMux, sessions sessionManager, store projectManager) {
	mux.HandleFunc("/v1/projects", onlyMethods(withSession(sessions, func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
		if r.Method == http.MethodGet {
			query := r.URL.Query()
			if len(query) > 0 && (len(query) != 1 || len(query["status"]) != 1) {
				apierror.Write(w, apierror.InvalidRequest)
				return
			}
			status := query.Get("status")
			if (len(query) != 0 && status == "") || (status != "" && status != "active" && status != "archived") {
				apierror.Write(w, apierror.InvalidRequest)
				return
			}
			rows, err := store.List(r.Context(), p.AccountID, status == "archived")
			if err != nil {
				projectError(w, r, err)
				return
			}
			items := make([]projectJSON, 0, len(rows))
			for _, row := range rows {
				items = append(items, projectResponse(row))
			}
			writeJSON(w, http.StatusOK, items)
			return
		}
		var input struct {
			Name        string  `json:"name"`
			Description *string `json:"description"`
		}
		if !decodeInputLimit(w, r, &input, 32<<10) {
			return
		}
		project, err := store.Create(r.Context(), p.AccountID, projects.CreateInput{Name: input.Name, Description: input.Description})
		if err != nil {
			projectError(w, r, err)
			return
		}
		w.Header().Set("Location", "/v1/projects/"+project.ID.String())
		writeJSON(w, http.StatusCreated, projectResponse(project))
	}), http.MethodGet, http.MethodPost))

	mux.HandleFunc("/v1/projects/{id}", onlyMethods(withSession(sessions, func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
		id, ok := projectID(w, r)
		if !ok {
			return
		}
		if r.Method == http.MethodGet {
			project, err := store.Get(r.Context(), p.AccountID, id)
			if err != nil {
				projectError(w, r, err)
				return
			}
			writeJSON(w, http.StatusOK, projectResponse(project))
			return
		}
		input, ok := decodeProjectPatch(w, r)
		if !ok {
			return
		}
		project, err := store.Patch(r.Context(), p.AccountID, id, input)
		if err != nil {
			projectError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, projectResponse(project))
	}), http.MethodGet, http.MethodPatch))

	mux.HandleFunc("/v1/projects/{id}/archive", onlyMethod(http.MethodPost, withSession(sessions, func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
		id, ok := projectID(w, r)
		if !ok {
			return
		}
		if err := store.Archive(r.Context(), p.AccountID, id); err != nil {
			projectError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})))
	mux.HandleFunc("/v1/projects/{id}/unarchive", onlyMethod(http.MethodPost, withSession(sessions, func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
		id, ok := projectID(w, r)
		if !ok {
			return
		}
		if err := store.Unarchive(r.Context(), p.AccountID, id); err != nil {
			projectError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})))
}
