// Package cache wraps the Redis client with parse + ping at construction.
package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// NewRedis parses a redis:// URL, opens a client, and pings it.
// Caller owns the client and must Close() it on shutdown.
func NewRedis(ctx context.Context, url string) (*redis.Client, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	// Bound dial/read/write latency so gateway hot paths fail fast on a slow Redis.
	// Honor URL-supplied values; only fill the gaps.
	if opt.DialTimeout == 0 {
		opt.DialTimeout = 2 * time.Second
	}
	if opt.ReadTimeout == 0 {
		opt.ReadTimeout = 2 * time.Second
	}
	if opt.WriteTimeout == 0 {
		opt.WriteTimeout = 2 * time.Second
	}
	client := redis.NewClient(opt)
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return client, nil
}
