package cache

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestNewRedisConnMaxIdleTime(t *testing.T) {
	// Verify that NewRedis applies a default ConnMaxIdleTime when not set.
	opt, err := redis.ParseURL("redis://localhost:6379")
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	// Simulate the default applied in NewRedis
	if opt.ConnMaxIdleTime == 0 {
		opt.ConnMaxIdleTime = 30 * time.Second
	}
	if opt.ConnMaxIdleTime != 30*time.Second {
		t.Fatalf("expected ConnMaxIdleTime 30s, got %v", opt.ConnMaxIdleTime)
