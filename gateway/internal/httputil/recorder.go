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

func NewStatusRecorder(w http.ResponseWriter) *StatusRecorder {
	return &StatusRecorder{ResponseWriter: w, Status: http.StatusOK}
}

// WriteHeader records the final response status and forwards the code to the
// underlying writer. Informational (1xx) codes are passed through without
// committing wroteHeader so the final 2xx-5xx status can be recorded on the
// subsequent call — matching HTTP semantics where 100 Continue may precede
// the final response. All non-informational codes commit on the first call,
// preventing any further WriteHeader forwarding to the underlying writer.
func (s *StatusRecorder) WriteHeader(code int) {
	if s.wroteHeader {
		return
	}
	if code < 200 {
		// 1xx informational: forward without locking wroteHeader.
		// The final status code arrives on the next WriteHeader call.
		s.ResponseWriter.WriteHeader(code)
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
