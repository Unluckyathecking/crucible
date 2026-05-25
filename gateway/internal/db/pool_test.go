package db

import (
	"strings"
	"testing"
	"time"
)

func TestParsePoolConfig_Valid(t *testing.T) {
	cfg, err := ParsePoolConfig("postgres://localhost", 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MaxConns != 20 {
		t.Errorf("MaxConns = %d, want 20", cfg.MaxConns)
	}
	if cfg.MinConns != 5 {
		t.Errorf("MinConns = %d, want 5", cfg.MinConns)
	}
	if cfg.MaxConnLifetime != 1*time.Hour {
		t.Errorf("MaxConnLifetime = %v, want 1h", cfg.MaxConnLifetime)
	}
}

func TestParsePoolConfig_SmallMaxConns(t *testing.T) {
	cfg, err := ParsePoolConfig("postgres://localhost", 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MinConns != 1 {
		t.Errorf("MinConns = %d, want 1 when maxConns is small", cfg.MinConns)
	}
}

func TestParsePoolConfig_InvalidMaxConns(t *testing.T) {
	_, err := ParsePoolConfig("postgres://localhost", 0)
	if err == nil {
		t.Fatal("expected error for maxConns=0, got nil")
	}
	if !strings.Contains(err.Error(), "maxConns must be >= 1") {
		t.Errorf("expected error to mention maxConns must be >= 1, got %v", err)
	}

	_, err = ParsePoolConfig("postgres://localhost", -1)
	if err == nil {
		t.Fatal("expected error for maxConns=-1, got nil")
	}
}

func TestParsePoolConfig_InvalidDSN(t *testing.T) {
	_, err := ParsePoolConfig("not-a-dsn", 20)
	if err == nil {
		t.Fatal("expected error for invalid dsn, got nil")
	}
}
