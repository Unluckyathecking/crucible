package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Unluckyathecking/crucible/gateway/internal/proxy"
)

// TestInvokeErrorCacheControlNoStore verifies that error responses from the
// invoke handler carry Cache-Control: no-store, matching the success path.
// apierror.Write sets this header on every error response it emits.
func TestInvokeErrorCacheControlNoStore(t *testing.T) {
	worker := successWorker(1, "op")
	defer worker.Close()

	p := proxy.New(worker.URL, 5*time.Second, 0)
	h := invoke(p, nil, "sanitized", "echo")

	// Malformed JSON body triggers a 400 BAD_REQUEST via apierror.Write.
	req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(`not-json`))
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", w.Code, w.Body.String())
	}
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
