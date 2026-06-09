package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/Unluckyathecking/crucible/gateway/internal/apierror"
	mwpkg "github.com/Unluckyathecking/crucible/gateway/internal/middleware"
)

type ctxKey string

const keyCtxKey ctxKey = "auth.key"

// Middleware gates downstream handlers behind Bearer-token API key auth.
// /healthz must be mounted outside this middleware so liveness checks still work.
func Middleware(store *Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rid, _ := r.Context().Value(mwpkg.RequestIDKey).(string)
			authz := r.Header.Get("Authorization")
			// HTTP auth schemes are case-insensitive per RFC 7235; accept "Bearer ", "bearer ", etc.
			token := ""
			if len(authz) > 7 && strings.EqualFold(authz[:7], "Bearer ") {
				token = authz[7:]
			}
			if token == "" {
				apierror.Write(w, rid, http.StatusUnauthorized, apierror.UNAUTHORIZED, "missing or malformed Authorization header", false)
				return
			}
			key, err := store.Lookup(r.Context(), strings.TrimSpace(token))
			if err != nil {
				if errors.Is(err, ErrKeyNotFound) {
					apierror.Write(w, rid, http.StatusUnauthorized, apierror.UNAUTHORIZED, "invalid api key", false)
					return
				}
				apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "auth lookup failed", true) // conservative: most pgx errors are transient connection issues
				return
			}
			ctx := context.WithValue(r.Context(), keyCtxKey, key)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// FromContext retrieves the authenticated Key from the request context. Returns nil if absent.
func FromContext(ctx context.Context) *Key {
	k, _ := ctx.Value(keyCtxKey).(*Key)
	return k
}
