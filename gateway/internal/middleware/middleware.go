// Package middleware provides the HTTP middleware stack every Crucible gateway route shares.
//
// Mount order (outer → inner): RequestID → AccessLog → Recovery → SecurityHeaders → BodyLimit.
package middleware

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/httputil"
)

type ctxKey string

const RequestIDKey ctxKey = "request_id"

// RequestID stamps an X-Request-ID on every request, honouring an inbound one if reasonable.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" || len(id) > 64 {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", id)
		ctx := context.WithValue(r.Context(), RequestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Recovery converts panics into a safe 500 envelope. Real cause logged with request id.
func Recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				rid, _ := r.Context().Value(RequestIDKey).(string)
				log.Error().
					Str("request_id", rid).
					Interface("panic", rec).
					Msg("panic in handler")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"code":"INTERNAL","message":"internal error"}}`))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// AccessLog emits one structured log line per request.
func AccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := httputil.NewStatusRecorder(w)
		next.ServeHTTP(ww, r)

		rid, _ := r.Context().Value(RequestIDKey).(string)
		log.Info().
			Str("request_id", rid).
			Str("method", r.Method).
			Str("path", r.URL.Path).
			Int("status", ww.Status).
			Dur("latency", time.Since(start)).
			Msg("access")
	})
}

// SecurityHeaders sets OWASP-recommended defaults that every response should carry.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-XSS-Protection", "0")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), interest-cohort=()")
		next.ServeHTTP(w, r)
	})
}

// BodyLimit caps inbound request body size. Per-route stricter limits go via http.MaxBytesReader in the handler.
func BodyLimit(max int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, max)
			next.ServeHTTP(w, r)
		})
	}
}
