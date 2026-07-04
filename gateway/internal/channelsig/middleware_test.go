package channelsig

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testHeader = "X-Test-Signature"

func newNextHandler(t *testing.T, called *bool) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*called = true
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("next handler: reading body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
}

func TestMiddleware_NoSecretPassesThrough(t *testing.T) {
	called := false
	handler := Middleware(nil, testHeader, 5*time.Minute)(newNextHandler(t, &called))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("body"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Fatal("next handler was not called when secret is empty")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestMiddleware_ValidSignaturePassesThrough(t *testing.T) {
	secret := []byte("mw-secret")
	body := []byte(`{"hello":"world"}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	header := Header(ts, Sign(secret, ts, body))

	called := false
	handler := Middleware(secret, testHeader, 5*time.Minute)(newNextHandler(t, &called))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	req.Header.Set(testHeader, header)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Fatal("next handler was not called for a valid signature")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != string(body) {
		t.Fatalf("next handler did not receive the original body: got %q", rec.Body.String())
	}
}

func TestMiddleware_MissingSignatureRejected(t *testing.T) {
	called := false
	handler := Middleware([]byte("mw-secret"), testHeader, 5*time.Minute)(newNextHandler(t, &called))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("body"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if called {
		t.Fatal("next handler was called despite a missing signature")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestMiddleware_TamperedBodyRejected(t *testing.T) {
	secret := []byte("mw-secret")
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	header := Header(ts, Sign(secret, ts, []byte("original")))

	called := false
	handler := Middleware(secret, testHeader, 5*time.Minute)(newNextHandler(t, &called))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("tampered"))
	req.Header.Set(testHeader, header)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if called {
		t.Fatal("next handler was called despite a tampered body")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestMiddleware_StaleTimestampRejected(t *testing.T) {
	secret := []byte("mw-secret")
	body := []byte("body")
	staleTs := strconv.FormatInt(time.Now().Add(-1*time.Hour).Unix(), 10)
	header := Header(staleTs, Sign(secret, staleTs, body))

	called := false
	handler := Middleware(secret, testHeader, 5*time.Minute)(newNextHandler(t, &called))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	req.Header.Set(testHeader, header)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if called {
		t.Fatal("next handler was called despite a stale timestamp")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestMiddleware_WrongSecretRejected(t *testing.T) {
	body := []byte("body")
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	header := Header(ts, Sign([]byte("right-secret"), ts, body))

	called := false
	handler := Middleware([]byte("wrong-secret"), testHeader, 5*time.Minute)(newNextHandler(t, &called))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	req.Header.Set(testHeader, header)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if called {
		t.Fatal("next handler was called despite a signature from the wrong secret")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
