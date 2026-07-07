// Package respcache is an opt-in, content-addressed cache of successful worker
// responses. It sits entirely in the gateway — workers never see it — and is
// keyed by sha256(operation || canonical-payload), never by API key or
// customer id: quota, rate-limit, and billing all run per-customer BEFORE this
// cache, so a hit remains a fully-metered, per-customer-billed call that only
// skips the worker HTTP round-trip. Distinct from internal/idempotency, which
// deduplicates a client's own Idempotency-Key retries; this is cross-request,
// cross-customer content dedup.
package respcache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const keyPrefix = "respcache:"

// Entry is a stored, replayable worker response.
type Entry struct {
	StatusCode    int    `json:"status_code"`
	Body          []byte `json:"body"`
	ContentType   string `json:"content_type"`
	BillableUnits uint64 `json:"billable_units"`
	UnitsLabel    string `json:"units_label,omitempty"`
}

// Store is a Redis-backed cache of Entry values, keyed by Key.
// A nil *Store is a valid, inert value: Get always misses and Set is a no-op,
// mirroring idempotency.Store's nil-safe pattern (feature disabled by default).
type Store struct {
	client *redis.Client
}

// NewStore returns a Store backed by client. Returns nil when client is nil
// (feature disabled; Middleware becomes a pass-through).
func NewStore(client *redis.Client) *Store {
	if client == nil {
		return nil
	}
	return &Store{client: client}
}

// Key returns the cache key for operation+payload: hex(sha256(operation ||
// \x00 || canonical(payload))). It intentionally excludes the API key,
// customer id, and any other per-caller secret — identical (operation,
// payload) requests from different customers are meant to share an entry.
func Key(operation string, payload []byte) (string, error) {
	canonical, err := canonicalize(payload)
	if err != nil {
		return "", fmt.Errorf("respcache: canonicalize payload: %w", err)
	}
	h := sha256.New()
	h.Write([]byte(operation))
	h.Write([]byte{0})
	h.Write(canonical)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// canonicalize re-marshals arbitrary JSON so that semantically identical
// payloads produce byte-identical output regardless of source key order.
// encoding/json sorts object keys alphabetically when marshaling a decoded
// map[string]interface{}, so decode-then-reencode normalises key order for free.
//
// UseNumber is required: plain json.Unmarshal decodes every JSON number as
// float64, which loses precision for integers > 2^53. Two semantically different
// requests — {"id":9007199254740992} and {"id":9007199254740993} — would
// canonicalize to the same bytes without it. json.Number marshals back as its
// exact source digits, so large integers and high-precision decimals survive.
func canonicalize(payload []byte) ([]byte, error) {
	if len(payload) == 0 {
		return []byte("null"), nil
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after JSON value")
	}
	return json.Marshal(v)
}

// Get returns the cached Entry for key, or nil (with a nil error) on a cache
// miss. A nil Store always misses.
func (s *Store) Get(ctx context.Context, key string) (*Entry, error) {
	if s == nil {
		return nil, nil
	}
	raw, err := s.client.Get(ctx, keyPrefix+key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var e Entry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, fmt.Errorf("respcache: corrupt cache entry for key %s: %w", key, err)
	}
	return &e, nil
}

// Set stores entry under key with the given TTL. A nil Store, or a TTL <= 0,
// is a no-op — the cache is opt-in per-route and must never grant an entry an
// unbounded lifetime.
func (s *Store) Set(ctx context.Context, key string, entry *Entry, ttl time.Duration) error {
	if s == nil || ttl <= 0 {
		return nil
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("respcache: marshal cache entry: %w", err)
	}
	return s.client.Set(ctx, keyPrefix+key, raw, ttl).Err()
}
