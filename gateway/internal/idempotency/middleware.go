package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
)

const (
	maxKeyLen    = 255
	bgOpTimeout  = 5 * time.Second
)

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
// Ordering invariant: must be registered AFTER auth.Middleware (needs auth context)
// and as the OUTER middleware relative to quota.Middleware. In chi, middleware
// registered earlier with r.Use() is outermost and executes first. Being outer
// lets us return early on replay without ever invoking quota — replays must not
// reserve or refund quota.
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
				writeIDError(w, http.StatusBadRequest, "IDEMPOTENCY_KEY_INVALID", "Idempotency-Key too long (max 255 characters)", false)
				return
			}

			authKey := auth.FromContext(r.Context())
			if authKey == nil {
				// Must not happen — middleware is mounted after auth. Fail-open.
				next.ServeHTTP(w, r)
				return
			}
			customerID := authKey.Customer.ID

			// Read the body; close the original immediately (fully consumed) and
			// restore a fresh reader for downstream handlers.
			origBody := r.Body
			bodyBytes, err := io.ReadAll(origBody)
			origBody.Close()
			if err != nil {
				writeIDError(w, http.StatusBadRequest, "BAD_REQUEST", "could not read body", false)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			fp := fingerprint(r.Method, bodyBytes, r.URL.RequestURI())

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
					if err != nil {
						log.Warn().Err(err).Str("key", key).Msg("idempotency: re-claim error, failing open")
						next.ServeHTTP(w, r)
						return
					}
					if !claimed {
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
					// Skip all RFC 7230 hop-by-hop headers plus Content-Length (we
					// recompute it from the stored body) so the replayed response is a
					// clean, length-framed message with no conflicting framing headers.
					dst := w.Header()
					for k, vv := range entry.ResponseHeaders {
						switch k {
						case "Connection", "Content-Length", "Keep-Alive",
							"Proxy-Authenticate", "Proxy-Authorization",
							"Te", "Trailers", "Transfer-Encoding", "Upgrade":
							continue
						}
						dst[k] = vv
					}
					w.Header().Set("X-Idempotent-Replayed", "true")
					w.Header().Set("Content-Length", strconv.Itoa(len(entry.Body)))
					w.WriteHeader(*entry.StatusCode)
					_, _ = w.Write(entry.Body)
					return
				}
			}

			// We own the key. Release it on panic so clients can retry after a 500.
			panicked := true
			defer func() {
				if panicked {
					bg, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), bgOpTimeout)
					defer cancel()
					if err := store.Release(bg, customerID, key); err != nil {
						log.Warn().Err(err).Str("key", key).Msg("idempotency: panic-path release failed")
					}
				}
			}()

			// Run the handler and capture its response.
			cw := newCaptureWriter()
			next.ServeHTTP(cw, r)

			status := cw.status
			body := cw.body.Bytes()
			panicked = false // handler returned normally; explicit Finalize/Release below owns the key

			if status >= 200 && status < 300 {
				// Persist only on 2xx so retryable failures remain retryable.
				bg, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), bgOpTimeout)
				defer cancel()
				if err := store.Finalize(bg, customerID, key, status, body, cw.header, fp); err != nil {
					log.Warn().Err(err).Str("key", key).Msg("idempotency: finalize failed, releasing so client can retry")
					// Release the pending row so a retry sees a fresh key rather
					// than a stuck pending claim that would 409 until TTL expires.
					if rerr := store.Release(bg, customerID, key); rerr != nil {
						log.Warn().Err(rerr).Str("key", key).Msg("idempotency: release after finalize-fail also failed")
					}
				}
			} else {
				// Release the pending row so the client can genuinely retry.
				bg, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), bgOpTimeout)
				defer cancel()
				if err := store.Release(bg, customerID, key); err != nil {
					log.Warn().Err(err).Str("key", key).Msg("idempotency: release failed; key will expire after TTL")
				}
			}

			cw.flush(w)
		})
	}
}

// fingerprint returns SHA-256(method + \x00 + requestURI + \x00 + body).
// requestURI includes the path and any query string so identical keys with
// different paths or query parameters are not treated as the same request.
func fingerprint(method string, body []byte, requestURI string) []byte {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(requestURI))
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
