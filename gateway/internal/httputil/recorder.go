// Package httputil holds tiny shared HTTP helpers that don't earn their own package.
package httputil

import "net/http"

// StatusRecorder wraps http.ResponseWriter to capture the response status code,
// so middleware can log/measure it after the inner handler returns. Both the
// access-log middleware and the Prometheus metrics middleware need this — keep
// it here so one copy can't drift from the other.
//
// StatusRecorder is not goroutine-safe — matching the http.ResponseWriter contract.
// It is designed for use by the single goroutine serving an HTTP request. Deferred
// middleware reads Status after ServeHTTP returns; all Write/WriteHeader calls
// complete before that read, so access is sequentially consistent.
type StatusRecorder struct {
	http.ResponseWriter
	Status      int
	wroteHeader bool // true once a non-1xx (final) status has been committed
	wrote1xx    bool // true once any WriteHeader has been called, including 1xx
}

// Compile-time assertion: *StatusRecorder must implement http.Flusher.
var _ http.Flusher = (*StatusRecorder)(nil)

func NewStatusRecorder(w http.ResponseWriter) *StatusRecorder {
	return &StatusRecorder{ResponseWriter: w, Status: http.StatusOK}
}

// WriteHeader forwards code to the underlying writer and records Status on the
// first call. For 1xx informational codes, Status is recorded but wroteHeader
// is not committed — a subsequent 2xx-5xx WriteHeader or Write (which always
// finalizes to 200) can still finalize the response. Non-informational codes
// commit on the first call; subsequent WriteHeader calls are silently ignored.
func (s *StatusRecorder) WriteHeader(code int) {
	if s.wroteHeader {
		return
	}
	s.Status = code
	if code >= 200 {
		s.wroteHeader = true
	}
	s.wrote1xx = true
	s.ResponseWriter.WriteHeader(code)
}

// Write records an implicit 200 on StatusRecorder only when no WriteHeader of any kind
// has been called yet — neither a final (2xx-5xx) nor an informational (1xx) code.
// When WriteHeader(1xx) was called first, wrote1xx is true and Write leaves Status
// unchanged, preserving the explicitly set informational code. Middleware observing
// s.Status after the handler returns will see the last explicitly set status.
func (s *StatusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader && !s.wrote1xx {
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
