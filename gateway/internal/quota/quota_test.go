package quota

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
				ok, _, err := tr.Reserve(context.Background(), cust, tt.cap)
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
				ok, _, err := tr.Reserve(context.Background(), cust, tt.cap)
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

	_, _, err := tr.Reserve(context.Background(), cust, 10)
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

	now := time.Date(2026, 5, 31, 23, 59, 0, 0, time.UTC)
	key := monthKey(cust, now)
	t.Cleanup(func() { rdb.Del(context.Background(), key) })

	ok, reservedKey, err := tr.Reserve(context.Background(), cust, 10)
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
		ok, _, err := tr.Reserve(context.Background(), cust, planCap)
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
}
