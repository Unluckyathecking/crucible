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

// flush copies the captured headers + status + body to the real writer.
func (c *captureWriter) flush(w http.ResponseWriter) {
	for k, vv := range c.header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(c.status)
	_, _ = w.Write(c.body.Bytes())
}

// Middleware deduplicates POST requests within the idempotency TTL window.
//
// Absent Idempotency-Key header → pass-through (zero behaviour change).
// store == nil → pass-through for all requests (feature disabled; main.go untouched).
//
// Must be mounted AFTER auth.Middleware — it reads the authenticated Key from context.
func Middleware(store *Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if store == nil {
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

			// Read and restore the body so the downstream handler still sees it.
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				writeIDError(w, http.StatusBadRequest, "BAD_REQUEST", "could not read body", false)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			fp := fingerprint(bodyBytes)

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
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("X-Idempotent-Replayed", "true")
					w.WriteHeader(*entry.StatusCode)
					_, _ = w.Write(entry.Body)
					return
				}
			}

			// We own the key. Run the handler and capture its response.
			cw := newCaptureWriter()
			next.ServeHTTP(cw, r)

			status := cw.status
			body := cw.body.Bytes()

			if status >= 200 && status < 300 {
				// Persist only on 2xx so retryable failures remain retryable.
				bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := store.Finalize(bg, customerID, key, status, body); err != nil {
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

func fingerprint(body []byte) []byte {
	sum := sha256.Sum256(body)
	return sum[:]
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
