package httputil

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStatusRecorderWriteHeader(t *testing.T) {
	tests := []struct {
		name string
		code int
	}{
		{"200 OK", http.StatusOK},
		{"201 Created", http.StatusCreated},
		{"400 Bad Request", http.StatusBadRequest},
		{"404 Not Found", http.StatusNotFound},
		{"500 Internal Server Error", http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sr := NewStatusRecorder(httptest.NewRecorder())
			sr.WriteHeader(tt.code)
			if sr.Status != tt.code {
				t.Errorf("Status = %d, want %d", sr.Status, tt.code)
			}
		})
	}
}

func TestStatusRecorderDefaultStatus(t *testing.T) {
	inner := httptest.NewRecorder()
	sr := NewStatusRecorder(inner)

	n, err := sr.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 5 {
		t.Errorf("Write returned %d bytes, want 5", n)
	}
	if sr.Status != http.StatusOK {
		t.Errorf("default Status = %d, want %d", sr.Status, http.StatusOK)
	}
	if inner.Body.String() != "hello" {
		t.Errorf("inner body = %q, want hello", inner.Body.String())
	}
}

func TestStatusRecorderWriteCapturesCode(t *testing.T) {
	inner := httptest.NewRecorder()
	sr := NewStatusRecorder(inner)

	sr.WriteHeader(http.StatusTeapot)
	n, err := sr.Write([]byte("tea"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 3 {
		t.Errorf("Write returned %d bytes, want 3", n)
	}

	if sr.Status != http.StatusTeapot {
		t.Errorf("Status = %d, want %d", sr.Status, http.StatusTeapot)
	}
	if inner.Code != http.StatusTeapot {
		t.Errorf("inner Code = %d, want %d", inner.Code, http.StatusTeapot)
	}
	if inner.Body.String() != "tea" {
		t.Errorf("inner body = %q, want tea", inner.Body.String())
	}
}

func TestStatusRecorderMultipleWriteHeader(t *testing.T) {
	inner := httptest.NewRecorder()
	sr := NewStatusRecorder(inner)

	sr.WriteHeader(http.StatusBadRequest)
	sr.WriteHeader(http.StatusInternalServerError) // Should be ignored

	if sr.Status != http.StatusBadRequest {
		t.Errorf("Status = %d, want %d", sr.Status, http.StatusBadRequest)
	}
	if inner.Code != http.StatusBadRequest {
		t.Errorf("inner Code = %d, want %d", inner.Code, http.StatusBadRequest)
	}
}

func TestStatusRecorder1xxThenFinal(t *testing.T) {
	inner := httptest.NewRecorder()
	sr := NewStatusRecorder(inner)

	sr.WriteHeader(http.StatusContinue) // 100 — informational, does not commit wroteHeader
	if sr.wroteHeader {
		t.Error("wroteHeader should be false after 1xx")
	}
	if sr.Status != http.StatusContinue {
		t.Errorf("Status = %d, want %d", sr.Status, http.StatusContinue)
	}
	sr.WriteHeader(http.StatusOK) // 200 finalizes

	if sr.Status != http.StatusOK {
		t.Errorf("Status = %d, want %d", sr.Status, http.StatusOK)
	}
	if !sr.wroteHeader {
		t.Error("wroteHeader should be true after 2xx")
	}
	// httptest.ResponseRecorder commits Code on the first WriteHeader call regardless
	// of informational status; the subsequent WriteHeader(200) is ignored by the inner
	// recorder. The authoritative final-status field is sr.Status, which middleware
	// logging and metrics rely on.
}

// flushRecorder wraps httptest.ResponseRecorder and implements http.Flusher so
// tests can verify that StatusRecorder.Flush() delegates to the underlying writer.
type flushRecorder struct {
	*httptest.ResponseRecorder
	flushed bool
}

func (f *flushRecorder) Flush() { f.flushed = true }

// TestStatusRecorder1xxThenWrite verifies that a 1xx informational status does not
// commit wroteHeader, so a subsequent Write still records Status=200 correctly.
func TestStatusRecorder1xxThenWrite(t *testing.T) {
	inner := httptest.NewRecorder()
	sr := NewStatusRecorder(inner)

	sr.WriteHeader(http.StatusContinue) // 100 — informational, must not commit wroteHeader
	if sr.wroteHeader {
		t.Fatal("wroteHeader must be false after 1xx WriteHeader")
	}
	if sr.Status != http.StatusContinue {
		t.Errorf("Status = %d, want %d", sr.Status, http.StatusContinue)
	}

	n, err := sr.Write([]byte("body"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 4 {
		t.Errorf("Write returned %d bytes, want 4", n)
	}
	// Write sets Status=200 on StatusRecorder, reflecting the implicit 200 that
	// http.ResponseWriter.Write always commits on the underlying writer when called
	// without a prior final WriteHeader — even after a 1xx informational WriteHeader.
	// Keeping sr.Status in sync with what the client receives is the contract.
	if sr.Status != http.StatusOK {
		t.Errorf("Status = %d after 1xx+Write, want %d (Write must track implicit 200)", sr.Status, http.StatusOK)
	}
	if !sr.wroteHeader {
		t.Error("wroteHeader must be true after Write")
	}
}

func TestStatusRecorderFlushDelegatesToFlusher(t *testing.T) {
	inner := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	sr := NewStatusRecorder(inner)
	sr.Flush()
	if !inner.flushed {
		t.Error("Flush must delegate to the underlying http.Flusher")
	}
}

// nonFlusherWriter is a minimal http.ResponseWriter that deliberately does NOT
// implement http.Flusher, so TestStatusRecorderFlushNoopWhenNotFlusher can
// exercise the no-op delegation path in StatusRecorder.Flush.
type nonFlusherWriter struct{ h http.Header }

func (n *nonFlusherWriter) Header() http.Header         { return n.h }
func (n *nonFlusherWriter) Write(b []byte) (int, error) { return len(b), nil }
func (n *nonFlusherWriter) WriteHeader(_ int)           {}

func TestStatusRecorderFlushNoopWhenNotFlusher(t *testing.T) {
	// nonFlusherWriter does not implement http.Flusher; Flush must be a safe no-op.
	sr := NewStatusRecorder(&nonFlusherWriter{h: make(http.Header)})
	sr.Flush() // must not panic
}

func TestStatusRecorderWriteWithoutWriteHeader(t *testing.T) {
	inner := httptest.NewRecorder()
	sr := NewStatusRecorder(inner)

	_, _ = sr.Write([]byte("implicit 200"))
	// Write commits wroteHeader (Status == 200 default). Subsequent WriteHeader calls
	// are ignored. Status stays at the NewStatusRecorder default of 200.
	if sr.Status != http.StatusOK {
		t.Errorf("Status after Write = %d, want %d", sr.Status, http.StatusOK)
	}
	if inner.Code != http.StatusOK {
		t.Errorf("inner Code after Write = %d, want %d", inner.Code, http.StatusOK)
	}

	sr.WriteHeader(http.StatusInternalServerError) // Should be ignored because header was implicitly written

	if sr.Status != http.StatusOK {
		t.Errorf("Status = %d, want %d", sr.Status, http.StatusOK)
	}
	if inner.Code != http.StatusOK {
		t.Errorf("inner Code = %d, want %d", inner.Code, http.StatusOK)
	}
}
