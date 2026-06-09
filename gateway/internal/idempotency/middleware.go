package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/apierror"
	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	mwpkg "github.com/Unluckyathecking/crucible/gateway/internal/middleware"
)

const (
	maxKeyLen        = 255
	bgOpTimeout      = 5 * time.Second
	minSuccessStatus = 200
	maxSuccessStatus = 299
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
	c.status = code
	c.wrote = true
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
			rid, _ := r.Context().Value(mwpkg.RequestIDKey).(string)

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
				apierror.Write(w, rid, http.StatusBadRequest, apierror.IDEMPOTENCY_KEY_INVALID, fmt.Sprintf("Idempotency-Key too long (max %d characters)", maxKeyLen), false)
				return
			}

			authKey := auth.FromContext(r.Context())
			if authKey == nil {
				// Must not happen — middleware is mounted after auth. Fail-open.
				next.ServeHTTP(w, r)
				return
			}
			customerID := authKey.Customer.ID

			// Read the body; restore r.Body BEFORE the error check so that
			// recovery middleware and access-log handlers that run after a 4xx
			// still see readable bytes rather than the closed original reader.
			// We return immediately after writing the 400, so no downstream
			// handler ever reads the partial body on the error path.
			origBody := r.Body
			bodyBytes, err := io.ReadAll(origBody)
			origBody.Close()
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			if err != nil {
				apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "could not read body", false)
				return
			}
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
						apierror.Write(w, rid, http.StatusConflict, apierror.IDEMPOTENCY_CONFLICT, "concurrent request with same key", false)
						return
					}
					// Fall through: we now own the key.
				} else if !bytes.Equal(fp, entry.Fingerprint) {
					// Design invariant: do NOT delete the stored row on fingerprint mismatch.
					// Deleting a completed row would allow a retry with the original body to
					// re-execute the worker — exactly the double-billing this module prevents.
					// 422 permanently prevents the mismatched body from executing; the
					// original fingerprint's replay remains available and is always safe.
					apierror.Write(w, rid, http.StatusUnprocessableEntity, apierror.IDEMPOTENCY_KEY_REUSE, "key reused with different request body", false)
					return
				} else if entry.StatusCode == nil {
					apierror.Write(w, rid, http.StatusConflict, apierror.IDEMPOTENCY_CONFLICT, "concurrent request with same key", false)
					return
				} else {
					// Completed entry: replay stored response without invoking the worker.
					// Skip all RFC 7230 hop-by-hop headers plus Content-Length (we
					// recompute it from the stored body) and Date (regenerated below
					// so the replayed response is a clean, length-framed message with
					// no conflicting framing or stale timestamp headers.
					dst := w.Header()
					for k, vv := range entry.ResponseHeaders {
						ck := http.CanonicalHeaderKey(k)
						switch ck {
						case "Connection", "Content-Length", "Date", "Keep-Alive",
							"Proxy-Authenticate", "Proxy-Authorization",
							"Te", "Trailer", "Transfer-Encoding", "Upgrade":
							continue
						}
						dst[ck] = vv
					}
					w.Header().Set("X-Idempotent-Replayed", "true")
					w.Header().Set("Content-Length", strconv.Itoa(len(entry.Body)))
					w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
					w.WriteHeader(*entry.StatusCode)
					_, _ = w.Write(entry.Body)
					return
				}
			}

			// We own the key. Release it on panic so clients can retry after a 500.
			// recover() is used directly so the defer is self-contained and does not
			// depend on a boolean flag that runtime.Goexit could leave stale.
			//
			// NOTE: cw.flush(w) is deliberately NOT called on this panic path.
			// captureWriter buffers all handler output; the real ResponseWriter w
			// is still clean when we re-panic. Flushing a partial/incomplete
			// handler response here would commit headers to the client and prevent
			// the outer mw.Recovery middleware from writing a proper 500 response.
			defer func() {
				if v := recover(); v != nil {
					bg, cancel := context.WithTimeout(context.Background(), bgOpTimeout)
					defer cancel()
					if err := store.Release(bg, customerID, key); err != nil {
						log.Warn().Err(err).Str("key", key).Msg("idempotency: panic-path release failed")
					}
					panic(v) // re-panic so outer recovery middleware (mw.Recovery) still fires
				}
			}()

			// Run the handler and capture its response.
			cw := newCaptureWriter()
			next.ServeHTTP(cw, r)

			status := cw.status
			body := cw.body.Bytes()

			if status >= minSuccessStatus && status <= maxSuccessStatus {
				// Persist only on 2xx so retryable failures remain retryable.
				bg, cancel := context.WithTimeout(context.Background(), bgOpTimeout)
				defer cancel()
				if err := store.Finalize(bg, customerID, key, status, body, cw.header, fp); err != nil {
					log.Warn().Err(err).Str("key", key).Msg("idempotency: finalize failed, releasing so client can retry")
					// bg may be exhausted if Finalize consumed the full timeout; allocate
					// a fresh context so Release doesn't fail with an expired deadline.
					relCtx, relCancel := context.WithTimeout(context.Background(), bgOpTimeout)
					defer relCancel()
					if rerr := store.Release(relCtx, customerID, key); rerr != nil {
						log.Warn().Err(rerr).Str("key", key).Msg("idempotency: release after finalize-fail also failed")
					}
				}
			} else {
				// Release the pending row so the client can genuinely retry.
				bg, cancel := context.WithTimeout(context.Background(), bgOpTimeout)
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

