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

// Write records an implicit 200 on StatusRecorder if no non-1xx status has been
// committed yet, then delegates to the inner writer. The inner writer's own Write
// triggers the implicit 200 on itself — we do NOT call s.ResponseWriter.WriteHeader
// here — so that a preceding 1xx WriteHeader (already forwarded) does not cause a
// double WriteHeader call on the inner writer.
func (s *StatusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.Status = http.StatusOK
		s.wroteHeader = true
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
