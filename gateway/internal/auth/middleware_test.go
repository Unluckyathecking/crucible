package auth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mwpkg "github.com/Unluckyathecking/crucible/gateway/internal/middleware"
)

// okHandler is the downstream handler that Middleware wraps in tests.
// It writes 200 OK and the authenticated key ID as JSON so tests can
// confirm the key was injected into the context correctly.
var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	k := FromContext(r.Context())
	if k == nil {
		http.Error(w, "no key in context", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"key_id": k.ID.String()})
})

// newMiddlewareStore builds a Store backed by real Postgres + Redis (or skips).
// It inserts a fresh API key and returns the store + the full key string.
func newMiddlewareStore(t *testing.T) (*Store, string) {
	t.Helper()
	db := newTestPostgres(t)
	t.Cleanup(db.Close)
	rdb := newTestRedis(t)

	s := NewStore(db, rdb, testSalt)
	t.Cleanup(s.Close)

	ctx := context.Background()
	_, fullKey, _ := insertTestKey(t, ctx, db, testSalt)
	return s, fullKey
}

// bodyError unmarshals the error code from the JSON response body.
func bodyError(t *testing.T, body []byte) string {
	t.Helper()
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("parse error body: %v (body: %q)", err, body)
	}
	return env.Error.Code
}

func TestMiddleware_MissingAuthorizationHeader(t *testing.T) {
	s, _ := newMiddlewareStore(t)
	h := Middleware(s)(okHandler)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	// No Authorization header at all.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
	body, _ := io.ReadAll(rr.Body)
	if code := bodyError(t, body); code != "UNAUTHORIZED" {
		t.Errorf("error.code = %q, want UNAUTHORIZED", code)
	}
}

func TestMiddleware_WrongScheme(t *testing.T) {
	s, _ := newMiddlewareStore(t)
	h := Middleware(s)(okHandler)

	cases := []struct {
		name  string
		value string
	}{
		{"Basic scheme", "Basic dXNlcjpwYXNz"},
		{"Token scheme", "Token abc123"},
		{"bare value no scheme", "abc123"},
		{"empty value", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.value != "" {
				req.Header.Set("Authorization", tc.value)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			if rr.Code != http.StatusUnauthorized {
				t.Errorf("%s: status = %d, want %d", tc.name, rr.Code, http.StatusUnauthorized)
			}
			body, _ := io.ReadAll(rr.Body)
			if code := bodyError(t, body); code != "UNAUTHORIZED" {
				t.Errorf("%s: error.code = %q, want UNAUTHORIZED", tc.name, code)
			}
		})
	}
}

func TestMiddleware_BearerCaseInsensitive(t *testing.T) {
	s, fullKey := newMiddlewareStore(t)
	h := Middleware(s)(okHandler)

	// RFC 7235 §2.1: auth-scheme is case-insensitive.
	cases := []string{
		"Bearer " + fullKey,
		"bearer " + fullKey,
		"BEARER " + fullKey,
		"BeArEr " + fullKey,
	}

	for _, authz := range cases {
		t.Run(authz[:6], func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Authorization", authz)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				body, _ := io.ReadAll(rr.Body)
				t.Errorf("Authorization %q: status = %d, want 200; body: %s", authz[:20], rr.Code, body)
			}
		})
	}
}

func TestMiddleware_ValidKey_InjectsContextAndForwardsRequest(t *testing.T) {
	s, fullKey := newMiddlewareStore(t)
	h := Middleware(s)(okHandler)

	req := httptest.NewRequest(http.MethodGet, "/v1/run", nil)
	req.Header.Set("Authorization", "Bearer "+fullKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		body, _ := io.ReadAll(rr.Body)
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, body)
	}

	var resp map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["key_id"] == "" {
		t.Error("key_id missing from response — key was not injected into context")
	}
}

func TestMiddleware_RevokedKey(t *testing.T) {
	db := newTestPostgres(t)
	t.Cleanup(db.Close)
	rdb := newTestRedis(t)

	s := NewStore(db, rdb, testSalt)
	t.Cleanup(s.Close)

	ctx := context.Background()
	keyID, fullKey, prefix := insertTestKey(t, ctx, db, testSalt)

	// Warm the cache with a successful lookup.
	if _, err := s.Lookup(ctx, fullKey); err != nil {
		t.Fatalf("warm lookup: %v", err)
	}
	// Confirm the cache entry exists before revocation.
	exists, _ := rdb.Exists(ctx, "auth:"+prefix).Result()
	if exists != 1 {
		t.Fatalf("cache entry should exist before revoke, got exists=%d", exists)
	}

	// Revoke the key — this must also invalidate the Redis cache entry.
	if err := s.Revoke(ctx, keyID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	// Middleware must now reject the revoked key.
	h := Middleware(s)(okHandler)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+fullKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	body, _ := io.ReadAll(rr.Body)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("revoked key: status = %d, want 401; body: %s", rr.Code, body)
	}
	if code := bodyError(t, body); code != "UNAUTHORIZED" {
		t.Errorf("revoked key: error.code = %q, want UNAUTHORIZED", code)
	}
}

