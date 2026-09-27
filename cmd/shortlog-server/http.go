package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
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
	UpdateProfile(context.Context, pgtype.UUID, auth.NewAccountProfile) (db.Account, error)
}

type emailLogin interface {
	Start(context.Context, string, string) (pgtype.UUID, error)
	Verify(context.Context, pgtype.UUID, string, string, *auth.NewAccountProfile) (auth.EmailResult, error)
	Restore(context.Context, string, string) (string, error)
}

type telegramLogin interface {
	Start(context.Context, string) (auth.TelegramStart, error)
	Callback(context.Context, string, string) error
	Poll(context.Context, pgtype.UUID, string, string, *auth.NewAccountProfile) (auth.TelegramResult, error)
	Restore(context.Context, string, string) (string, error)
}

func newHandler(db databasePinger, sessions sessionManager, email emailLogin, telegram telegramLogin, projects projectManager, noteStore noteManager) http.Handler {
	mux := http.NewServeMux()
	if projects != nil {
		registerProjectRoutes(mux, sessions, projects)
	}
	if noteStore != nil {
		registerNoteRoutes(mux, sessions, noteStore)
	}

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

	mux.HandleFunc("/v1/auth/email/start", onlyMethod(http.MethodPost, func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		r = r.WithContext(ctx)
		var input struct {
			Email string `json:"email"`
		}
		if !decodeInput(w, r, &input) {
			return
		}

		ip, _, _ := net.SplitHostPort(r.RemoteAddr)

		id, err := email.Start(ctx, input.Email, ip)
		if errors.Is(err, auth.ErrLoginRateLimit) {
			apierror.Write(w, apierror.RateLimited)
			return
		}

		if errors.Is(err, auth.ErrEmailUnavailable) {
			apierror.Write(w, apierror.ServiceUnavailable)
			return
		}

		if errors.Is(err, auth.ErrInvalidChallenge) {
			apierror.Write(w, apierror.InvalidRequest)
			return
		}

		if err != nil {
			serverError(w, r, err)
			return
		}

		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusAccepted, struct {
			ChallengeID string `json:"challenge_id"`
		}{id.String()})
	}))

	mux.HandleFunc("/v1/auth/email/verify", onlyMethod(http.MethodPost, func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		r = r.WithContext(ctx)

		var input struct {
			ChallengeID string `json:"challenge_id"`
			Code        string `json:"code"`
			Username    string `json:"username"`
			TimeZone    string `json:"time_zone"`
		}
		if !decodeInput(w, r, &input) {
			return
		}

		var id pgtype.UUID

		if err := id.Scan(input.ChallengeID); err != nil || !id.Valid {
			apierror.Write(w, apierror.InvalidRequest)
			return
		}

		var profile *auth.NewAccountProfile
		if input.Username != "" || input.TimeZone != "" {
			profile = &auth.NewAccountProfile{Username: input.Username, TimeZone: input.TimeZone}
		}
		result, err := email.Verify(ctx, id, input.Code, r.UserAgent(), profile)
		if errors.Is(err, auth.ErrProfileRequired) {
			apierror.Write(w, apierror.ProfileRequired)
			return
		}
		if errors.Is(err, auth.ErrInvalidProfile) {
			apierror.Write(w, apierror.InvalidRequest)
			return
		}
		if errors.Is(err, auth.ErrInvalidChallenge) {
			apierror.Write(w, apierror.InvalidRequest)
			return
		}

		if err != nil {
			serverError(w, r, err)
			return
		}

		w.Header().Set("Cache-Control", "no-store")

		if result.RecoveryTicket != "" {
			writeJSON(w, http.StatusOK, struct {
				Status         string `json:"status"`
				RecoveryTicket string `json:"recovery_ticket"`
			}{"restore_required", result.RecoveryTicket})
			return
		}

		writeJSON(w, http.StatusOK, struct {
			Status string `json:"status"`
			Token  string `json:"token"`
		}{"signed_in", result.Token})
	}))

	mux.HandleFunc("/v1/auth/email/restore", onlyMethod(http.MethodPost, func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		r = r.WithContext(ctx)
		var input struct {
			RecoveryTicket string `json:"recovery_ticket"`
		}
		if !decodeInput(w, r, &input) {
			return
		}

		token, err := email.Restore(ctx, input.RecoveryTicket, r.UserAgent())
		if errors.Is(err, auth.ErrInvalidChallenge) {
			apierror.Write(w, apierror.InvalidRequest)
			return
		}
		if err != nil {
			serverError(w, r, err)
			return
		}

		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, struct {
			Token string `json:"token"`
		}{token})
	}))

	mux.HandleFunc("/v1/auth/telegram/start", onlyMethod(http.MethodPost, func(w http.ResponseWriter, r *http.Request) {
		if telegram == nil {
			apierror.Write(w, apierror.ServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		result, err := telegram.Start(ctx, ip)
		if errors.Is(err, auth.ErrLoginRateLimit) {
			apierror.Write(w, apierror.RateLimited)
			return
		}
		if err != nil {
			serverError(w, r, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusCreated, struct {
			AttemptID        string `json:"attempt_id"`
			PollSecret       string `json:"poll_secret"`
			AuthorizationURL string `json:"authorization_url"`
		}{result.AttemptID.String(), result.PollSecret, result.AuthorizationURL})
	}))
	mux.HandleFunc("/v1/auth/telegram/callback", onlyMethod(http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if telegram == nil {
			apierror.Write(w, apierror.ServiceUnavailable)
			return
		}
		query := r.URL.Query()
		if len(query["state"]) != 1 || len(query["code"]) != 1 {
			apierror.Write(w, apierror.InvalidRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		if err := telegram.Callback(ctx, query.Get("state"), query.Get("code")); err != nil {
			if errors.Is(err, auth.ErrInvalidChallenge) {
				apierror.Write(w, apierror.InvalidRequest)
				return
			}
			serverError(w, r, err)
			return
		}
		http.Redirect(w, r, "/v1/auth/telegram/complete", http.StatusSeeOther)
	}))
	mux.HandleFunc("/v1/auth/telegram/complete", onlyMethod(http.MethodGet, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'")
		_, _ = io.WriteString(w, "<!doctype html><title>Shortlog</title><p>Telegram approved. Return to Shortlog to finish signing in.</p>")
	}))
	mux.HandleFunc("/v1/auth/telegram/poll", onlyMethod(http.MethodPost, func(w http.ResponseWriter, r *http.Request) {
		if telegram == nil {
			apierror.Write(w, apierror.ServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		var input struct {
			AttemptID  string `json:"attempt_id"`
			PollSecret string `json:"poll_secret"`
			Username   string `json:"username"`
			TimeZone   string `json:"time_zone"`
		}
		if !decodeInput(w, r, &input) {
			return
		}
		var id pgtype.UUID
		if err := id.Scan(input.AttemptID); err != nil || !id.Valid {
			apierror.Write(w, apierror.InvalidRequest)
			return
		}
		var profile *auth.NewAccountProfile
		if input.Username != "" || input.TimeZone != "" {
			profile = &auth.NewAccountProfile{Username: input.Username, TimeZone: input.TimeZone}
		}
		result, err := telegram.Poll(ctx, id, input.PollSecret, r.UserAgent(), profile)
		if errors.Is(err, auth.ErrInvalidChallenge) {
			apierror.Write(w, apierror.InvalidRequest)
			return
		}
		if errors.Is(err, auth.ErrProfileRequired) {
			apierror.Write(w, apierror.ProfileRequired)
			return
		}
		if errors.Is(err, auth.ErrInvalidProfile) {
			apierror.Write(w, apierror.InvalidRequest)
			return
		}
		if err != nil {
			serverError(w, r, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if result.Status == "pending" {
			writeJSON(w, http.StatusAccepted, struct {
				Status string `json:"status"`
			}{"pending"})
			return
		}
		if result.Status == "restore_required" {
			writeJSON(w, http.StatusOK, struct {
				Status         string `json:"status"`
				RecoveryTicket string `json:"recovery_ticket"`
			}{result.Status, result.RecoveryTicket})
			return
		}
		writeJSON(w, http.StatusOK, struct {
			Status string `json:"status"`
			Token  string `json:"token"`
		}{result.Status, result.Token})
	}))
	mux.HandleFunc("/v1/auth/telegram/restore", onlyMethod(http.MethodPost, func(w http.ResponseWriter, r *http.Request) {
		if telegram == nil {
			apierror.Write(w, apierror.ServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		var input struct {
			RecoveryTicket string `json:"recovery_ticket"`
		}
		if !decodeInput(w, r, &input) {
			return
		}
		token, err := telegram.Restore(ctx, input.RecoveryTicket, r.UserAgent())
		if errors.Is(err, auth.ErrInvalidChallenge) {
			apierror.Write(w, apierror.InvalidRequest)
			return
		}
		if err != nil {
			serverError(w, r, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, struct {
			Token string `json:"token"`
		}{token})
	}))
	mux.HandleFunc("/v1/auth/logout", onlyMethod(http.MethodPost, withSession(sessions, func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
		if _, err := sessions.RevokeSession(r.Context(), p.AccountID, p.SessionID); err != nil {
			serverError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})))

	mux.HandleFunc("/v1/me", onlyMethods(withSession(sessions, func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
		if r.Method == http.MethodPatch {
			var input struct {
				Username string `json:"username"`
				TimeZone string `json:"time_zone"`
			}
			if !decodeInput(w, r, &input) {
				return
			}
			account, err := sessions.UpdateProfile(r.Context(), p.AccountID, auth.NewAccountProfile{
				Username: input.Username, TimeZone: input.TimeZone,
			})
			if errors.Is(err, auth.ErrInvalidProfile) || errors.Is(err, auth.ErrProfileRequired) {
				apierror.Write(w, apierror.InvalidRequest)
				return
			}
			if err != nil {
				serverError(w, r, err)
				return
			}
			writeMe(w, account.ID, account.Username, account.TimeZone, account.CreatedAt.Time)
			return
		}
		writeMe(w, p.AccountID, p.Username, p.TimeZone, p.AccountCreated.Time)
	}), http.MethodGet, http.MethodPatch))

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

func decodeInput(w http.ResponseWriter, r *http.Request, dst any) bool {
	return decodeInputLimit(w, r, dst, 4096)
}

func decodeInputLimit(w http.ResponseWriter, r *http.Request, dst any, limit int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(dst); err != nil {
		apierror.Write(w, apierror.InvalidRequest)
		return false
	}

	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		apierror.Write(w, apierror.InvalidRequest)
		return false
	}

	return true
}

func onlyMethod(method string, next http.HandlerFunc) http.HandlerFunc {
	return onlyMethods(next, method)
}

func onlyMethods(next http.HandlerFunc, methods ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for _, method := range methods {
			if r.Method == method {
				next(w, r)
				return
			}
		}
		w.Header().Set("Allow", strings.Join(methods, ", "))
		apierror.Write(w, apierror.MethodNotAllowed)
	}
}

func writeMe(w http.ResponseWriter, id pgtype.UUID, username string, timeZone string, createdAt time.Time) {
	writeJSON(w, http.StatusOK, struct {
		ID        string    `json:"id"`
		Username  string    `json:"username"`
		TimeZone  string    `json:"time_zone"`
		CreatedAt time.Time `json:"created_at"`
	}{id.String(), username, timeZone, createdAt})
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
