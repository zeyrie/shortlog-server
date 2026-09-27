// Package apierror defines stable, client-facing errors for the HTTP API.
package apierror

import (
	"encoding/json"
	"net/http"
)

type Code string

const (
	InvalidRequest     Code = "invalid_request"
	ProfileRequired    Code = "profile_required"
	Unauthorized       Code = "unauthorized"
	Forbidden          Code = "forbidden"
	NotFound           Code = "not_found"
	MethodNotAllowed   Code = "method_not_allowed"
	Conflict           Code = "conflict"
	RateLimited        Code = "rate_limited"
	ServiceUnavailable Code = "service_unavailable"
	Internal           Code = "internal_error"
)

type definition struct {
	status  int
	message string
}

type detail struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
}

type response struct {
	Error detail `json:"error"`
}

func (c Code) definition() definition {
	switch c {
	case InvalidRequest:
		return definition{http.StatusBadRequest, "Invalid request."}
	case ProfileRequired:
		return definition{http.StatusUnprocessableEntity, "Username and time zone are required to create an account."}
	case Unauthorized:
		return definition{http.StatusUnauthorized, "Authentication required."}
	case Forbidden:
		return definition{http.StatusForbidden, "Permission denied."}
	case NotFound:
		return definition{http.StatusNotFound, "Resource not found."}
	case MethodNotAllowed:
		return definition{http.StatusMethodNotAllowed, "Method not allowed."}
	case Conflict:
		return definition{http.StatusConflict, "Request conflicts with the current state."}
	case RateLimited:
		return definition{http.StatusTooManyRequests, "Too many requests. Try again later."}
	case ServiceUnavailable:
		return definition{http.StatusServiceUnavailable, "Service temporarily unavailable."}
	default:
		return definition{http.StatusInternalServerError, "An unexpected error occurred."}
	}
}

// Write sends a stable error code and safe message; internal error details
// should be logged separately and never returned to the client.
func Write(w http.ResponseWriter, code Code) {
	switch code {
	case InvalidRequest, ProfileRequired, Unauthorized, Forbidden, NotFound, MethodNotAllowed,
		Conflict, RateLimited, ServiceUnavailable:
	default:
		code = Internal
	}

	if code == Unauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="shortlog"`)
	}

	def := code.definition()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(def.status)

	_ = json.NewEncoder(w).Encode(response{Error: detail{Code: code, Message: def.message}})
}
