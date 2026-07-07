package respcache_test

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Unluckyathecking/crucible/gateway/internal/respcache"
)

func newTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		t.Skipf("redis unavailable: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestNewStore_NilClient(t *testing.T) {
	if s := respcache.NewStore(nil); s != nil {
		t.Errorf("NewStore(nil) = %v, want nil", s)
	}
}

func TestNilStore_GetAlwaysMisses(t *testing.T) {
	var s *respcache.Store
	entry, err := s.Get(context.Background(), "any-key")
	if err != nil {
		t.Fatalf("Get on nil store: unexpected error: %v", err)
	}
	if entry != nil {
		t.Errorf("Get on nil store = %+v, want nil (always a miss)", entry)
	}
}

func TestNilStore_SetIsNoOp(t *testing.T) {
	var s *respcache.Store
	err := s.Set(context.Background(), "any-key", &respcache.Entry{StatusCode: 200}, time.Minute)
	if err != nil {
		t.Errorf("Set on nil store: unexpected error: %v", err)
	}
}

func TestKey_SameOperationAndPayload_SameKey(t *testing.T) {
	k1, err := respcache.Key("lookup", []byte(`{"a":1,"b":2}`))
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	k2, err := respcache.Key("lookup", []byte(`{"a":1,"b":2}`))
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if k1 != k2 {
		t.Errorf("Key mismatch for identical input: %q != %q", k1, k2)
	}
}

// TestKey_ObjectKeyOrderIsCanonicalized is the "stable canonicalization" acceptance:
// the same object with keys in a different order must hash to the same key.
func TestKey_ObjectKeyOrderIsCanonicalized(t *testing.T) {
	k1, err := respcache.Key("lookup", []byte(`{"a":1,"b":2}`))
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	k2, err := respcache.Key("lookup", []byte(`{"b":2,"a":1}`))
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if k1 != k2 {
		t.Errorf("Key must be stable under object key reordering: %q != %q", k1, k2)
	}
}

func TestKey_DifferentOperation_DifferentKey(t *testing.T) {
	k1, err := respcache.Key("lookup", []byte(`{"a":1}`))
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	k2, err := respcache.Key("other-op", []byte(`{"a":1}`))
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if k1 == k2 {
		t.Errorf("expected different keys for different operations, both = %q", k1)
	}
}

func TestKey_DifferentPayload_DifferentKey(t *testing.T) {
	k1, err := respcache.Key("lookup", []byte(`{"a":1}`))
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	k2, err := respcache.Key("lookup", []byte(`{"a":2}`))
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if k1 == k2 {
		t.Errorf("expected different keys for different payloads, both = %q", k1)
	}
}

func TestKey_MalformedPayload_ReturnsError(t *testing.T) {
	if _, err := respcache.Key("lookup", []byte(`{not json`)); err == nil {
		t.Error("expected error for malformed JSON payload, got nil")
	}
}

// TestKey_TrailingData_NoCollision covers two requirements from the trailing-data
// guard:
//
// (b) canonicalize must return a non-nil error for input with trailing bytes.
// (a) Key on a payload with trailing bytes must not return the same key as the
// same payload without trailing bytes (collision impossible once an error is returned,
// but the test makes both assertions explicit).
func TestKey_TrailingData_NoCollision(t *testing.T) {
	const op = "lookup"
	valid := []byte(`{"a":1}`)
	trailing := []byte(`{"a":1} x`)

	k1, err := respcache.Key(op, valid)
	if err != nil {
		t.Fatalf("Key(valid): unexpected error: %v", err)
	}

	k2, err2 := respcache.Key(op, trailing)

	// (b) trailing data must be rejected
	if err2 == nil {
		t.Error("Key(trailing): expected non-nil error for trailing data, got nil")
	}

	// (a) keys must not collide; if err2 != nil, k2 is "" so they already differ.
	if err2 == nil && k1 == k2 {
		t.Errorf("Key with trailing data must not collide with valid key: both = %q", k1)
	}
}

