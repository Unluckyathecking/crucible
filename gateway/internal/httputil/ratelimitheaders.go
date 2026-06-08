package httputil

import (
	"net/http"
	"strconv"
	"time"
)

// WriteRateLimitHeaders writes the IETF RateLimit-* draft fields and their
// de-facto X-RateLimit-* aliases. Both sets carry identical values so clients
// that expect either naming convention work without extra configuration.
//
// limit: per-minute request cap.
// remaining: slots left in the current sliding window after this call.
// resetAt: earliest time the full window could expire (callers pass now + 60 s).
func WriteRateLimitHeaders(w http.ResponseWriter, limit, remaining int, resetAt time.Time) {
	limitStr := strconv.Itoa(limit)
	remainStr := strconv.Itoa(remaining)
	resetStr := strconv.FormatInt(resetAt.Unix(), 10)

	h := w.Header()
	h.Set("RateLimit-Limit", limitStr)
	h.Set("RateLimit-Remaining", remainStr)
	h.Set("RateLimit-Reset", resetStr)
	h.Set("X-RateLimit-Limit", limitStr)
	h.Set("X-RateLimit-Remaining", remainStr)
	h.Set("X-RateLimit-Reset", resetStr)
}

// WriteQuotaHeaders writes the X-Quota-* response headers.
//
// cap: monthly billable-unit cap from the customer's plan.
// remaining: units left in the current calendar month (cap − counter after reserve).
// resetAt: UTC timestamp at which the quota counter resets. Callers pass
// expireAt(now) from tracker.go — the 2nd of the next UTC month.
func WriteQuotaHeaders(w http.ResponseWriter, cap, remaining int64, resetAt time.Time) {
	h := w.Header()
	h.Set("X-Quota-Limit", strconv.FormatInt(cap, 10))
	h.Set("X-Quota-Remaining", strconv.FormatInt(remaining, 10))
	h.Set("X-Quota-Reset", strconv.FormatInt(resetAt.Unix(), 10))
}
