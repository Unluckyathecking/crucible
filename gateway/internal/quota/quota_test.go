package quota

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

func TestReserveN_TableDriven(t *testing.T) {
	rdb := newTestRedis(t)

	tests := []struct {
		name     string
		cap      int64
		reserves int
		wantOK   []bool
		wantCnt  int64
	}{
		{
			name:     "under cap",
			cap:      10,
			reserves: 5,
			wantOK:   []bool{true, true, true, true, true},
			wantCnt:  5,
		},
		{
			name:     "at cap boundary",
			cap:      3,
			reserves: 4,
			wantOK:   []bool{true, true, true, false},
			wantCnt:  3,
		},
		{
			name:     "single reserve under cap",
			cap:      1,
			reserves: 1,
			wantOK:   []bool{true},
			wantCnt:  1,
		},
		{
			name:     "single reserve over cap of zero",
			cap:      5,
			reserves: 6,
			wantOK:   []bool{true, true, true, true, true, false},
			wantCnt:  5,
		},
		{
			name:     "cap of one",
			cap:      1,
			reserves: 3,
			wantOK:   []bool{true, false, false},
			wantCnt:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cust := uuid.New()
			key := monthKey(cust, time.Now())
			t.Cleanup(func() { rdb.Del(context.Background(), key) })

			tr := New(rdb)
			for i := 0; i < tt.reserves; i++ {
				ok, _, _, _, err := tr.Reserve(context.Background(), cust, tt.cap)
				if err != nil {
					t.Fatalf("call %d: %v", i, err)
				}
				if ok != tt.wantOK[i] {
					t.Errorf("call %d: ok=%v, want %v", i, ok, tt.wantOK[i])
				}
			}
			v, _ := tr.Current(context.Background(), cust)
			if int64(v) != tt.wantCnt {
				t.Errorf("counter = %d, want %d", v, tt.wantCnt)
			}
		})
	}
}

func TestRefundN_TableDriven(t *testing.T) {
	rdb := newTestRedis(t)

	tests := []struct {
		name     string
		cap      int64
		reserves int
		refunds  int
		wantCnt  int64
	}{
		{
			name:     "refund returns single unit",
			cap:      10,
			reserves: 3,
			refunds:  1,
			wantCnt:  2,
		},
		{
			name:     "refund returns all reserved units",
			cap:      10,
			reserves: 5,
			refunds:  5,
			wantCnt:  0,
		},
		{
			name:     "refund more than reserved is safe",
			cap:      10,
			reserves: 2,
			refunds:  3,
			wantCnt:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cust := uuid.New()
			key := monthKey(cust, time.Now())
			t.Cleanup(func() { rdb.Del(context.Background(), key) })

			tr := New(rdb)
			for i := 0; i < tt.reserves; i++ {
				ok, _, _, _, err := tr.Reserve(context.Background(), cust, tt.cap)
				if err != nil || !ok {
					t.Fatalf("reserve %d: ok=%v err=%v", i, ok, err)
				}
			}
			for i := 0; i < tt.refunds; i++ {
				if err := tr.RefundAt(context.Background(), key); err != nil {
					t.Fatalf("refund %d: %v", i, err)
				}
			}
			v, _ := tr.Current(context.Background(), cust)
			if int64(v) != tt.wantCnt {
				t.Errorf("counter = %d, want %d", v, tt.wantCnt)
			}
		})
	}
}

func TestTracker_FailOpenOnRedisError(t *testing.T) {
	rdb := newTestRedis(t)
	rdb.Close()

	tr := New(rdb)
	cust := uuid.New()

	_, _, _, _, err := tr.Reserve(context.Background(), cust, 10)
	if err == nil {
		t.Error("Reserve should return error when Redis is down")
	}
	var redisErr redis.Error
	if !errors.As(err, &redisErr) && err.Error() == "" {
		t.Error("expected a non-nil error from Reserve on Redis failure")
	}
}

