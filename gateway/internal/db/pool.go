// Package db is the gateway's Postgres connection pool + migration runner.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultMinConnsRatio   = 4
	defaultMaxConnLifetime = 1 * time.Hour
)

// ParsePoolConfig parses a DSN into a pgxpool.Config with Crucible defaults.
func ParsePoolConfig(dsn string, maxConns int) (*pgxpool.Config, error) {
	if maxConns < 1 {
		return nil, fmt.Errorf("maxConns must be >= 1, got %d", maxConns)
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}

	cfg.MaxConns = int32(maxConns)
	cfg.MinConns = int32(maxConns / defaultMinConnsRatio)
	if cfg.MinConns < 1 {
		cfg.MinConns = 1
	}

	cfg.MaxConnLifetime = defaultMaxConnLifetime
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second

	return cfg, nil
}

// NewPool opens a pgx connection pool, pings it, and returns the pool.
// Caller owns the pool lifecycle and must Close() it on shutdown.
func NewPool(ctx context.Context, dsn string, maxConns int) (*pgxpool.Pool, error) {
	cfg, err := ParsePoolConfig(dsn, maxConns)
	if err != nil {
		return nil, err
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return pool, nil
}
