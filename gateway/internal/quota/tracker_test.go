package quota

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func newTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		t.Skipf("redis unavailable on localhost:6379, skipping: %v", err)
	}
	return c
}

func TestReserve_BelowCapAdmits(t *testing.T) {
	rdb := newTestRedis(t)
	cust := uuid.New()
	defer rdb.Del(context.Background(), monthKey(cust, time.Now()))

	tr := New(rdb)
	for i := 0; i < 5; i++ {
		ok, _, err := tr.Reserve(context.Background(), cust, 10)
		if err != nil || !ok {
			t.Fatalf("call %d: ok=%v err=%v", i, ok, err)
		}
	}
}

func TestReserve_OverCapRejects(t *testing.T) {
	rdb := newTestRedis(t)
	cust := uuid.New()
	defer rdb.Del(context.Background(), monthKey(cust, time.Now()))

	tr := New(rdb)
	const cap = 3
	for i := 0; i < cap; i++ {
		ok, _, _ := tr.Reserve(context.Background(), cust, cap)
		if !ok {
			t.Fatalf("call %d should admit", i)
		}
	}
	ok, _, _ := tr.Reserve(context.Background(), cust, cap)
	if ok {
		t.Error("call past cap should reject")
	}
	// Counter must equal cap (not cap+1) — rejected reserves roll back.
	v, _ := tr.Current(context.Background(), cust)
	if int64(v) != cap {
		t.Errorf("counter = %d, want %d (rejection should have rolled back its increment)", v, cap)
	}
}

// The whole motivation for the fix: stampede must not overshoot the cap.
// 100 concurrent goroutines hit a cap of 10 — exactly 10 must be admitted.
func TestReserve_NoStampedeOvershoot(t *testing.T) {
	rdb := newTestRedis(t)
	cust := uuid.New()
	defer rdb.Del(context.Background(), monthKey(cust, time.Now()))

	tr := New(rdb)
	const cap = 10
	const concurrent = 100

	var admitted atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, _, _ := tr.Reserve(context.Background(), cust, cap)
			if ok {
				admitted.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	got := admitted.Load()
	if got != cap {
		t.Errorf("admitted=%d under stampede, want exactly %d — soft-overshoot regression", got, cap)
	}
	v, _ := tr.Current(context.Background(), cust)
	if int64(v) != cap {
		t.Errorf("counter = %d after stampede, want %d", v, cap)
	}
}

func TestReserve_CapZeroMeansUnlimited(t *testing.T) {
	rdb := newTestRedis(t)
	cust := uuid.New()

	tr := New(rdb)
	for i := 0; i < 50; i++ {
		ok, _, err := tr.Reserve(context.Background(), cust, 0)
		if err != nil || !ok {
			t.Fatalf("call %d: unlimited cap should always admit (ok=%v err=%v)", i, ok, err)
		}
	}
	// Should not have touched the counter.
	if _, err := rdb.Get(context.Background(), monthKey(cust, time.Now())).Result(); err == nil {
		t.Errorf("counter key should not exist for unlimited cap")
	}
}

func TestReserve_PerCustomerIsolation(t *testing.T) {
	rdb := newTestRedis(t)
	a, b := uuid.New(), uuid.New()
	defer rdb.Del(context.Background(),
		monthKey(a, time.Now()),
		monthKey(b, time.Now()),
	)

	tr := New(rdb)
	const cap = 3
	for i := 0; i < cap; i++ {
		ok, _, _ := tr.Reserve(context.Background(), a, cap)
		if !ok {
			t.Fatalf("a call %d should admit", i)
		}
	}
	// Customer A is exhausted; customer B should be untouched.
	ok, _, _ := tr.Reserve(context.Background(), b, cap)
	if !ok {
		t.Error("customer B should be admitted independently of A")
	}
}

func TestReserve_MultipleCustomersConcurrent(t *testing.T) {
	rdb := newTestRedis(t)
	custs := make([]uuid.UUID, 5)
	for i := range custs {
		custs[i] = uuid.New()
	}
	defer func() {
		for _, c := range custs {
			rdb.Del(context.Background(), monthKey(c, time.Now()))
		}
	}()

	tr := New(rdb)
	const capPerCust = 20
	const callsPerCust = 50

	var wg sync.WaitGroup
	admittedPerCust := make([]atomic.Int64, len(custs))
	for ci, cust := range custs {
		for j := 0; j < callsPerCust; j++ {
			wg.Add(1)
			go func(ci int, cust uuid.UUID) {
				defer wg.Done()
				ok, _, _ := tr.Reserve(context.Background(), cust, capPerCust)
				if ok {
					admittedPerCust[ci].Add(1)
				}
			}(ci, cust)
		}
	}
	wg.Wait()

	for ci := range custs {
		got := admittedPerCust[ci].Load()
		if got != capPerCust {
			t.Errorf("customer %d: admitted=%d, want exactly %d", ci, got, capPerCust)
		}
	}
}

// Sanity: keys carry the right shape — "quota:<uuid>:<YYYY-MM>" with UTC month.
func TestMonthKey_Shape(t *testing.T) {
	c := uuid.New()
	got := monthKey(c, time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC))
	want := fmt.Sprintf("quota:%s:2026-05", c)
	if got != want {
		t.Errorf("monthKey = %q, want %q", got, want)
	}
}