func TestRefundAt_MonthBoundary_UsesOriginalKey(t *testing.T) {
	rdb := newTestRedis(t)
	cust := uuid.New()

	tr := New(rdb)

	// Reserve derives the month key from time.Now() internally, so the expected
	// key must be computed from the same clock — a hardcoded date breaks the test
	// at every month rollover. The invariant under test is that RefundAt reuses
	// the key Reserve returned, not which month it is.
	now := time.Now().UTC()
	key := monthKey(cust, now)
	t.Cleanup(func() { rdb.Del(context.Background(), key) })

	ok, reservedKey, _, _, err := tr.Reserve(context.Background(), cust, 10)
	if err != nil || !ok {
		t.Fatalf("Reserve: ok=%v err=%v", ok, err)
	}
	if reservedKey != key {
		t.Errorf("reservedKey = %q, want %q", reservedKey, key)
	}

	v, _ := rdb.Get(context.Background(), reservedKey).Int64()
	if v != 1 {
		t.Fatalf("post-reserve counter = %d, want 1", v)
	}

	if err := tr.RefundAt(context.Background(), reservedKey); err != nil {
		t.Fatalf("RefundAt: %v", err)
	}

	v, _ = rdb.Get(context.Background(), reservedKey).Int64()
	if v != 0 {
		t.Errorf("post-refund counter at %q = %d, want 0", reservedKey, v)
	}
}

