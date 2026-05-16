package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// newTestRedis returns a redis client pointed at localhost:6379 or skips the test
// if no Redis is reachable. Keeps the test self-contained without testcontainers.
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

func TestAllow_BelowLimitPasses(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	cust := fmt.Sprintf("test-below-%d", time.Now().UnixNano())
	rdb.Del(ctx, "rl:"+cust) // clean state

	b := New(rdb)
	for i := 0; i < 5; i++ {
		if err := b.Allow(ctx, cust, 10); err != nil {
			t.Fatalf("call %d should pass under limit, got %v", i, err)
		}
	}
}

func TestAllow_OverLimitRejects(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	cust := fmt.Sprintf("test-over-%d", time.Now().UnixNano())
	rdb.Del(ctx, "rl:"+cust)
	defer rdb.Del(ctx, "rl:"+cust)

	b := New(rdb)
	const limit = 5
	for i := 0; i < limit; i++ {
		if err := b.Allow(ctx, cust, limit); err != nil {
			t.Fatalf("call %d should pass, got %v", i, err)
		}
	}
	if err := b.Allow(ctx, cust, limit); !errors.Is(err, ErrLimited) {
		t.Errorf("call %d should be ErrLimited, got %v", limit+1, err)
	}
}

// The whole reason this fix exists: a fixed-minute bucket would let a customer
// burst limit*2 across the second-59→second-61 boundary. With sliding window we
// can't easily reproduce the boundary case in fast tests (would need time freezing),
// but we can verify that exhausted limits don't reset until the window has actually
// passed.
func TestAllow_LimitedRequestsDoNotResetWindow(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	cust := fmt.Sprintf("test-noslip-%d", time.Now().UnixNano())
	rdb.Del(ctx, "rl:"+cust)
	defer rdb.Del(ctx, "rl:"+cust)

	b := New(rdb)
	const limit = 3
	for i := 0; i < limit; i++ {
		_ = b.Allow(ctx, cust, limit)
	}
	// Hammer with rejected attempts — they MUST NOT slide the window forward.
	for i := 0; i < 10; i++ {
		if err := b.Allow(ctx, cust, limit); !errors.Is(err, ErrLimited) {
			t.Errorf("rejected attempt %d should still be ErrLimited, got %v", i, err)
		}
	}
}

func TestAllow_ZeroLimitMeansUnlimited(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	cust := fmt.Sprintf("test-unl-%d", time.Now().UnixNano())
	rdb.Del(ctx, "rl:"+cust)

	b := New(rdb)
	for i := 0; i < 1000; i++ {
		if err := b.Allow(ctx, cust, 0); err != nil {
			t.Fatalf("perMinute=0 means unlimited, got %v at call %d", err, i)
		}
	}
}
