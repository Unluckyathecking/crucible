package respcache

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	mwpkg "github.com/Unluckyathecking/crucible/gateway/internal/middleware"
	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
	"github.com/Unluckyathecking/crucible/gateway/internal/usage"
)

// bgOpTimeout bounds the cache-populate write on a miss so a slow Redis never
// holds the response past a reasonable ceiling; it deliberately does not bound
// the hit-path recorder.Record call, which reuses the request context so the
// quota middleware's MarkRecorded signal (recordedKey, gateway/internal/quota)
// still reaches it — see Middleware's doc comment.
const bgOpTimeout = 5 * time.Second

// captureWriter buffers a handler's response so Middleware can decide whether
// the result is eligible for caching before anything reaches the real
// ResponseWriter. Kept local rather than shared with idempotency.captureWriter:
// the two packages model different contracts (see package doc) and the
// forbidden constraints on this module require respcache to stay a separate,
// independently editable package.
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

func (c *captureWriter) flush(w http.ResponseWriter) {
	dst := w.Header()
	for k, vv := range c.header {
		dst[k] = vv
	}
	w.WriteHeader(c.status)
	_, _ = w.Write(c.body.Bytes())
}

// Middleware serves cached worker responses for a single cache-opted /v1
// route. Mount it per-route, immediately in front of the worker invoke
// handler and AFTER quota.Middleware in the chain: a cache hit must remain a
// fully-metered, quota-counted, per-customer-billed call — it only skips the
// worker HTTP round-trip.
//
// store == nil or ttl <= 0 is strict pass-through (feature disabled, or this
// route hasn't opted in).
//
// On a hit, Middleware calls recorder.Record itself — using the request's own
// context, not a derived one, so the quota middleware's in-context
// MarkRecorded signal still reaches it — because the invoke handler never
// runs and so never records usage on its own. On a miss, invoke() runs
// normally (including its own recorder.Record); Middleware only observes the
// response to decide whether to populate the cache.
//
// metrics is optional (nil = no-op for test-isolated counters). Package-level
// observability vars are always incremented for production /metrics output.
func Middleware(store *Store, recorder *usage.Recorder, operation string, ttl time.Duration, metrics *observability.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if store == nil || ttl <= 0 {
				next.ServeHTTP(w, r)
				return
			}

			// Read the body for hashing, then restore it so invoke() can decode it
			// as normal on both the miss path and the fail-open paths below.
			origBody := r.Body
			bodyBytes, err := io.ReadAll(origBody)
			origBody.Close()
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			if err != nil {
				next.ServeHTTP(w, r) // let invoke()'s own body read surface the error
				return
			}

			key, err := Key(operation, bodyBytes)
			if err != nil {
				next.ServeHTTP(w, r) // malformed payload; invoke()'s JSON decode will reject it
				return
			}

			entry, err := store.Get(r.Context(), key)
			if err != nil {
				log.Warn().Err(err).Str("operation", operation).Msg("respcache: get failed, failing open")
				observability.RespCacheFailOpenTotal.WithLabelValues(operation).Inc()
				if metrics != nil {
					metrics.RespCacheFailOpenTotal.WithLabelValues(operation).Inc()
				}
				next.ServeHTTP(w, r)
				return
			}

			if entry != nil {
				observability.RespCacheHitsTotal.WithLabelValues(operation).Inc()
				if metrics != nil {
					metrics.RespCacheHitsTotal.WithLabelValues(operation).Inc()
				}
				if authKey := auth.FromContext(r.Context()); authKey != nil {
					rid, _ := r.Context().Value(mwpkg.RequestIDKey).(string)
					// r.Context(), not a derived context: recorder.Record calls
					// quota.MarkRecorded, which reads a signal seeded into this exact
					// request's context by quota.Middleware. A background context
					// would silently drop that signal and cause quota.Middleware to
					// refund the reserved unit for a call we just billed.
					if err := recorder.Record(r.Context(), authKey.Customer.ID, authKey.ID, operation, rid, entry.BillableUnits); err != nil {
						log.Warn().Err(err).Str("operation", operation).Msg("respcache: usage record failed on cache hit")
					}
				}
				dst := w.Header()
				if entry.ContentType != "" {
					dst.Set("Content-Type", entry.ContentType)
				}
				dst.Set("Cache-Control", "no-store")
				if entry.BillableUnits > 0 {
					dst.Set("X-Billable-Units", strconv.FormatUint(entry.BillableUnits, 10))
				}
				if entry.UnitsLabel != "" {
					dst.Set("X-Units-Label", entry.UnitsLabel)
				}
				dst.Set("X-Respcache", "hit")
				w.WriteHeader(entry.StatusCode)
				_, _ = w.Write(entry.Body)
				return
			}

			observability.RespCacheMissesTotal.WithLabelValues(operation).Inc()
			if metrics != nil {
				metrics.RespCacheMissesTotal.WithLabelValues(operation).Inc()
			}
			cw := newCaptureWriter()
			next.ServeHTTP(cw, r)

			// Only responses that already passed routes.go's success +
			// BillableUnits >= 1 trust-boundary check are eligible: that check
			// rejects billable_units<1 with 502 before invoke() ever writes a 2xx,
			// so a 2xx status here already implies units >= 1. The header lookup
			// below recovers the actual number to store, not to re-validate it.
			if cw.status >= 200 && cw.status <= 299 {
				if raw := cw.header.Get("X-Billable-Units"); raw != "" {
					if units, perr := strconv.ParseUint(raw, 10, 64); perr == nil && units >= 1 {
						e := &Entry{
							StatusCode:    cw.status,
							Body:          append([]byte(nil), cw.body.Bytes()...),
							ContentType:   cw.header.Get("Content-Type"),
							BillableUnits: units,
							UnitsLabel:    cw.header.Get("X-Units-Label"),
						}
						bg, cancel := context.WithTimeout(context.Background(), bgOpTimeout)
						if serr := store.Set(bg, key, e, ttl); serr != nil {
							log.Warn().Err(serr).Str("operation", operation).Msg("respcache: set failed")
						}
						cancel()
					}
				}
			}
			cw.flush(w)
		})
	}
}