func TestQuotaMiddleware_AuthRequiredBeforeQuota(t *testing.T) {
	rdb := newTestRedis(t)
	pool := newTestPool(t)
	plans := billing.NewPlanCache(pool)

	tr := New(rdb)

	called := false
	handler := Middleware(tr, plans)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("POST", "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("middleware should pass through when no auth context (fail-safe for unmounted auth)")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestQuotaMiddleware_UnlimitedPlanSkipsReserve(t *testing.T) {
	rdb := newTestRedis(t)
	pool := newTestPool(t)
	plans := billing.NewPlanCache(pool)

	tr := New(rdb)

	called := false
	handler := Middleware(tr, plans)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	cust := uuid.New()
	key := &auth.Key{
		ID: uuid.New(),
		Customer: auth.Customer{
			ID:    cust,
			Email: fmt.Sprintf("test-%s@example.com", cust.String()[:8]),
			Plan:  "business",
		},
	}
	ctx := auth.WithTestKey(context.Background(), key)
	req := httptest.NewRequest("POST", "/", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("unlimited plan should pass through without Reserve")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	_, err := rdb.Get(context.Background(), monthKey(cust, time.Now())).Result()
	if !errors.Is(err, redis.Nil) {
		t.Errorf("unlimited plan should not touch Redis counter (got key)")
	}
	// Unlimited plan must NOT emit quota headers.
	for _, hdr := range []string{"X-Quota-Limit", "X-Quota-Remaining", "X-Quota-Reset"} {
		if v := rec.Header().Get(hdr); v != "" {
			t.Errorf("unlimited plan: header %s = %q, want empty (must not emit misleading cap)", hdr, v)
		}
	}
}

func TestQuotaMiddleware_RejectsWhenOverCap(t *testing.T) {
	rdb := newTestRedis(t)
	pool := newTestPool(t)
	plans := billing.NewPlanCache(pool)

	tr := New(rdb)

	cust := uuid.New()
	key := &auth.Key{
		ID: uuid.New(),
		Customer: auth.Customer{
			ID:    cust,
			Email: fmt.Sprintf("test-%s@example.com", cust.String()[:8]),
			Plan:  "free",
		},
	}
	t.Cleanup(func() { rdb.Del(context.Background(), monthKey(cust, time.Now())) })

	called := false
	handler := Middleware(tr, plans)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	planCap := plans.MonthlyCap(context.Background(), "free")
	for i := int64(0); i < planCap; i++ {
		ok, _, _, _, err := tr.Reserve(context.Background(), cust, planCap)
		if err != nil || !ok {
			t.Fatalf("pre-fill reserve %d: ok=%v err=%v", i, ok, err)
		}
	}

	ctx := auth.WithTestKey(context.Background(), key)
	req := httptest.NewRequest("POST", "/", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if called {
		t.Error("handler should not be called when quota is exceeded")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	// 429 QUOTA_EXCEEDED must carry X-Quota-* headers with Remaining=0.
	checkQuotaHeaders(t, rec.Header(), planCap, 0)
}

func TestQuotaMiddleware_FailOpenOnRedisError(t *testing.T) {
	rdb := newTestRedis(t)
	pool := newTestPool(t)
	plans := billing.NewPlanCache(pool)

	tr := New(rdb)

	cust := uuid.New()
	key := &auth.Key{
		ID: uuid.New(),
		Customer: auth.Customer{
			ID:    cust,
			Email: fmt.Sprintf("test-%s@example.com", cust.String()[:8]),
			Plan:  "free",
		},
	}

	rdb.Close()

	called := false
	handler := Middleware(tr, plans)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	before := testutil.ToFloat64(observability.QuotaFailOpenTotal)

	ctx := auth.WithTestKey(context.Background(), key)
	req := httptest.NewRequest("POST", "/", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("handler should be called on Redis error (fail-open)")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (fail-open on Redis error)", rec.Code, http.StatusOK)
	}

	after := testutil.ToFloat64(observability.QuotaFailOpenTotal)
	if after != before+1 {
		t.Errorf("crucible_quota_failopen_total = %v, want %v (must increment on Redis-error fail-open)", after, before+1)
	}

	// Fail-open must NOT emit fabricated quota headers.
	for _, hdr := range []string{"X-Quota-Limit", "X-Quota-Remaining", "X-Quota-Reset"} {
		if v := rec.Header().Get(hdr); v != "" {
			t.Errorf("Redis-down path: header %s = %q, want empty (must not fabricate values)", hdr, v)
		}
	}
}

// TestQuotaMiddleware_SuccessEmitsQuotaHeaders checks the success path emits X-Quota-*
// headers derived from the plan cap and live remaining count.
func TestQuotaMiddleware_SuccessEmitsQuotaHeaders(t *testing.T) {
	rdb := newTestRedis(t)
	pool := newTestPool(t)
	plans := billing.NewPlanCache(pool)

	tr := New(rdb)
	key := &auth.Key{
		ID: uuid.New(),
		Customer: auth.Customer{
			ID:    uuid.New(),
			Email: "test-quotahdr@example.com",
			Plan:  "free",
		},
	}
	cust := key.Customer

	redisKey := monthKey(cust.ID, time.Now().UTC())
	t.Cleanup(func() { rdb.Del(context.Background(), redisKey) })

	handler := Middleware(tr, plans)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		MarkRecorded(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	ctx := auth.WithTestKey(context.Background(), key)
	req := httptest.NewRequest("POST", "/", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	planCap := plans.MonthlyCap(context.Background(), "free")
	if planCap == 0 {
		t.Skip("free plan is unlimited in this environment; skipping quota header test")
	}
	// After one successful reserve, remaining = cap - 1.
	checkQuotaHeaders(t, rec.Header(), planCap, planCap-1)
}

// checkQuotaHeaders verifies all three X-Quota-* headers are present and accurate.
func checkQuotaHeaders(t *testing.T, h http.Header, wantCap, wantRemaining int64) {
	t.Helper()
	for _, pair := range [][2]string{
		{"X-Quota-Limit", strconv.FormatInt(wantCap, 10)},
		{"X-Quota-Remaining", strconv.FormatInt(wantRemaining, 10)},
	} {
		if got := h.Get(pair[0]); got != pair[1] {
			t.Errorf("header %s = %q, want %q", pair[0], got, pair[1])
		}
	}
	v := h.Get("X-Quota-Reset")
	if v == "" {
		t.Error("X-Quota-Reset is missing")
		return
	}
	ts, err := strconv.ParseInt(v, 10, 64)
	if err != nil || ts <= 0 {
		t.Errorf("X-Quota-Reset = %q, want a positive Unix timestamp", v)
	}
}

// TestQuotaMiddleware_QuotaExceeded_TableDriven is a table-driven 429 test. It pre-fills
// the counter to capacity via direct Reserve calls, then fires a request through the
// middleware and asserts the response shape and quota headers.
func TestMiddleware_QuotaExceeded_TableDriven(t *testing.T) {
	rdb := newTestRedis(t)
	pool := newTestPool(t)
	plans := billing.NewPlanCache(pool)

	// "free" plan has a known cap from the DB; use the real MonthlyCap call to get it.
	freeCap := plans.MonthlyCap(context.Background(), "free")
	if freeCap == 0 {
		t.Skip("free plan has unlimited cap in this environment; skipping over-cap test")
	}

	tests := []struct {
		name    string
		prefill int64 // reserves to make before the middleware call
		planID  string
	}{
		{
			name:    "exactly at cap returns 429",
			prefill: freeCap,
			planID:  "free",
		},
		{
			name:    "two over cap returns 429",
			prefill: freeCap + 2, // clamped by rollback; counter stays at freeCap
			planID:  "free",
		},
	}

	tr := New(rdb)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := testKeyForPlan(tt.planID)
			cust := key.Customer

			redisKey := monthKey(cust.ID, time.Now().UTC())
			t.Cleanup(func() { rdb.Del(context.Background(), redisKey) })

			// Pre-fill the counter. Once the counter == freeCap the Reserve script rolls
			// back, so the actual counter will be at most freeCap regardless of prefill.
			for i := int64(0); i < tt.prefill; i++ {
				tr.Reserve(context.Background(), cust.ID, freeCap) //nolint:errcheck
			}

			handlerCalled := false
			handler := Middleware(tr, plans)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handlerCalled = true
				w.WriteHeader(http.StatusOK)
			}))

			ctx := auth.WithTestKey(context.Background(), key)
			req := httptest.NewRequest("POST", "/", nil).WithContext(ctx)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if handlerCalled {
				t.Error("handler must not be called when quota is exceeded")
			}
			if rec.Code != http.StatusTooManyRequests {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			// Verify the response body is valid JSON with the expected error code.
			var body struct {
				Error struct {
					Code      string `json:"code"`
					Retryable bool   `json:"retryable"`
				} `json:"error"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("response body is not valid JSON: %v", err)
			}
			if body.Error.Code != "QUOTA_EXCEEDED" {
				t.Errorf("error.code = %q, want QUOTA_EXCEEDED", body.Error.Code)
			}
			if body.Error.Retryable {
				t.Error("error.retryable must be false for quota exhaustion")
			}
			// X-Quota-* headers must be present with Remaining=0.
			checkQuotaHeaders(t, rec.Header(), freeCap, 0)
		})
	}
}

// TestMiddleware_RefundsWhenNoUsageRecorded_Concurrent stress-tests the refund path:
// N concurrent requests, none of which record usage. After all handlers complete, the
// counter must be zero because every reserve was refunded.
func TestMiddleware_RefundsWhenNoUsageRecorded_Concurrent(t *testing.T) {
	rdb := newTestRedis(t)
	pool := newTestPool(t)
	plans := billing.NewPlanCache(pool)

	tr := New(rdb)
	key := testKeyForPlan("free")
	cust := key.Customer

	redisKey := monthKey(cust.ID, time.Now().UTC())
	t.Cleanup(func() { rdb.Del(context.Background(), redisKey) })

	const n = 10

	// Handler always "fails" (no MarkRecorded), but the plan's cap (1000 for free
	// fallback) is large enough that all 10 requests are admitted.
	handler := Middleware(tr, plans)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Never call MarkRecorded — simulate worker failure for every request.
		w.WriteHeader(http.StatusInternalServerError)
	}))

	done := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			ctx := auth.WithTestKey(context.Background(), key)
			req := httptest.NewRequest("POST", "/", nil).WithContext(ctx)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
		}()
	}
	for i := 0; i < n; i++ {
		<-done
	}

	// All reserves were refunded; counter must be zero.
	got, err := tr.Current(context.Background(), cust.ID)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if got != 0 {
		t.Errorf("counter after %d refunded requests = %d, want 0", n, got)
	}
}
