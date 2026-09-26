package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"shortlog-server/internal/apierror"
	"shortlog-server/internal/auth"
	"shortlog-server/internal/db"
)

type databasePinger interface {
	Ping(context.Context) error
}

type sessionManager interface {
	Authenticate(context.Context, string) (auth.Principal, error)
	ListSessions(context.Context, pgtype.UUID) ([]db.ListActiveSessionsRow, error)
	RevokeSession(context.Context, pgtype.UUID, pgtype.UUID) (bool, error)
	RevokeAllSessions(context.Context, pgtype.UUID) (int64, error)
}

func newHandler(db databasePinger, sessions sessionManager) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", onlyMethod(http.MethodGet, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	}))

	mux.HandleFunc("/readyz", onlyMethod(http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if err := db.Ping(ctx); err != nil {
			apierror.Write(w, apierror.ServiceUnavailable)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	}))

	mux.HandleFunc("/v1/me", onlyMethod(http.MethodGet, withSession(sessions, func(w http.ResponseWriter, _ *http.Request, p auth.Principal) {
		var username *string
		if p.Username.Valid {
			username = &p.Username.String
		}

		writeJSON(w, http.StatusOK, struct {
			ID        string    `json:"id"`
			Username  *string   `json:"username"`
			TimeZone  string    `json:"time_zone"`
			CreatedAt time.Time `json:"created_at"`
		}{p.AccountID.String(), username, p.TimeZone, p.AccountCreated.Time})
	})))

	mux.HandleFunc("/v1/sessions", onlyMethod(http.MethodGet, withSession(sessions, func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
		rows, err := sessions.ListSessions(r.Context(), p.AccountID)
		if err != nil {
			serverError(w, r, err)
			return
		}

		items := make([]struct {
			ID              string    `json:"id"`
			UserAgent       string    `json:"user_agent"`
			DeviceLabel     string    `json:"device_label"`
			CreatedAt       time.Time `json:"created_at"`
			LastUsedAt      time.Time `json:"last_used_at"`
			AuthenticatedAt time.Time `json:"authenticated_at"`
			Current         bool      `json:"current"`
		}, 0, len(rows))

		for _, row := range rows {
			items = append(items, struct {
				ID              string    `json:"id"`
				UserAgent       string    `json:"user_agent"`
				DeviceLabel     string    `json:"device_label"`
				CreatedAt       time.Time `json:"created_at"`
				LastUsedAt      time.Time `json:"last_used_at"`
				AuthenticatedAt time.Time `json:"authenticated_at"`
				Current         bool      `json:"current"`
			}{row.ID.String(), row.UserAgent, row.DeviceLabel,
				row.CreatedAt.Time, row.LastUsedAt.Time, row.AuthenticatedAt.Time,
				row.ID == p.SessionID})
		}

		writeJSON(w, http.StatusOK, items)
	})))

	mux.HandleFunc("/v1/sessions/{id}", onlyMethod(http.MethodDelete, withSession(sessions, func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
		var sessionID pgtype.UUID
		if err := sessionID.Scan(r.PathValue("id")); err != nil || !sessionID.Valid {
			apierror.Write(w, apierror.NotFound)
			return
		}

		revoked, err := sessions.RevokeSession(r.Context(), p.AccountID, sessionID)
		if err != nil {
			serverError(w, r, err)
			return
		}

		if !revoked {
			apierror.Write(w, apierror.NotFound)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	})))

	mux.HandleFunc("/v1/sessions/revoke-all", onlyMethod(http.MethodPost, withSession(sessions, func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
		if _, err := sessions.RevokeAllSessions(r.Context(), p.AccountID); err != nil {
			serverError(w, r, err)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	})))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		apierror.Write(w, apierror.NotFound)
	})

	return mux
}

func onlyMethod(method string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.Header().Set("Allow", method)
			apierror.Write(w, apierror.MethodNotAllowed)
			return
		}
		next(w, r)
	}
}

func withSession(sessions sessionManager, next func(http.ResponseWriter, *http.Request, auth.Principal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {

		w.Header().Set("Cache-Control", "no-store")

		if len(r.Header.Values("Authorization")) != 1 {
			apierror.Write(w, apierror.Unauthorized)
			return
		}

		scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || len(token) != 43 || strings.ContainsAny(token, " \t\r\n") {
			apierror.Write(w, apierror.Unauthorized)
			return
		}

		// Current protected routes are short requests. Future polling endpoints
		// must choose their own deadline rather than inherit this one.
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		r = r.WithContext(ctx)

		principal, err := sessions.Authenticate(r.Context(), token)
		if errors.Is(err, auth.ErrInvalidSession) {
			apierror.Write(w, apierror.Unauthorized)
			return
		}

		if err != nil {
			serverError(w, r, err)
			return
		}

		next(w, r, principal)
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")

	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Error("encode response", "error", err)
	}
}

func serverError(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("request failed", "error", err)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(r.Context().Err(), context.DeadlineExceeded) {
		apierror.Write(w, apierror.ServiceUnavailable)
		return
	}
	apierror.Write(w, apierror.Internal)
}
