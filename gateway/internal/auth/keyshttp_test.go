package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/paging"
)

// insertKeysTestKey mirrors insertTestKey (store_test.go) but also returns the
// customer UUID, since the keyshttp handlers need a customer id to build the
// authenticated request context — insertTestKey only returns the key id.
func insertKeysTestKey(t *testing.T, ctx context.Context, pool *pgxpool.Pool, salt string) (keyID, custID uuid.UUID, fullKey, prefix string) {
	t.Helper()

	fullKey, prefix, err := Generate(testPrefix)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	custID = uuid.New()
	email := fmt.Sprintf("keyshttp-%s@example.com", uuid.NewString()[:8])
	if _, err := pool.Exec(ctx, `INSERT INTO customers (id, email, plan_id) VALUES ($1, $2, 'free')`, custID, email); err != nil {
		t.Fatalf("insert customer: %v", err)
	}

	hash := Hash(salt, fullKey)
	keyID = uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO api_keys (id, customer_id, prefix, hash, name)
		VALUES ($1, $2, $3, $4, 'keyshttp test')
	`, keyID, custID, prefix, hash); err != nil {
		t.Fatalf("insert api_key: %v", err)
	}

	return keyID, custID, fullKey, prefix
}

// newKeysRouter mirrors the exact route shape routes.go registers for the
// d.DB-gated /v1/keys block (see server.NewRouter), minus auth.Middleware:
// tests inject the authenticated customer directly via WithTestKey, exactly
// as store_test.go's sibling packages do for similar handler tests.
func newKeysRouter(s *Store) http.Handler {
	r := chi.NewRouter()
	r.Get("/v1/keys", ListKeysHandler(s))
	r.Post("/v1/keys/{id}/rotate", RotateKeysHandler(s, testPrefix))
	r.Delete("/v1/keys/{id}", RevokeKeysHandler(s))
	return r
}

func authedRequest(method, path string, body []byte, custID uuid.UUID) *http.Request {
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(method, path, bytes.NewReader(body))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	key := &Key{ID: uuid.New(), Customer: Customer{ID: custID, Email: "test@example.com", Plan: "free"}}
	return req.WithContext(WithTestKey(context.Background(), key))
}

// --- List -------------------------------------------------------------------

func TestListKeysHandler_ScopedToCustomer(t *testing.T) {
	db := newTestPostgres(t)
	defer db.Close()
	rdb := newTestRedis(t)
	ctx := context.Background()

	s := NewStore(db, rdb, testSalt)
	defer s.Close()
	_, custA, _, prefixA := insertKeysTestKey(t, ctx, db, testSalt)
	_, _, _, _ = insertKeysTestKey(t, ctx, db, testSalt) // custB's key, must not appear in custA's list

	r := newKeysRouter(s)
	req := authedRequest(http.MethodGet, "/v1/keys", nil, custA)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var page paging.Page[keyItemResponse]
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if page.Total != 1 {
		t.Fatalf("total = %d, want exactly 1 (custB's key must not appear)", page.Total)
	}
	if len(page.Items) != 1 {
		t.Fatalf("got %d keys, want exactly 1 (custB's key must not appear)", len(page.Items))
	}
	if page.Items[0].Prefix != prefixA {
		t.Errorf("prefix = %q, want %q", page.Items[0].Prefix, prefixA)
	}
}

func TestListKeysHandler_NeverIncludesSecret(t *testing.T) {
	db := newTestPostgres(t)
	defer db.Close()
	rdb := newTestRedis(t)
	ctx := context.Background()

	s := NewStore(db, rdb, testSalt)
	defer s.Close()
	_, custID, fullKey, _ := insertKeysTestKey(t, ctx, db, testSalt)

	r := newKeysRouter(s)
	req := authedRequest(http.MethodGet, "/v1/keys", nil, custID)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, fullKey) {
		t.Error("response body contains the full key")
	}
	if strings.Contains(body, `"hash"`) {
		t.Error("response body contains a hash field")
	}
}

func TestListKeysHandler_NoAuth(t *testing.T) {
	s := NewStore(nil, nil, testSalt)
	defer s.Close()
	r := newKeysRouter(s)
	req := httptest.NewRequest(http.MethodGet, "/v1/keys", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// --- Rotate -------------------------------------------------------------------

func TestRotateKeysHandler_Success(t *testing.T) {
	db := newTestPostgres(t)
	defer db.Close()
	rdb := newTestRedis(t)
	ctx := context.Background()

	s := NewStore(db, rdb, testSalt)
	defer s.Close()
	keyID, custID, oldFull, _ := insertKeysTestKey(t, ctx, db, testSalt)

	r := newKeysRouter(s)
	req := authedRequest(http.MethodPost, "/v1/keys/"+keyID.String()+"/rotate", []byte(`{}`), custID)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	newFull, ok := out["key"]
	if !ok || newFull == "" {
		t.Fatalf("response missing non-empty 'key': %v", out)
	}
	if newFull == oldFull {
		t.Error("new key must differ from the old key")
	}

	// Default grace (1h) means the old key is still valid immediately after rotation.
	if _, err := s.Lookup(ctx, oldFull); err != nil {
		t.Errorf("old key should still authenticate during grace window: %v", err)
	}
	// The new key must authenticate too.
	if _, err := s.Lookup(ctx, newFull); err != nil {
		t.Errorf("new key should authenticate: %v", err)
	}
}

func TestRotateKeysHandler_ClampsExcessiveGrace(t *testing.T) {
	db := newTestPostgres(t)
	defer db.Close()
	rdb := newTestRedis(t)
	ctx := context.Background()

	s := NewStore(db, rdb, testSalt)
	defer s.Close()
	keyID, custID, _, oldPrefix := insertKeysTestKey(t, ctx, db, testSalt)

	r := newKeysRouter(s)
	// 30 days requested; Store.Rotate clamps to maxGrace (7 days) server-side.
	body := []byte(`{"grace_secs": 2592000}`)
	req := authedRequest(http.MethodPost, "/v1/keys/"+keyID.String()+"/rotate", body, custID)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var expiresAt time.Time
	if err := db.QueryRow(ctx, `SELECT expires_at FROM api_keys WHERE prefix = $1`, oldPrefix).Scan(&expiresAt); err != nil {
		t.Fatalf("query expires_at: %v", err)
	}
	if max := time.Now().Add(7 * 24 * time.Hour).Add(time.Minute); expiresAt.After(max) {
		t.Errorf("expires_at = %v, exceeds 7-day clamp (want before %v)", expiresAt, max)
	}
}

func TestRotateKeysHandler_OwnedByOtherCustomer_404(t *testing.T) {
	db := newTestPostgres(t)
	defer db.Close()
	rdb := newTestRedis(t)
	ctx := context.Background()

	s := NewStore(db, rdb, testSalt)
	defer s.Close()
	keyID, _, oldFull, _ := insertKeysTestKey(t, ctx, db, testSalt)
	_, attacker, _, _ := insertKeysTestKey(t, ctx, db, testSalt)

	r := newKeysRouter(s)
	req := authedRequest(http.MethodPost, "/v1/keys/"+keyID.String()+"/rotate", []byte(`{}`), attacker)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (IDOR-safe)", rec.Code)
	}

	// The victim's key must be unaffected — still authenticates, unrotated.
	if _, err := s.Lookup(ctx, oldFull); err != nil {
		t.Errorf("victim's key must still authenticate after failed cross-customer rotate: %v", err)
	}
}

func TestRotateKeysHandler_InvalidID(t *testing.T) {
	db := newTestPostgres(t)
	defer db.Close()
	rdb := newTestRedis(t)
	s := NewStore(db, rdb, testSalt)
	defer s.Close()

	r := newKeysRouter(s)
	req := authedRequest(http.MethodPost, "/v1/keys/not-a-uuid/rotate", []byte(`{}`), uuid.New())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestRotateKeysHandler_NotFound(t *testing.T) {
	db := newTestPostgres(t)
	defer db.Close()
	rdb := newTestRedis(t)
	s := NewStore(db, rdb, testSalt)
	defer s.Close()

	r := newKeysRouter(s)
	req := authedRequest(http.MethodPost, "/v1/keys/"+uuid.New().String()+"/rotate", []byte(`{}`), uuid.New())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestRotateKeysHandler_InGraceKey_409(t *testing.T) {
	db := newTestPostgres(t)
	defer db.Close()
	rdb := newTestRedis(t)
	ctx := context.Background()

	s := NewStore(db, rdb, testSalt)
	defer s.Close()
	keyID, custID, _, _ := insertKeysTestKey(t, ctx, db, testSalt)

	r := newKeysRouter(s)

	// First rotate: succeeds, puts the original key in grace (expires_at set).
	req1 := authedRequest(http.MethodPost, "/v1/keys/"+keyID.String()+"/rotate", []byte(`{}`), custID)
	req1.Header.Set("Content-Type", "application/json")
	rec1 := httptest.NewRecorder()
	r.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first rotate status = %d, want 200, body = %s", rec1.Code, rec1.Body.String())
	}

	// Second rotate of the same (now in-grace) key: must return 409, not 404.
	req2 := authedRequest(http.MethodPost, "/v1/keys/"+keyID.String()+"/rotate", []byte(`{}`), custID)
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusConflict {
		t.Fatalf("re-rotate of in-grace key status = %d, want 409, body = %s", rec2.Code, rec2.Body.String())
	}
	var body map[string]map[string]string
	if err := json.Unmarshal(rec2.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 409 response: %v", err)
	}
	if code := body["error"]["code"]; code != "KEY_ALREADY_ROTATED" {
		t.Errorf("error code = %q, want KEY_ALREADY_ROTATED", code)
	}
}

// --- Revoke -------------------------------------------------------------------

func TestRevokeKeysHandler_ImmediateCacheInvalidation(t *testing.T) {
	db := newTestPostgres(t)
	defer db.Close()
	rdb := newTestRedis(t)
	ctx := context.Background()

	s := NewStore(db, rdb, testSalt)
	defer s.Close()
	keyID, custID, fullKey, _ := insertKeysTestKey(t, ctx, db, testSalt)

	// Warm the Redis hot-cache entry.
	if _, err := s.Lookup(ctx, fullKey); err != nil {
		t.Fatalf("warm lookup: %v", err)
	}

	r := newKeysRouter(s)
	req := authedRequest(http.MethodDelete, "/v1/keys/"+keyID.String(), nil, custID)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// The very next request with the revoked key must 401 — not after the 60s
	// cache TTL. Exercised through the real auth.Middleware so this covers the
	// actual production code path, not just Store.Lookup directly.
	protected := chi.NewRouter()
	protected.With(Middleware(s)).Get("/protected", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	authReq := httptest.NewRequest(http.MethodGet, "/protected", nil)
	authReq.Header.Set("Authorization", "Bearer "+fullKey)
	authRec := httptest.NewRecorder()
	protected.ServeHTTP(authRec, authReq)

	if authRec.Code != http.StatusUnauthorized {
		t.Fatalf("status after revoke = %d, want 401 (cache must be invalidated immediately)", authRec.Code)
	}
}

func TestRevokeKeysHandler_EmitsAudit(t *testing.T) {
	db := newTestPostgres(t)
	defer db.Close()
	rdb := newTestRedis(t)
	ctx := context.Background()

	s := NewStore(db, rdb, testSalt)
	defer s.Close()
	keyID, custID, _, _ := insertKeysTestKey(t, ctx, db, testSalt)

	r := newKeysRouter(s)
	req := authedRequest(http.MethodDelete, "/v1/keys/"+keyID.String(), nil, custID)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var count int
	err := db.QueryRow(ctx, `
		SELECT count(*) FROM audit_log
		WHERE action = 'api_key.revoked' AND actor_id = $1 AND target_id = $2
	`, custID.String(), keyID.String()).Scan(&count)
	if err != nil {
		t.Fatalf("query audit_log: %v", err)
	}
	if count != 1 {
		t.Fatalf("audit_log rows for this revoke = %d, want 1", count)
	}
}

func TestRevokeKeysHandler_OwnedByOtherCustomer_404(t *testing.T) {
	db := newTestPostgres(t)
	defer db.Close()
	rdb := newTestRedis(t)
	ctx := context.Background()

	s := NewStore(db, rdb, testSalt)
	defer s.Close()
	keyID, _, fullKey, _ := insertKeysTestKey(t, ctx, db, testSalt)
	_, attacker, _, _ := insertKeysTestKey(t, ctx, db, testSalt)

	r := newKeysRouter(s)
	req := authedRequest(http.MethodDelete, "/v1/keys/"+keyID.String(), nil, attacker)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (IDOR-safe)", rec.Code)
	}

	// The victim's key must still authenticate — the cross-customer revoke must not
	// have taken effect.
	if _, err := s.Lookup(ctx, fullKey); err != nil {
		t.Errorf("victim's key must still authenticate after failed cross-customer revoke: %v", err)
	}
}

func TestRevokeKeysHandler_InvalidID(t *testing.T) {
	db := newTestPostgres(t)
	defer db.Close()
	rdb := newTestRedis(t)
	s := NewStore(db, rdb, testSalt)
	defer s.Close()

	r := newKeysRouter(s)
	req := authedRequest(http.MethodDelete, "/v1/keys/not-a-uuid", nil, uuid.New())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestRevokeKeysHandler_NotFound(t *testing.T) {
	db := newTestPostgres(t)
	defer db.Close()
	rdb := newTestRedis(t)
	s := NewStore(db, rdb, testSalt)
	defer s.Close()

	r := newKeysRouter(s)
	req := authedRequest(http.MethodDelete, "/v1/keys/"+uuid.New().String(), nil, uuid.New())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
