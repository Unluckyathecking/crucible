package ratelimit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	"github.com/Unluckyathecking/crucible/gateway/internal/billing"
	mwpkg "github.com/Unluckyathecking/crucible/gateway/internal/middleware"
	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
)

func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, "postgres://crucible@localhost:5432/crucible?sslmode=disable")
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("postgres ping failed: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestAllow_TableDriven(t *testing.T) {
	rdb := newTestRedis(t)

	tests := []struct {
		name    string
		limit   int
		calls   int
		wantErr []error
	}{
		{
			name:    "all under limit",
			limit:   10,
			calls:   5,
			wantErr: []error{nil, nil, nil, nil, nil},
		},
		{
			name:    "at limit rejects",
			limit:   3,
			calls:   5,
			wantErr: []error{nil, nil, nil, ErrLimited, ErrLimited},
		},
		{
			name:    "limit of one",
			limit:   1,
			calls:   3,
			wantErr: []error{nil, ErrLimited, ErrLimited},
		},
		{
			name:    "zero limit allows all",
			limit:   0,
			calls:   5,
			wantErr: []error{nil, nil, nil, nil, nil},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cust := fmt.Sprintf("td-%s-%d", tt.name, time.Now().UnixNano())
			key := "rl:" + cust
			t.Cleanup(func() { rdb.Del(context.Background(), key) })

			b := New(rdb)
			for i := 0; i < tt.calls; i++ {
				rem, err := b.Allow(context.Background(), cust, tt.limit)
				if !errors.Is(err, tt.wantErr[i]) {
					t.Errorf("call %d: err=%v, want %v", i, err, tt.wantErr[i])
				}
				// Unlimited plans (limit==0) must return the noRemaining sentinel so
				// callers know not to emit rate-limit headers.
				if tt.limit == 0 && rem != noRemaining {
					t.Errorf("call %d: unlimited plan should return noRemaining (%d), got %d", i, noRemaining, rem)
				}
			}
		})
	}
}

func TestBucket_FailOpenOnRedisError(t *testing.T) {
	rdb := newTestRedis(t)
	rdb.Close()

	b := New(rdb)
	cust := fmt.Sprintf("failopen-%d", time.Now().UnixNano())

	before := testutil.ToFloat64(observability.RateLimitFailOpenTotal)

	rem, err := b.Allow(context.Background(), cust, 5)
	if err != nil {
		t.Errorf("Allow should return nil on Redis error (fail-open), got %v", err)
	}
	if rem != noRemaining {
		t.Errorf("Allow on Redis error should return noRemaining sentinel, got %d", rem)
	}

	after := testutil.ToFloat64(observability.RateLimitFailOpenTotal)
	if after != before+1 {
		t.Errorf("crucible_ratelimit_failopen_total = %v, want %v (must increment on Redis-error fail-open)", after, before+1)
	}
}

// checkRateLimitHeaders asserts all six RateLimit-* / X-RateLimit-* headers are present
// and consistent with the expected limit and remaining values.
func checkRateLimitHeaders(t *testing.T, h http.Header, wantLimit, wantRemaining int) {
	t.Helper()
	for _, pair := range [][2]string{
		{"RateLimit-Limit", strconv.Itoa(wantLimit)},
		{"RateLimit-Remaining", strconv.Itoa(wantRemaining)},
		{"X-RateLimit-Limit", strconv.Itoa(wantLimit)},
		{"X-RateLimit-Remaining", strconv.Itoa(wantRemaining)},
	} {
		if got := h.Get(pair[0]); got != pair[1] {
			t.Errorf("header %s = %q, want %q", pair[0], got, pair[1])
		}
	}
	// Reset headers must be set and look like a Unix timestamp (positive integer).
	for _, hdr := range []string{"RateLimit-Reset", "X-RateLimit-Reset"} {
		v := h.Get(hdr)
		if v == "" {
			t.Errorf("header %s is missing", hdr)
			continue
		}
		ts, err := strconv.ParseInt(v, 10, 64)
		if err != nil || ts <= 0 {
			t.Errorf("header %s = %q, want a positive Unix timestamp", hdr, v)
		}
	}
}

