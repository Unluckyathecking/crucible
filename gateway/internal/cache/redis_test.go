package cache

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// skipIfNoRedis probes localhost:6379 directly so the test can be skipped on
// CI machines without Redis, matching the pattern in ratelimit/bucket_test.go.
func skipIfNoRedis(t *testing.T) {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		t.Skipf("redis unavailable on localhost:6379, skipping: %v", err)
	}
}

func TestNewRedis_DefaultsTimeoutsTo2Seconds(t *testing.T) {
	skipIfNoRedis(t)

	client, err := NewRedis(context.Background(), "redis://localhost:6379")
	if err != nil {
		t.Fatalf("NewRedis: %v", err)
	}
	defer client.Close()

	opt := client.Options()
	if opt.DialTimeout != 2*time.Second {
		t.Errorf("DialTimeout = %v, want 2s", opt.DialTimeout)
	}
	if opt.ReadTimeout != 2*time.Second {
		t.Errorf("ReadTimeout = %v, want 2s", opt.ReadTimeout)
	}
	if opt.WriteTimeout != 2*time.Second {
		t.Errorf("WriteTimeout = %v, want 2s", opt.WriteTimeout)
	}
}

func TestNewRedis_URLTimeoutsWin(t *testing.T) {
	skipIfNoRedis(t)

	url := "redis://localhost:6379?dial_timeout=500ms&read_timeout=750ms&write_timeout=1s"
	client, err := NewRedis(context.Background(), url)
	if err != nil {
		t.Fatalf("NewRedis: %v", err)
	}
	defer client.Close()

	opt := client.Options()
	if opt.DialTimeout != 500*time.Millisecond {
		t.Errorf("DialTimeout = %v, want 500ms", opt.DialTimeout)
	}
	if opt.ReadTimeout != 750*time.Millisecond {
		t.Errorf("ReadTimeout = %v, want 750ms", opt.ReadTimeout)
	}
	if opt.WriteTimeout != 1*time.Second {
		t.Errorf("WriteTimeout = %v, want 1s", opt.WriteTimeout)
	}
}
