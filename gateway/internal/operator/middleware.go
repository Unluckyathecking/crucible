package operator

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/Unluckyathecking/crucible/gateway/internal/apierror"
	mwpkg "github.com/Unluckyathecking/crucible/gateway/internal/middleware"
)

// Middleware validates Authorization: Bearer <token> against the static operator token
// using constant-time comparison (subtle.ConstantTimeCompare).
//
// Returns 401 when:
//   - token is empty (OPERATOR_TOKEN not configured)
//   - Authorization header is missing or malformed
//   - presented bearer does not match token
//
// The customer auth.Middleware path is separate and unchanged by this middleware.
func Middleware(token string) func(http.Handler) http.Handler {
	tokenBytes := []byte(token)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rid, _ := r.Context().Value(mwpkg.RequestIDKey).(string)

			authz := r.Header.Get("Authorization")
			// HTTP auth scheme is case-insensitive per RFC 7235.
			bearer := ""
			if len(authz) > 7 && strings.EqualFold(authz[:7], "Bearer ") {
				bearer = strings.TrimSpace(authz[7:])
			}

			// Both an unconfigured operator token and a wrong bearer produce 401
			// so the response does not reveal whether OPERATOR_TOKEN is set.
			if len(tokenBytes) == 0 || bearer == "" ||
				subtle.ConstantTimeCompare([]byte(bearer), tokenBytes) != 1 {
				apierror.Write(w, rid, http.StatusUnauthorized, apierror.UNAUTHORIZED, "invalid operator token", false)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