func TestMiddleware_EmitsRateLimitHeaders(t *testing.T) {
	rdb := newTestRedis(t)
	pool := newTestPool(t)
	plans := billing.NewPlanCache(pool)

	bucket := New(rdb)

	custID := uuid.New()
	custStr := custID.String()
	key := &auth.Key{
		ID: uuid.New(),
		Customer: auth.Customer{
			ID:    custID,
			Email: fmt.Sprintf("test-%s@example.com", custStr[:8]),
			Plan:  "free",
		},
	}
	t.Cleanup(func() { rdb.Del(context.Background(), "rl:"+custStr) })

	handler := Middleware(bucket, plans)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	t.Run("under limit returns ok with rate-limit headers", func(t *testing.T) {
		rdb.Del(context.Background(), "rl:"+custStr)
		ctx := auth.WithTestKey(context.Background(), key)
		req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		limit := plans.RatePerMinute(context.Background(), "free")
		checkRateLimitHeaders(t, rec.Header(), limit, limit-1)

		if v := rec.Header().Get("Retry-After"); v != "" {
			t.Errorf("Retry-After should not be set on success, got %q", v)
		}
	})

	t.Run("over limit returns 429 with rate-limit headers Remaining=0", func(t *testing.T) {
		rdb.Del(context.Background(), "rl:"+custStr)

		planLimit := plans.RatePerMinute(context.Background(), "free")
		for i := 0; i < planLimit; i++ {
			if _, err := bucket.Allow(context.Background(), custStr, planLimit); err != nil {
				t.Fatalf("pre-fill call %d: %v", i, err)
			}
		}

		const testRID = "test-rid-ratelimit-429"
		ctx := context.WithValue(
			auth.WithTestKey(context.Background(), key),
			mwpkg.RequestIDKey,
			testRID,
		)
		req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusTooManyRequests {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
		}
		if v := rec.Header().Get("Retry-After"); v != "60" {
			t.Errorf("Retry-After = %q, want %q", v, "60")
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		checkRateLimitHeaders(t, rec.Header(), planLimit, 0)

		// Verify the 429 body is the canonical four-field error envelope.
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("429 body not valid JSON: %v", err)
		}
		var errObj struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable bool   `json:"retryable"`
			RequestID string `json:"request_id"`
		}
		if err := json.Unmarshal(envelope["error"], &errObj); err != nil {
			t.Fatalf("error object decode failed: %v", err)
		}
		if errObj.Code != "RATE_LIMITED" {
			t.Errorf("error.code = %q, want RATE_LIMITED", errObj.Code)
		}
		if errObj.Message == "" {
			t.Error("error.message must not be empty")
		}
		if !errObj.Retryable {
			t.Error("error.retryable = false, want true; rate-limit 429 must be retryable")
		}
		if errObj.RequestID != testRID {
			t.Errorf("error.request_id = %q, want %q", errObj.RequestID, testRID)
		}
	})
}

// TestMiddleware_UnlimitedPlanNoRateLimitHeaders asserts that unlimited plans
// (RatePerMinute <= 0) do not emit misleading rate-limit headers.
func TestMiddleware_UnlimitedPlanNoRateLimitHeaders(t *testing.T) {
	rdb := newTestRedis(t)
	pool := newTestPool(t)

	// Seed a test-specific plan with rate_limit_per_minute=0 (unlimited).
	// The built-in "business" plan is seeded with rate=6000, not 0.
	_, err := pool.Exec(context.Background(), `
		INSERT INTO plans (id, display_name, stripe_price_id, rate_limit_per_minute, monthly_unit_cap)
		VALUES ('test-unlimited-rl', 'Test Unlimited RL', NULL, 0, NULL)
		ON CONFLICT (id) DO UPDATE SET rate_limit_per_minute = 0
	`)
	if err != nil {
		t.Skipf("cannot seed test plan: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DELETE FROM plans WHERE id = 'test-unlimited-rl'")
	})

	plans := billing.NewPlanCache(pool)
	bucket := New(rdb)

	key := &auth.Key{
		ID: uuid.New(),
		Customer: auth.Customer{
			ID:    uuid.New(),
			Email: "test-unlimited@example.com",
			Plan:  "test-unlimited-rl",
		},
	}

	handler := Middleware(bucket, plans)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	ctx := auth.WithTestKey(context.Background(), key)
	req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	for _, hdr := range []string{
		"RateLimit-Limit", "RateLimit-Remaining", "RateLimit-Reset",
		"X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset",
	} {
		if v := rec.Header().Get(hdr); v != "" {
			t.Errorf("unlimited plan: header %s = %q, want empty (must not emit misleading cap)", hdr, v)
		}
	}
}

