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
	wroteHeader bool
}

// Compile-time assertion: *StatusRecorder must implement http.Flusher.
var _ http.Flusher = (*StatusRecorder)(nil)

func NewStatusRecorder(w http.ResponseWriter) *StatusRecorder {
	return &StatusRecorder{ResponseWriter: w, Status: http.StatusOK}
}

// WriteHeader records the status code and forwards it to the underlying writer on
// the first call. Subsequent calls are silently ignored — the HTTP spec only
// allows one response status per request. The Go HTTP server handles 100-Continue
// automatically before handlers run, so handlers never need to forward 1xx codes
// through a recorder.
func (s *StatusRecorder) WriteHeader(code int) {
	if s.wroteHeader {
		return
	}
	s.Status = code
	s.wroteHeader = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *StatusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.wroteHeader = true
		s.Status = http.StatusOK
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
