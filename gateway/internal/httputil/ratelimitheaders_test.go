package httputil

import (
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestSetRateLimitHeaders_SetsAllSixHeaders(t *testing.T) {
	w := httptest.NewRecorder()
	limit := 100
	remaining := 42
	resetAt := time.Unix(1700000000, 0)

	SetRateLimitHeaders(w, limit, remaining, resetAt)

	h := w.Header()
	tests := [][2]string{
		{"RateLimit-Limit", "100"},
		{"RateLimit-Remaining", "42"},
		{"RateLimit-Reset", "1700000000"},
		{"X-RateLimit-Limit", "100"},
		{"X-RateLimit-Remaining", "42"},
		{"X-RateLimit-Reset", "1700000000"},
	}
	for _, pair := range tests {
		if got := h.Get(pair[0]); got != pair[1] {
			t.Errorf("header %s = %q, want %q", pair[0], got, pair[1])
		}
	}
}

func TestSetRateLimitHeaders_RemainingZero(t *testing.T) {
	w := httptest.NewRecorder()
	SetRateLimitHeaders(w, 60, 0, time.Now().Add(60*time.Second))

	if got := w.Header().Get("RateLimit-Remaining"); got != "0" {
		t.Errorf("RateLimit-Remaining = %q, want 0", got)
	}
	if got := w.Header().Get("X-RateLimit-Remaining"); got != "0" {
		t.Errorf("X-RateLimit-Remaining = %q, want 0", got)
	}
}

func TestSetRateLimitHeaders_ResetIsPositiveUnixTimestamp(t *testing.T) {
	w := httptest.NewRecorder()
	resetAt := time.Now().Add(60 * time.Second)
	SetRateLimitHeaders(w, 100, 50, resetAt)

	for _, hdr := range []string{"RateLimit-Reset", "X-RateLimit-Reset"} {
		v := w.Header().Get(hdr)
		ts, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			t.Errorf("header %s = %q: not a valid integer", hdr, v)
			continue
		}
		if ts <= 0 {
			t.Errorf("header %s = %d: must be a positive Unix timestamp", hdr, ts)
		}
		if ts != resetAt.Unix() {
			t.Errorf("header %s = %d, want %d", hdr, ts, resetAt.Unix())
		}
	}
}

func TestSetRateLimitHeaders_AliasesMatchCanonicals(t *testing.T) {
	w := httptest.NewRecorder()
	SetRateLimitHeaders(w, 200, 77, time.Unix(1800000000, 0))

	h := w.Header()
	pairs := [][2]string{
		{"RateLimit-Limit", "X-RateLimit-Limit"},
		{"RateLimit-Remaining", "X-RateLimit-Remaining"},
		{"RateLimit-Reset", "X-RateLimit-Reset"},
	}
	for _, p := range pairs {
		canon := h.Get(p[0])
		alias := h.Get(p[1])
		if canon == "" {
			t.Errorf("canonical header %s is empty", p[0])
		}
		if canon != alias {
			t.Errorf("%s = %q, %s = %q (aliases must match)", p[0], canon, p[1], alias)
		}
	}
}

func TestSetQuotaHeaders_SetsAllThreeHeaders(t *testing.T) {
	w := httptest.NewRecorder()
	cap := int64(1000)
	remaining := int64(750)
	resetAt := time.Unix(1700000000, 0)

	SetQuotaHeaders(w, cap, remaining, resetAt)

	h := w.Header()
	tests := [][2]string{
		{"X-Quota-Limit", "1000"},
		{"X-Quota-Remaining", "750"},
		{"X-Quota-Reset", "1700000000"},
	}
	for _, pair := range tests {
		if got := h.Get(pair[0]); got != pair[1] {
			t.Errorf("header %s = %q, want %q", pair[0], got, pair[1])
		}
	}
}

func TestSetQuotaHeaders_RemainingZero(t *testing.T) {
	w := httptest.NewRecorder()
	SetQuotaHeaders(w, 1000, 0, time.Now().Add(24*time.Hour))

	if got := w.Header().Get("X-Quota-Remaining"); got != "0" {
		t.Errorf("X-Quota-Remaining = %q, want 0", got)
	}
}
