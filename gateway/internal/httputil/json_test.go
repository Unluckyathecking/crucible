package httputil

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.WriteHeader(http.StatusCreated)
	WriteJSON(rec, map[string]any{"a": 1})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got, want := rec.Body.String(), "{\"a\":1}\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}
