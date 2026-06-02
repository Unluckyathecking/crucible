package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	"github.com/Unluckyathecking/crucible/gateway/internal/billing"
)

// testKeyForPlan builds a minimal auth.Key for middleware tests.
func testKeyForPlan(planID string) *auth.Key {
	cust := uuid.New()
	return &auth.Key{
		ID: uuid.New(),
		Customer: auth.Customer{
			ID:    cust,
			Email: fmt.Sprintf("test-%s@example.com", cust.String()[:8]),
			Plan:  planID,
		},
	}
}

// ---------------------------------------------------------------------------
// Tracker.Add — 0 % covered before this file.
// ---------------------------------------------------------------------------

// TestTrackerAdd_TableDriven verifies that Add() increments the monthly counter by
// the given number of units using a Redis pipeline and sets a month-end expiry.
func TestTrackerAdd_TableDriven(t *testing.T) {
	rdb := newTestRedis(t)
	tr := New(rdb)

	tests := []struct {
		name     string
		reserves int64  // pre-fill via Reserve to establish a baseline
		addUnits uint64 // units passed to Add
		wantCnt  int64  // expected counter after Add
	}{
		{
			name:     "add to zero baseline",
			reserves: 0,
			addUnits: 5,
			wantCnt:  5,
		},
		{
			name:     "add on top of prior reserves",
			reserves: 3,
			addUnits: 7,
			wantCnt:  10,
		},
		{
			name:     "add zero units is a no-op increment",
			reserves: 2,
			addUnits: 0,
			wantCnt:  2,
		},
		{
			name:     "add large unit count",
			reserves: 1,
			addUnits: 999,
			wantCnt:  1000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cust := uuid.New()
			key := monthKey(cust, time.Now().UTC())
			t.Cleanup(func() { rdb.Del(context.Background(), key) })

			// Pre-fill via Reserve so there's a baseline counter.
			for i := int64(0); i < tt.reserves; i++ {
				ok, _, err := tr.Reserve(context.Background(), cust, 9999)
				if err != nil || !ok {
					t.Fatalf("pre-fill reserve %d: ok=%v err=%v", i, ok, err)
				}
			}

			if err := tr.Add(context.Background(), cust, tt.addUnits); err != nil {
				t.Fatalf("Add: %v", err)
			}

			got, err := tr.Current(context.Background(), cust)
			if err != nil {
				t.Fatalf("Current: %v", err)
			}
			if int64(got) != tt.wantCnt {
				t.Errorf("counter = %d, want %d", got, tt.wantCnt)
			}

			// Expiry must be set to the month-end sentinel (first day of next month + 1d buffer).
			ttl := rdb.TTL(context.Background(), key).Val()
			if ttl <= 0 {
				t.Errorf("Add must set a positive TTL on the key; got %v", ttl)
			}
		})
	}
}

// TestTrackerAdd_SetsExpiry verifies that Add always refreshes the month-end EXPIREAT,
// even on a key that has no pre-existing expiry (first write of the month).
func TestTrackerAdd_SetsExpiry(t *testing.T) {
	rdb := newTestRedis(t)
	tr := New(rdb)

	cust := uuid.New()
	key := monthKey(cust, time.Now().UTC())
	t.Cleanup(func() { rdb.Del(context.Background(), key) })

	if err := tr.Add(context.Background(), cust, 3); err != nil {
		t.Fatalf("Add: %v", err)
	}

	ttl := rdb.TTL(context.Background(), key).Val()
	if ttl <= 0 {
		t.Errorf("TTL after Add = %v, want positive (month-end expiry)", ttl)
	}
	// Should be no more than ~32 days away.
	if ttl > 33*24*time.Hour {
		t.Errorf("TTL = %v, unexpectedly large (> 33 days)", ttl)
	}
}

