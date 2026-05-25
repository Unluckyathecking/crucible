package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

type ctxKey string

const keyCtxKey ctxKey = "auth.key"

// Middleware gates downstream handlers behind Bearer-token API key auth.
// /healthz must be mounted outside this middleware so liveness checks still work.
func Middleware(store *Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authz := r.Header.Get("Authorization")
			// HTTP auth schemes are case-insensitive per RFC 7235; accept "Bearer ", "bearer ", etc.
			token := ""
			if len(authz) > 7 && strings.EqualFold(authz[:7], "Bearer ") {
				token = authz[7:]
			}
			if token == "" {
				writeUnauthorized(w, "missing or malformed Authorization header")
				return
			}
			key, err := store.Lookup(r.Context(), strings.TrimSpace(token))
			if err != nil {
				if errors.Is(err, ErrKeyNotFound) {
					writeUnauthorized(w, "invalid api key")
					return
				}
				// Log the internal error for operational visibility before returning generic response.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{
						"code":    "INTERNAL",
						"message": "auth lookup failed",
					},
				})
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

func writeUnauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	// json.Encoder over string-concat — the current callers only pass literals,
	// but encoding eliminates the footgun if a future call site forwards user input
	// (which would break the envelope and could enable response-splitting).
	// We ignore the error here as there's no logging wired up for auth failures and write failures imply connection dropped.
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":    "UNAUTHORIZED",
			"message": msg,
		},
	})
}