// TestMiddleware_RedisDownNoFabricatedHeaders asserts that fail-open (Redis error)
// passes the request through without emitting rate-limit headers.
func TestMiddleware_RedisDownNoFabricatedHeaders(t *testing.T) {
	rdb := newTestRedis(t)
	pool := newTestPool(t)
	plans := billing.NewPlanCache(pool)

	bucket := New(rdb)
	rdb.Close() // force Redis error

	key := &auth.Key{
		ID: uuid.New(),
		Customer: auth.Customer{
			ID:    uuid.New(),
			Email: "test-redisdown@example.com",
			Plan:  "free",
		},
	}

	called := false
	handler := Middleware(bucket, plans)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	ctx := auth.WithTestKey(context.Background(), key)
	req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("handler should be called on Redis error (fail-open)")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (fail-open on Redis error)", rec.Code, http.StatusOK)
	}
	for _, hdr := range []string{
		"RateLimit-Limit", "RateLimit-Remaining", "RateLimit-Reset",
		"X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset",
	} {
		if v := rec.Header().Get(hdr); v != "" {
			t.Errorf("Redis-down path: header %s = %q, want empty (must not fabricate values)", hdr, v)
		}
	}
}

func TestMiddleware_FailOpenOnRedisError(t *testing.T) {
	rdb := newTestRedis(t)
	pool := newTestPool(t)
	plans := billing.NewPlanCache(pool)

	bucket := New(rdb)

	key := &auth.Key{
		ID: uuid.New(),
		Customer: auth.Customer{
			ID:    uuid.New(),
			Email: "test-failopen@example.com",
			Plan:  "free",
		},
	}

	rdb.Close()

	called := false
	handler := Middleware(bucket, plans)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	ctx := auth.WithTestKey(context.Background(), key)
	req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("handler should be called on Redis error (fail-open)")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (fail-open on Redis error)", rec.Code, http.StatusOK)
	}
}

// TestRetryAfterMatchesWindowSeconds guards the invariant that retryAfterSeconds == windowSeconds.
// Both constants represent the same 60 s fixed window; divergence would send clients a Retry-After
// value that does not match the actual window expiry, making retry guidance incorrect.
// The const retryAfterSeconds = windowSeconds assignment already guarantees this at compile time,
// but an explicit test makes the coupling visible and prevents future refactors from breaking it.
func TestRetryAfterMatchesWindowSeconds(t *testing.T) {
	if retryAfterSeconds != windowSeconds {
		t.Errorf("retryAfterSeconds (%d) != windowSeconds (%d); Retry-After must match the actual window", retryAfterSeconds, windowSeconds)
	}
}

// TestAllow_BoundaryBurstCapped is the proof-of-correctness for the sliding-window
// design. A fixed-minute bucket resets its counter at each clock minute, so a customer
// can exhaust `limit` calls at second 59 and another `limit` at second 61 — 2× the
// advertised cap across the boundary. The sliding window counts the last 60 s exactly:
// prior entries that are still within 60 s continue to occupy slots, and no doubling
// is possible.
//
// The test seeds the Redis sorted set directly (bypassing Allow) to place `limit`
// entries 30 s in the past — within the 60 s sliding window but in what a fixed-minute
// bucket would consider the "previous" minute (a new fixed minute started ~30 s ago).
// Under a fixed-window implementation the current-minute counter would be zero and all
// subsequent Allow calls would succeed, yielding 2× the limit. Under the sliding window
// those 30-s-old entries still occupy their slots and every subsequent call must be
// rejected.
func TestAllow_BoundaryBurstCapped(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	cust := fmt.Sprintf("test-boundary-%d", time.Now().UnixNano())
	key := "rl:" + cust
	defer rdb.Del(ctx, key)

	const limit = 5
	b := New(rdb)

	// Seed `limit` entries at -30 s: within the 60 s sliding window, but 30 s
	// into what a fixed-minute bucket would call the "previous" minute.
	thirtySecondsAgo := time.Now().Add(-30 * time.Second).UnixMilli()
	for i := 0; i < limit; i++ {
		if err := rdb.ZAdd(ctx, key, redis.Z{
			Score:  float64(thirtySecondsAgo),
			Member: fmt.Sprintf("seed-%d", i),
		}).Err(); err != nil {
			t.Fatalf("seeding sorted set entry %d: %v", i, err)
		}
	}

	// All subsequent Allow calls must be rejected: the sliding window still counts
	// those 5 entries within the last 60 s and the cap is already hit.
	for i := 0; i < 3; i++ {
		_, err := b.Allow(ctx, cust, limit)
		if !errors.Is(err, ErrLimited) {
			t.Errorf("call %d: got %v, want ErrLimited — 30-s-old entries must still occupy window slots in a sliding window (a fixed-window would incorrectly allow 2× limit across minute boundaries)", i, err)
		}
	}
}
