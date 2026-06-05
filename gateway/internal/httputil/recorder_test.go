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

// TestStatusRecorder1xxCommitsOnFirstCall documents that a 1xx code commits
// the recorder immediately. The Go HTTP server manages 100-Continue internally
// before the handler runs, so a handler never needs to send 1xx through a
// recorder. If one does, the recorder treats it as the final status.
func TestStatusRecorder1xxCommitsOnFirstCall(t *testing.T) {
	inner := httptest.NewRecorder()
	sr := NewStatusRecorder(inner)

	sr.WriteHeader(http.StatusContinue) // 100 — commits on first call
	if !sr.wroteHeader {
		t.Error("wroteHeader should be true after first WriteHeader call")
	}
	if sr.Status != http.StatusContinue {
		t.Errorf("Status = %d, want %d", sr.Status, http.StatusContinue)
	}

	sr.WriteHeader(http.StatusOK) // ignored — already committed
	if sr.Status != http.StatusContinue {
		t.Errorf("Status after second WriteHeader = %d, want %d (second call must be ignored)", sr.Status, http.StatusContinue)
	}
}

func TestStatusRecorderWriteWithoutWriteHeader(t *testing.T) {
	inner := httptest.NewRecorder()
	sr := NewStatusRecorder(inner)

    // explicitly reset the status to verify Write actually sets it to 200
    sr.Status = 0

	_, _ = sr.Write([]byte("implicit 200"))
	// Critical: verify Status is locked at 200 immediately after Write
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