// Refund rolls back a previously-reserved slot — used by the middleware when a
// request didn't produce billable usage (worker failure, contract reject, bad request).
// Without this the cap would slowly drain even for failed requests, exhausting a
// customer's quota without any usage_events row to bill.
func TestRefund_DecrementsCounter(t *testing.T) {
	rdb := newTestRedis(t)
	cust := uuid.New()
	defer rdb.Del(context.Background(), monthKey(cust, time.Now().UTC()))

	tr := New(rdb)
	const cap = 10

	// Reserve a few slots.
	for i := 0; i < 3; i++ {
		ok, _, _ := tr.Reserve(context.Background(), cust, cap)
		if !ok {
			t.Fatalf("reserve %d should admit", i)
		}
	}
	pre, _ := tr.Current(context.Background(), cust)
	if pre != 3 {
		t.Fatalf("counter after 3 reserves = %d, want 3", pre)
	}

	// Refund one — simulates a failed worker call.
	if err := tr.RefundAt(context.Background(), monthKey(cust, time.Now().UTC())); err != nil {
		t.Fatalf("Refund: %v", err)
	}
	post, _ := tr.Current(context.Background(), cust)
	if post != 2 {
		t.Errorf("counter after refund = %d, want 2", post)
	}
}

// Refund on an empty (missing or zero) counter is a no-op — protects against a
// refund firing after the month has rolled over and the key has expired.
func TestRefund_NeverNegative(t *testing.T) {
	rdb := newTestRedis(t)
	cust := uuid.New()
	defer rdb.Del(context.Background(), monthKey(cust, time.Now().UTC()))

	tr := New(rdb)

	// No prior reserves. Refund should not put the counter into negative territory.
	for i := 0; i < 5; i++ {
		if err := tr.RefundAt(context.Background(), monthKey(cust, time.Now().UTC())); err != nil {
			t.Fatalf("refund %d failed: %v", i, err)
		}
	}
	v, _ := tr.Current(context.Background(), cust)
	if int64(v) < 0 {
		t.Errorf("counter went negative: %d", v)
	}
	// uint64 doesn't really go negative — but the underlying Redis value could.
	raw, err := rdb.Get(context.Background(), monthKey(cust, time.Now().UTC())).Int64()
	if err == nil && raw < 0 {
		t.Errorf("raw redis counter went negative: %d", raw)
	}
}

// Reserve + Refund should net to zero — verifies the "failed request doesn't burn quota"
// guarantee that motivated the PR #5 P1 fix.
func TestReserveThenRefund_NetsToZero(t *testing.T) {
	rdb := newTestRedis(t)
	cust := uuid.New()
	defer rdb.Del(context.Background(), monthKey(cust, time.Now().UTC()))

	tr := New(rdb)
	for i := 0; i < 20; i++ {
		ok, _, _ := tr.Reserve(context.Background(), cust, 100)
		if !ok {
			t.Fatalf("admit %d", i)
		}
		if err := tr.RefundAt(context.Background(), monthKey(cust, time.Now().UTC())); err != nil {
			t.Fatalf("refund %d: %v", i, err)
		}
	}
	v, _ := tr.Current(context.Background(), cust)
	if v != 0 {
		t.Errorf("counter after 20 reserve+refund pairs = %d, want 0", v)
	}
}

// Codex P2: monthKey and expireAt must agree on the calendar month even when the
// host runs in a non-UTC timezone. This test pins the issue: Reserve must touch
// the UTC-named key and set expiry in the UTC month.
//
// We can't easily change the runtime TZ inside a test, so we verify by inspecting
// the key the Reserve actually wrote to.
func TestReserve_UsesUTCKey(t *testing.T) {
	rdb := newTestRedis(t)
	cust := uuid.New()
	utcKey := monthKey(cust, time.Now().UTC())
	defer rdb.Del(context.Background(), utcKey)

	tr := New(rdb)
	ok, returnedKey, err := tr.Reserve(context.Background(), cust, 5)
	if err != nil || !ok {
		t.Fatalf("Reserve: ok=%v err=%v", ok, err)
	}
	if returnedKey != utcKey {
		t.Errorf("Reserve returned key %q, want %q", returnedKey, utcKey)
	}
	v, err := rdb.Get(context.Background(), utcKey).Int64()
	if err != nil {
		t.Fatalf("UTC key %q should exist after Reserve: %v", utcKey, err)
	}
	if v != 1 {
		t.Errorf("UTC counter = %d after one Reserve, want 1", v)
	}
}

// Codex P2 (second pass): RefundAt must use the EXACT key returned by Reserve, not
// a freshly-computed one. Verifies a refund against the original key correctly
// decrements that month's counter.
func TestRefundAt_TargetsOriginalReservedKey(t *testing.T) {
	rdb := newTestRedis(t)
	cust := uuid.New()
	defer rdb.Del(context.Background(), monthKey(cust, time.Now().UTC()))

	tr := New(rdb)
	ok, reservedKey, err := tr.Reserve(context.Background(), cust, 10)
	if err != nil || !ok {
		t.Fatalf("Reserve: ok=%v err=%v", ok, err)
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

// RefundAt with an empty key (cap=0 unlimited tier) is a no-op.
func TestRefundAt_EmptyKeyNoOps(t *testing.T) {
	rdb := newTestRedis(t)
	tr := New(rdb)
	if err := tr.RefundAt(context.Background(), ""); err != nil {
		t.Errorf("RefundAt with empty key should be a no-op, got %v", err)
	}
}

// Codex P1 (second pass): the record-signal in context is the bridge from recorder
// to middleware. Defaults to false; MarkRecorded flips it true; called on a context
// without the seeded signal is a safe no-op.
func TestRecordSignal_DefaultsToFalse_MarkRecordedFlips(t *testing.T) {
	ctx, sig := withRecordSignal(context.Background())
	if sig.recorded.Load() {
		t.Error("fresh signal should be false")
	}
	// Safe no-op when context has no signal:
	MarkRecorded(context.Background())
	// Flips when context has signal:
	MarkRecorded(ctx)
	if !sig.recorded.Load() {
		t.Error("MarkRecorded should have flipped the signal to true")
	}
}
