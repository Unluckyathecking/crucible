// Package httputil holds tiny shared HTTP helpers that don't earn their own package.
package httputil

import "net/http"

// StatusRecorder wraps http.ResponseWriter to capture the response status code,
// so middleware can log/measure it after the inner handler returns. Both the
// access-log middleware and the Prometheus metrics middleware need this — keep
// it here so one copy can't drift from the other.
type StatusRecorder struct {
	http.ResponseWriter
	Status      int
	wroteHeader bool // true once a non-1xx status has been committed on StatusRecorder
}

// Compile-time assertion: *StatusRecorder must implement http.Flusher.
var _ http.Flusher = (*StatusRecorder)(nil)

func NewStatusRecorder(w http.ResponseWriter) *StatusRecorder {
	return &StatusRecorder{ResponseWriter: w, Status: http.StatusOK}
}

// WriteHeader forwards code to the underlying writer and records Status on the
// first call. For 1xx informational codes, Status is recorded but wroteHeader
// is not committed — a subsequent 2xx-5xx WriteHeader or implicit Write can still
// finalize the response. Non-informational codes commit on the first call;
// subsequent WriteHeader calls are silently ignored per HTTP semantics.
func (s *StatusRecorder) WriteHeader(code int) {
	if s.wroteHeader {
		return
	}
	s.Status = code
	if code >= 200 {
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

// Write records an implicit 200 on StatusRecorder if no non-1xx WriteHeader has been
// committed yet, then explicitly sends WriteHeader(200) to the inner writer before
// writing the body. This is required for real net/http.response where a prior 1xx
// WriteHeader leaves the inner writer's final-status slot still open (wroteHeader=false
// on the real writer). For httptest.ResponseRecorder the call is a no-op (its own
// wroteHeader is already true from the 1xx WriteHeader), so there is no double-header.
// The authoritative final-status field is sr.Status, which middleware logging and
// Prometheus metrics consume.
func (s *StatusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.Status = http.StatusOK
		s.wroteHeader = true
		s.ResponseWriter.WriteHeader(http.StatusOK)
	}
	return s.ResponseWriter.Write(b)
}

// Flush implements http.Flusher by delegating to the underlying writer when it
// supports flushing. This preserves the optional interface through the wrapper so
// downstream type assertions like w.(http.Flusher) succeed on a StatusRecorder.
func (s *StatusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
