package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Unluckyathecking/crucible/gateway/internal/proxy"
)

func TestWriteJSONErrorCacheControlNoStore(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid input", false)

	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want \"no-store\"", got)
	}
}

func TestInvokeSuccessCacheControlNoStore(t *testing.T) {
	worker := successWorker(1, "op")
	defer worker.Close()

	p := proxy.New(worker.URL, 5*time.Second, 0)
	h := invoke(p, nil, "sanitized", "echo")

	req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(`{"x":1}`))
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want \"no-store\"", got)
	}
}
