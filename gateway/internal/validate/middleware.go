package validate

import (
	"bytes"
	"io"
	"net/http"

	"github.com/Unluckyathecking/crucible/gateway/internal/apierror"
	mwpkg "github.com/Unluckyathecking/crucible/gateway/internal/middleware"
	"github.com/Unluckyathecking/crucible/gateway/internal/openapi"
)

// Middleware returns a chi middleware that validates the POST request body
// against the RequestSchema declared for the matched route.
//
// Routes without a RequestSchema are passed through unchanged (nil/no-op per route).
//
// Body restoration: body bytes are read once, validated, then r.Body is replaced
// with io.NopCloser(bytes.NewReader(bodyBytes)) so downstream handlers decode
// the body byte-identical to the original (same idiom as idempotency/middleware.go).
//
// Ordering invariant: must be registered AFTER idempotency.Middleware (replays
// exit before reaching this middleware, so replayed bodies bypass schema
// validation) and BEFORE quota.Middleware (invalid bodies never consume quota).
func Middleware(routes []openapi.RouteDescriptor) func(http.Handler) http.Handler {
	// Build a map from the full chi route pattern ("/v1" + rt.Path) to the
	// route's RequestSchema. Routes without a schema are omitted; a missing
	// map entry is the fast path for schema-less routes.
	schemas := make(map[string]*openapi.Schema, len(routes))
	for _, rt := range routes {
		if rt.RequestSchema != nil {
			schemas["/v1"+rt.Path] = rt.RequestSchema
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Fast path: no schemas registered.
			if len(schemas) == 0 {
				next.ServeHTTP(w, r)
				return
			}

			// Use the cleaned request URL path as the lookup key.
			// chi sub-router middleware runs before the inner route is fully
			// matched (RoutePattern returns "/v1/*" at this point), so we use
			// r.URL.Path which Go's net/http has already cleaned and normalized.
			// All Crucible /v1 routes are exact paths with no URL parameters,
			// so URL.Path == the registered route pattern ("/v1" + rt.Path).
			schema, hasSchema := schemas[r.URL.Path]
			if !hasSchema || schema == nil {
				next.ServeHTTP(w, r)
				return
			}

			rid, _ := r.Context().Value(mwpkg.RequestIDKey).(string)

			// Read the body; restore r.Body immediately so downstream handlers
			// (including the error path) can still read it.
			// Mirrors idempotency/middleware.go:111-114.
			orig := r.Body
			bodyBytes, err := io.ReadAll(orig)
			orig.Close()
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			if err != nil {
				apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "could not read request body", false)
				return
			}

			if verr := ValidateBytes(schema, bodyBytes); verr != nil {
				apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, verr.Error(), false)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
