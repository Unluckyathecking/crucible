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
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rid, _ := r.Context().Value(mwpkg.RequestIDKey).(string)

			// Both an unconfigured operator token and a wrong bearer produce 401
			// so the response does not reveal whether OPERATOR_TOKEN is set.
			if !staticTokenMatches(token, parseBearer(r.Header.Get("Authorization"))) {
				apierror.Write(w, rid, http.StatusUnauthorized, apierror.UNAUTHORIZED, "invalid operator token", false)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// parseBearer extracts the token from an "Authorization: Bearer <token>" header,
// returning "" when the header is absent or not a Bearer credential. The scheme
// match is case-insensitive per RFC 7235.
func parseBearer(authz string) string {
	if len(authz) > 7 && strings.EqualFold(authz[:7], "Bearer ") {
		return strings.TrimSpace(authz[7:])
	}
	return ""
}

// staticTokenMatches reports whether presented equals the configured static
// operator token in constant time. An empty configured token (OPERATOR_TOKEN
// unset) or empty presented token never matches.
func staticTokenMatches(configured, presented string) bool {
	if configured == "" || presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(configured)) == 1
}
