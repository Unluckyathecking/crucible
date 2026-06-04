package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
)

const maxKeyLen = 255

// captureWriter intercepts the handler's response without forwarding to the
// real ResponseWriter until flush() is called. Allows idempotency middleware
// to inspect status + body before deciding whether to persist or release.
type captureWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
	wrote  bool
}

func newCaptureWriter() *captureWriter {
	return &captureWriter{header: make(http.Header), status: http.StatusOK}
}

func (c *captureWriter) Header() http.Header { return c.header }

func (c *captureWriter) WriteHeader(code int) {
	if !c.wrote {
		c.status = code
		c.wrote = true
	}
}

func (c *captureWriter) Write(p []byte) (int, error) {
	if !c.wrote {
		c.status = http.StatusOK
		c.wrote = true
	}
	return c.body.Write(p)
}

// flush copies the captured headers, status, and body to the real writer.
// Direct map assignment avoids duplicate header values from a prior Add call.
func (c *captureWriter) flush(w http.ResponseWriter) {
	dst := w.Header()
	for k, vv := range c.header {
		dst[k] = vv
	}
	w.WriteHeader(c.status)
	_, _ = w.Write(c.body.Bytes())
}

// Middleware deduplicates POST /v1/* retries within the idempotency TTL window.
//
// Only POST requests that carry an Idempotency-Key header are deduplicated;
// all other requests (non-POST or missing header) are pass-through.
// store == nil → pass-through for all requests (feature disabled; main.go untouched).
//
// Mount AFTER auth.Middleware (reads authenticated Key from context) and
// BEFORE quota.Middleware so that replays exit before the quota reserve/refund
// path runs — replays must not reserve or refund quota.
func Middleware(store *Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if store == nil || r.Method != http.MethodPost {
				next.ServeHTTP(w, r)
				return
			}

			key := r.Header.Get("Idempotency-Key")
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}
			if len(key) > maxKeyLen {
				writeIDError(w, http.StatusBadRequest, "IDEMPOTENCY_KEY_INVALID", "idempotency-key too long", false)
				return
			}

			authKey := auth.FromContext(r.Context())
			if authKey == nil {
				// Must not happen — middleware is mounted after auth. Fail-open.
				next.ServeHTTP(w, r)
				return
			}
			customerID := authKey.Customer.ID

			// Read the body; close the original once done and restore for downstream.
			origBody := r.Body
			defer origBody.Close()
			bodyBytes, err := io.ReadAll(origBody)
			if err != nil {
				writeIDError(w, http.StatusBadRequest, "BAD_REQUEST", "could not read body", false)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			fp := fingerprint(bodyBytes, r.URL.Path)

			// Try to claim the key (UNIQUE INSERT — first in wins).
			claimed, err := store.Claim(r.Context(), customerID, key, fp)
			if err != nil {
				log.Warn().Err(err).Str("key", key).Msg("idempotency: claim error, failing open")
				next.ServeHTTP(w, r)
				return
			}

			if !claimed {
				entry, err := store.Load(r.Context(), customerID, key)
				if err != nil {
					log.Warn().Err(err).Str("key", key).Msg("idempotency: load error, failing open")
					next.ServeHTTP(w, r)
					return
				}

				if entry == nil {
					// Row expired between Claim and Load. Try one more Claim.
					claimed, err = store.Claim(r.Context(), customerID, key, fp)
					if err != nil || !claimed {
						writeIDError(w, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "concurrent request with same key", false)
						return
					}
					// Fall through: we now own the key.
				} else if !bytes.Equal(fp, entry.Fingerprint) {
					writeIDError(w, http.StatusUnprocessableEntity, "IDEMPOTENCY_KEY_REUSE", "key reused with different request body", false)
					return
				} else if entry.StatusCode == nil {
					writeIDError(w, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "concurrent request with same key", false)
					return
				} else {
					// Completed entry: replay stored response without invoking the worker.
					// Restore all original response headers first, then mark as replayed.
					dst := w.Header()
					for k, vv := range entry.ResponseHeaders {
						dst[k] = vv
					}
					w.Header().Set("X-Idempotent-Replayed", "true")
					w.WriteHeader(*entry.StatusCode)
					_, _ = w.Write(entry.Body)
					return
				}
			}

			// We own the key. Release it on panic so clients can retry after a 500.
			panicked := true
			defer func() {
				if panicked {
					bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if err := store.Release(bg, customerID, key); err != nil {
						log.Warn().Err(err).Str("key", key).Msg("idempotency: panic-path release failed")
					}
				}
			}()

			// Run the handler and capture its response.
			cw := newCaptureWriter()
			next.ServeHTTP(cw, r)
			panicked = false

			status := cw.status
			body := cw.body.Bytes()

			if status >= 200 && status < 300 {
				// Persist only on 2xx so retryable failures remain retryable.
				bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := store.Finalize(bg, customerID, key, status, body, cw.header); err != nil {
					log.Warn().Err(err).Str("key", key).Msg("idempotency: finalize failed")
				}
			} else {
				// Release the pending row so the client can genuinely retry.
				bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := store.Release(bg, customerID, key); err != nil {
					log.Warn().Err(err).Str("key", key).Msg("idempotency: release failed")
				}
			}

			cw.flush(w)
		})
	}
}

func fingerprint(body []byte, path string) []byte {
	h := sha256.New()
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write(body)
	return h.Sum(nil)
}

// writeIDError writes a JSON error envelope matching the gateway's stable shape.
func writeIDError(w http.ResponseWriter, status int, code, msg string, retryable bool) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":      code,
			"message":   msg,
			"retryable": retryable,
		},
	})
}