// TestTrackerAdd_IsolatedKey verifies that Add uses a per-customer-per-month key
// (not a shared key), so two customers' Add calls don't bleed into each other.
func TestTrackerAdd_IsolatedKey(t *testing.T) {
	rdb := newTestRedis(t)
	tr := New(rdb)

	custA, custB := uuid.New(), uuid.New()
	keyA := monthKey(custA, time.Now().UTC())
	keyB := monthKey(custB, time.Now().UTC())
	t.Cleanup(func() {
		rdb.Del(context.Background(), keyA, keyB)
	})

	if err := tr.Add(context.Background(), custA, 10); err != nil {
		t.Fatalf("Add custA: %v", err)
	}
	if err := tr.Add(context.Background(), custB, 25); err != nil {
		t.Fatalf("Add custB: %v", err)
	}

	a, _ := tr.Current(context.Background(), custA)
	b, _ := tr.Current(context.Background(), custB)
	if a != 10 {
		t.Errorf("custA counter = %d, want 10", a)
	}
	if b != 25 {
		t.Errorf("custB counter = %d, want 25", b)
	}
}

// ---------------------------------------------------------------------------
// Middleware refund path (backgroundRefund) — 0 % covered before this file.
//
// The middleware plants a record-signal before calling the inner handler. If the
// handler does NOT call MarkRecorded (worker failed, returned an error envelope,
// recorder write failed), the middleware triggers backgroundRefund which calls
// RefundAt on the exact key Reserve returned.
// ---------------------------------------------------------------------------

// TestMiddleware_RefundsWhenNoUsageRecorded is the core test for the backgroundRefund
// path. The handler is called (admitted) but never calls MarkRecorded, simulating a
// worker failure or error-envelope response. The middleware must refund the reserve so
// the customer's monthly counter nets to zero.
func TestMiddleware_RefundsWhenNoUsageRecorded(t *testing.T) {
	rdb := newTestRedis(t)
	pool := newTestPool(t)
	plans := billing.NewPlanCache(pool)

	tr := New(rdb)
	key := testKeyForPlan("free")
	cust := key.Customer

	redisKey := monthKey(cust.ID, time.Now().UTC())
	t.Cleanup(func() { rdb.Del(context.Background(), redisKey) })

	// Handler runs but deliberately does NOT call MarkRecorded — simulates worker failure.
	handler := Middleware(tr, plans)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	ctx := auth.WithTestKey(context.Background(), key)
	req := httptest.NewRequest("POST", "/", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// backgroundRefund is called synchronously at the tail of the HandlerFunc, so by
	// the time ServeHTTP returns the refund has completed.

	// Counter must be zero: Reserve incremented it, backgroundRefund decremented it back.
	got, err := tr.Current(context.Background(), cust.ID)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if got != 0 {
		t.Errorf("counter after reserve+refund = %d, want 0 (refund must cancel the reserve on worker failure)", got)
	}
}

// TestMiddleware_NoRefundWhenUsageRecorded verifies the OPPOSITE case: when the handler
// calls MarkRecorded (usage persisted to DB), the middleware does NOT refund, so the
// counter retains the +1 from the reserve.
func TestMiddleware_NoRefundWhenUsageRecorded(t *testing.T) {
	rdb := newTestRedis(t)
	pool := newTestPool(t)
	plans := billing.NewPlanCache(pool)

	tr := New(rdb)
	key := testKeyForPlan("free")
	cust := key.Customer

	redisKey := monthKey(cust.ID, time.Now().UTC())
	t.Cleanup(func() { rdb.Del(context.Background(), redisKey) })

	// Handler succeeds and marks usage as recorded.
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

	// Counter must be 1: Reserve incremented it and the refund must NOT have run.
	got, err := tr.Current(context.Background(), cust.ID)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if got != 1 {
		t.Errorf("counter = %d, want 1 (reserve must stay when usage was recorded)", got)
	}
}

// TestMiddleware_QuotaExceeded_TableDriven is a table-driven 429 test. It pre-fills
// the counter to capacity via direct Reserve calls, then fires a request through the
// middleware and asserts the response shape. Covers the response body in addition to
// the status code.
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
