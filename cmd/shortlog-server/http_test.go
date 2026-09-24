package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeDatabase struct{ err error }

func (f fakeDatabase) Ping(context.Context) error { return f.err }

func TestHealth(t *testing.T) {
	response := httptest.NewRecorder()
	newHandler(fakeDatabase{err: errors.New("offline")}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("health status = %d, want %d", response.Code, http.StatusOK)
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
			newHandler(fakeDatabase{err: test.err}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if response.Code != test.want {
				t.Fatalf("ready status = %d, want %d", response.Code, test.want)
			}
		})
	}
}
