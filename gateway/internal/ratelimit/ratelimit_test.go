package ratelimit

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
				err := b.Allow(context.Background(), cust, tt.limit)
				if !errors.Is(err, tt.wantErr[i]) {
					t.Errorf("call %d: err=%v, want %v", i, err, tt.wantErr[i])
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

	err := b.Allow(context.Background(), cust, 5)
	if err != nil {
		t.Errorf("Allow should return nil on Redis error (fail-open), got %v", err)
	}

	after := testutil.ToFloat64(observability.RateLimitFailOpenTotal)
	if after != before+1 {
		t.Errorf("crucible_ratelimit_failopen_total = %v, want %v (must increment on Redis-error fail-open)", after, before+1)
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

	t.Run("under limit returns ok with no rate-limit headers", func(t *testing.T) {
		ctx := auth.WithTestKey(context.Background(), key)
		req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if v := rec.Header().Get("Retry-After"); v != "" {
			t.Errorf("Retry-After should not be set on success, got %q", v)
		}
	})

	t.Run("over limit returns 429 with Retry-After header", func(t *testing.T) {
		rdb.Del(context.Background(), "rl:"+custStr)

		planLimit := plans.RatePerMinute(context.Background(), "free")
		for i := 0; i < planLimit; i++ {
			if err := bucket.Allow(context.Background(), custStr, planLimit); err != nil {
				t.Fatalf("pre-fill call %d: %v", i, err)
			}
		}

		ctx := auth.WithTestKey(context.Background(), key)
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
	})
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
