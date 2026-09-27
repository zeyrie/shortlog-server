package apierror

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWrite(t *testing.T) {
	for _, tc := range []struct {
		code Code
		want int
	}{
		{InvalidRequest, http.StatusBadRequest},
		{ProfileRequired, http.StatusUnprocessableEntity},
		{Unauthorized, http.StatusUnauthorized},
		{Forbidden, http.StatusForbidden},
		{NotFound, http.StatusNotFound},
		{MethodNotAllowed, http.StatusMethodNotAllowed},
		{Conflict, http.StatusConflict},
		{RateLimited, http.StatusTooManyRequests},
		{ServiceUnavailable, http.StatusServiceUnavailable},
		{Internal, http.StatusInternalServerError},
	} {
		t.Run(string(tc.code), func(t *testing.T) {
			w := httptest.NewRecorder()
			Write(w, tc.code)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
			if got := w.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("content type = %q", got)
			}
			if got := w.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("cache control = %q", got)
			}
			var body response
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != tc.code || body.Error.Message == "" {
				t.Fatalf("body = %+v", body)
			}
		})
	}
}
