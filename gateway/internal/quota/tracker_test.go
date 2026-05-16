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
		ok, err := tr.Reserve(context.Background(), cust, 10)
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
		ok, _ := tr.Reserve(context.Background(), cust, cap)
		if !ok {
			t.Fatalf("call %d should admit", i)
		}
	}
	ok, _ := tr.Reserve(context.Background(), cust, cap)
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
			ok, _ := tr.Reserve(context.Background(), cust, cap)
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
		ok, err := tr.Reserve(context.Background(), cust, 0)
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
		ok, _ := tr.Reserve(context.Background(), a, cap)
		if !ok {
			t.Fatalf("a call %d should admit", i)
		}
	}
	// Customer A is exhausted; customer B should be untouched.
	ok, _ := tr.Reserve(context.Background(), b, cap)
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
				ok, _ := tr.Reserve(context.Background(), cust, capPerCust)
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