func TestMiddleware_MalformedShortKey(t *testing.T) {
	s, _ := newMiddlewareStore(t)
	h := Middleware(s)(okHandler)

	cases := []struct {
		name  string
		token string
	}{
		{"empty token after Bearer", ""},
		{"whitespace only", "   "},
		{"shorter than PrefixLen", "cru_live_short"},
		{"exactly PrefixLen minus one", strings.Repeat("A", PrefixLen-1)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Authorization", "Bearer "+tc.token)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			body, _ := io.ReadAll(rr.Body)
			if rr.Code != http.StatusUnauthorized {
				t.Errorf("%s: status = %d, want 401; body: %s", tc.name, rr.Code, body)
			}
			if code := bodyError(t, body); code != "UNAUTHORIZED" {
				t.Errorf("%s: error.code = %q, want UNAUTHORIZED", tc.name, code)
			}
		})
	}
}

func TestMiddleware_RedisCacheHit(t *testing.T) {
	db := newTestPostgres(t)
	t.Cleanup(db.Close)
	rdb := newTestRedis(t)

	s := NewStore(db, rdb, testSalt)
	t.Cleanup(s.Close)

	ctx := context.Background()
	_, fullKey, prefix := insertTestKey(t, ctx, db, testSalt)

	// Ensure cache is cold, then warm it with a direct store Lookup.
	rdb.Del(ctx, "auth:"+prefix)
	if _, err := s.Lookup(ctx, fullKey); err != nil {
		t.Fatalf("cold lookup to populate cache: %v", err)
	}

	// Verify cache was actually populated.
	cached, err := rdb.Get(ctx, "auth:"+prefix).Bytes()
	if err != nil {
		t.Fatalf("cache should be populated: %v", err)
	}
	if len(cached) == 0 {
		t.Fatal("cache entry is empty after cold lookup")
	}

	// Now make a middleware request — this should satisfy from the Redis cache.
	h := Middleware(s)(okHandler)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+fullKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		body, _ := io.ReadAll(rr.Body)
		t.Fatalf("cache-hit path: status = %d, want 200; body: %s", rr.Code, body)
	}
}

func TestMiddleware_PostgresMissColdPath(t *testing.T) {
	db := newTestPostgres(t)
	t.Cleanup(db.Close)
	rdb := newTestRedis(t)

	s := NewStore(db, rdb, testSalt)
	t.Cleanup(s.Close)

	ctx := context.Background()
	_, fullKey, prefix := insertTestKey(t, ctx, db, testSalt)

	// Force a completely cold lookup by deleting both cache and miss-sentinel.
	rdb.Del(ctx, "auth:"+prefix)
	rdb.Del(ctx, "auth:miss:"+prefix)

	h := Middleware(s)(okHandler)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+fullKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		body, _ := io.ReadAll(rr.Body)
		t.Fatalf("postgres cold path: status = %d, want 200; body: %s", rr.Code, body)
	}

	// After a successful cold lookup, cache must now be warm.
	exists, _ := rdb.Exists(ctx, "auth:"+prefix).Result()
	if exists != 1 {
		t.Error("cache should be populated after cold postgres path")
	}
}

func TestFromContext_NilWhenAbsent(t *testing.T) {
	k := FromContext(context.Background())
	if k != nil {
		t.Errorf("FromContext(empty) = %v, want nil", k)
	}
}

func TestFromContext_ReturnsInjectedKey(t *testing.T) {
	db := newTestPostgres(t)
	t.Cleanup(db.Close)
	rdb := newTestRedis(t)

	s := NewStore(db, rdb, testSalt)
	t.Cleanup(s.Close)

	ctx := context.Background()
	_, fullKey, _ := insertTestKey(t, ctx, db, testSalt)

	key, err := s.Lookup(ctx, fullKey)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}

	enriched := WithTestKey(ctx, key)
	got := FromContext(enriched)
	if got == nil {
		t.Fatal("FromContext returned nil after WithTestKey")
	}
	if got.ID != key.ID {
		t.Errorf("FromContext ID = %s, want %s", got.ID, key.ID)
	}
}

func TestMiddleware_InvalidKeyReturnsUnauthorized(t *testing.T) {
	s, _ := newMiddlewareStore(t)
	h := Middleware(s)(okHandler)

	// A key that is long enough to pass the length check but has no DB row.
	fakeKey := strings.Repeat("A", PrefixLen) + "SUFFIX"

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+fakeKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	body, _ := io.ReadAll(rr.Body)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("invalid key: status = %d, want 401; body: %s", rr.Code, body)
	}
	if code := bodyError(t, body); code != "UNAUTHORIZED" {
		t.Errorf("invalid key: error.code = %q, want UNAUTHORIZED", code)
	}
}

func TestMiddleware_ErrorEnvelopeRequestID(t *testing.T) {
	s, _ := newMiddlewareStore(t)
	h := Middleware(s)(okHandler)

	const wantRID = "test-req-id-auth-mw"
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := context.WithValue(req.Context(), mwpkg.RequestIDKey, wantRID)
	req = req.WithContext(ctx)
	// No Authorization header — triggers the missing-token 401 path without a store lookup.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
	var got struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable bool   `json:"retryable"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if got.Error.RequestID != wantRID {
		t.Errorf("error.request_id = %q, want %q", got.Error.RequestID, wantRID)
	}
	if got.Error.Code != "UNAUTHORIZED" {
		t.Errorf("error.code = %q, want UNAUTHORIZED", got.Error.Code)
	}
}
