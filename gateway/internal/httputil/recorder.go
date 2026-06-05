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
// first non-informational (2xx+) call. Informational 1xx codes are forwarded so
// that Early Hints and similar responses reach the client, but they do not commit
// wroteHeader — this lets the recorder capture the final response status rather
// than the interim informational code. Non-informational codes commit on the first
// call; subsequent WriteHeader calls are silently ignored per HTTP semantics.
func (s *StatusRecorder) WriteHeader(code int) {
	if s.wroteHeader {
		return
	}
	if code >= 200 {
		s.Status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

// Write triggers an implicit 200 if no status has been committed yet.
// Fields are set directly rather than delegating to WriteHeader so that the
// underlying writer is NOT sent a second WriteHeader call. This matters for
// the 1xx-then-body sequence: WriteHeader(100) forwards to the underlying
// writer but leaves s.wroteHeader=false intentionally. If Write then called
// s.WriteHeader(200), the underlying writer would receive two WriteHeader
// calls (100 then 200), which is incorrect on real net/http ResponseWriters.
// The underlying writer's own implicit-200 path handles the non-1xx case.
// A prior WriteHeader(5xx) sets s.wroteHeader=true, so the if-block is
// skipped and Status stays at the 5xx value — no 200 leak is possible.
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