// TestKey_LargeIntegersPreservePrecision is the acceptance test for the UseNumber
// fix: integers above 2^53 that differ by 1 must produce different cache keys.
// Without UseNumber, both collapse to 9.007199254740992e+15 after the float64
// round-trip and generate the same key, causing cross-request cache collisions.
func TestKey_LargeIntegersPreservePrecision(t *testing.T) {
	a := `{"id":9007199254740992}`
	b := `{"id":9007199254740993}`
	ka, err := respcache.Key("lookup", []byte(a))
	if err != nil {
		t.Fatalf("Key(a): %v", err)
	}
	kb, err := respcache.Key("lookup", []byte(b))
	if err != nil {
		t.Fatalf("Key(b): %v", err)
	}
	if ka == kb {
		t.Errorf("Key must differ for large integers that differ by 1: both = %q", ka)
	}
}

// TestKey_LargeIntegerOrderIsCanonicalized confirms that UseNumber does not
// break key-order normalisation: an object with large-integer values and
// reordered keys must still hash to the same key.
func TestKey_LargeIntegerOrderIsCanonicalized(t *testing.T) {
	ab := `{"a":9007199254740992,"b":9007199254740993}`
	ba := `{"b":9007199254740993,"a":9007199254740992}`
	kab, err := respcache.Key("lookup", []byte(ab))
	if err != nil {
		t.Fatalf("Key(ab): %v", err)
	}
	kba, err := respcache.Key("lookup", []byte(ba))
	if err != nil {
		t.Fatalf("Key(ba): %v", err)
	}
	if kab != kba {
		t.Errorf("Key must be stable under object key reordering with large integers: %q != %q", kab, kba)
	}
}

func TestStore_SetAndGet_RoundTrips(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	s := respcache.NewStore(rdb)
	key := "test-roundtrip-" + time.Now().String()
	t.Cleanup(func() { rdb.Del(ctx, "respcache:"+key) })

	want := &respcache.Entry{
		StatusCode:    200,
		Body:          []byte(`{"result":"ok"}`),
		ContentType:   "application/json",
		BillableUnits: 3,
		UnitsLabel:    "lookups",
	}
	if err := s.Set(ctx, key, want, time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("Get: expected a hit, got nil (miss)")
	}
	if got.StatusCode != want.StatusCode || string(got.Body) != string(want.Body) ||
		got.ContentType != want.ContentType || got.BillableUnits != want.BillableUnits ||
		got.UnitsLabel != want.UnitsLabel {
		t.Errorf("Get = %+v, want %+v", got, want)
	}
}

func TestStore_Get_Miss(t *testing.T) {
	rdb := newTestRedis(t)
	s := respcache.NewStore(rdb)
	got, err := s.Get(context.Background(), "definitely-absent-"+time.Now().String())
	if err != nil {
		t.Fatalf("Get: unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("Get = %+v, want nil (miss)", got)
	}
}

func TestStore_Set_ZeroTTL_IsNoOp(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	s := respcache.NewStore(rdb)
	key := "test-zero-ttl-" + time.Now().String()
	t.Cleanup(func() { rdb.Del(ctx, "respcache:"+key) })

	if err := s.Set(ctx, key, &respcache.Entry{StatusCode: 200, BillableUnits: 1}, 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Errorf("Set with ttl<=0 must not store an entry, got %+v", got)
	}
}

func TestStore_TTLExpiry(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	s := respcache.NewStore(rdb)
	key := "test-ttl-expiry-" + time.Now().String()
	t.Cleanup(func() { rdb.Del(ctx, "respcache:"+key) })

	if err := s.Set(ctx, key, &respcache.Entry{StatusCode: 200, BillableUnits: 1}, 200*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got, err := s.Get(ctx, key); err != nil || got == nil {
		t.Fatalf("expected a hit immediately after Set, got entry=%+v err=%v", got, err)
	}

	time.Sleep(400 * time.Millisecond)

	got, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get after expiry: unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected a miss after TTL expiry, got %+v", got)
	}
}
