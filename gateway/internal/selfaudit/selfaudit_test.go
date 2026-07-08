package selfaudit_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	"github.com/Unluckyathecking/crucible/gateway/internal/license"
	"github.com/Unluckyathecking/crucible/gateway/internal/selfaudit"
)

func newTestPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	explicit := dsn != ""
	if !explicit {
		if v := os.Getenv("POSTGRES_DSN"); v != "" {
			dsn = v
			explicit = true
		} else {
			dsn = "postgres://crucible@localhost:5432/crucible?sslmode=disable"
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		if explicit {
			t.Fatalf("TEST_DATABASE_URL set but postgres unavailable: %v", err)
		}
		t.Skipf("postgres unavailable, skipping: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		if explicit {
			t.Fatalf("TEST_DATABASE_URL set but postgres ping failed: %v", err)
		}
		t.Skipf("postgres ping failed, skipping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func exportLicensed() *license.License {
	return &license.License{Features: []string{license.FeatureAuditExport}}
}

// seedActorRow inserts an audit_log row where the customer is the actor.
func seedActorRow(t *testing.T, pool *pgxpool.Pool, customerID uuid.UUID, action string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO audit_log (actor_type, actor_id, action) VALUES ('customer', $1, $2)`,
		customerID.String(), action)
	if err != nil {
		t.Fatalf("seedActorRow: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM audit_log WHERE action = $1`, action) })
}

// seedTargetRow inserts an admin-initiated audit_log row targeting the customer.
func seedTargetRow(t *testing.T, pool *pgxpool.Pool, customerID uuid.UUID, action string) {
	t.Helper()
	cid := customerID.String()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO audit_log (actor_type, actor_id, action, target_type, target_id)
		 VALUES ('admin', 'operator', $1, 'customer', $2)`,
		action, cid)
	if err != nil {
		t.Fatalf("seedTargetRow: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM audit_log WHERE action = $1`, action) })
}

func testKeyContext(customerID uuid.UUID) context.Context {
	return auth.WithTestKey(context.Background(), &auth.Key{
		ID:       uuid.New(),
		Customer: auth.Customer{ID: customerID, Email: "selfaudit@example.com", Plan: "free"},
	})
}

func newRouter(pool *pgxpool.Pool, lic *license.License) http.Handler {
	r := chi.NewRouter()
	r.Get("/v1/audit", selfaudit.Handler(pool, lic))
	return r
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) selfaudit.Response {
	t.Helper()
	var resp selfaudit.Response
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v — body: %s", err, rec.Body.String())
	}
	return resp
}

// TestHandler_Unlicensed403 asserts a nil (community) license yields 403
// FEATURE_NOT_LICENSED before any auth or DB work — so a nil DB is safe.
func TestHandler_Unlicensed403(t *testing.T) {
	r := newRouter(nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/audit", nil).WithContext(testKeyContext(uuid.New()))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403 — body: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if body.Error.Code != "FEATURE_NOT_LICENSED" {
		t.Errorf("code: got %q, want FEATURE_NOT_LICENSED", body.Error.Code)
	}
}

// TestHandler_NoAuth asserts a licensed request with no auth context 401s.
func TestHandler_NoAuth(t *testing.T) {
	r := newRouter(nil, exportLicensed())
	req := httptest.NewRequest(http.MethodGet, "/v1/audit", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
}

// TestHandler_IDOR asserts customer A never sees customer B's audit rows, for
// both actor-scoped and target-scoped rows.
func TestHandler_IDOR(t *testing.T) {
	pool := newTestPostgres(t)
	custA, custB := uuid.New(), uuid.New()
	tag := uuid.NewString()[:8]
	aActor := tag + ".actor-a"
	aTarget := tag + ".target-a"
	bActor := tag + ".actor-b"
	bTarget := tag + ".target-b"
	seedActorRow(t, pool, custA, aActor)
	seedTargetRow(t, pool, custA, aTarget)
	seedActorRow(t, pool, custB, bActor)
	seedTargetRow(t, pool, custB, bTarget)

	r := newRouter(pool, exportLicensed())
	req := httptest.NewRequest(http.MethodGet, "/v1/audit", nil).WithContext(testKeyContext(custA))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 — body: %s", rec.Code, rec.Body.String())
	}
	resp := decode(t, rec)

	seen := map[string]bool{}
	for _, e := range resp.Data {
		seen[e.Action] = true
	}
	if !seen[aActor] || !seen[aTarget] {
		t.Errorf("customer A missing own rows: %+v", resp.Data)
	}
	if seen[bActor] || seen[bTarget] {
		t.Errorf("customer A leaked customer B's rows: %+v", resp.Data)
	}
}

// TestHandler_ActionFilter asserts the action query param narrows results.
func TestHandler_ActionFilter(t *testing.T) {
	pool := newTestPostgres(t)
	cust := uuid.New()
	tag := uuid.NewString()[:8]
	wanted := tag + ".wanted"
	other := tag + ".other"
	seedActorRow(t, pool, cust, wanted)
	seedActorRow(t, pool, cust, other)

	r := newRouter(pool, exportLicensed())
	req := httptest.NewRequest(http.MethodGet, "/v1/audit?action="+wanted, nil).WithContext(testKeyContext(cust))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d — body: %s", rec.Code, rec.Body.String())
	}
	resp := decode(t, rec)
	if len(resp.Data) != 1 || resp.Data[0].Action != wanted {
		t.Fatalf("action filter: got %+v, want single %q", resp.Data, wanted)
	}
}

// TestHandler_InvalidActionFilter asserts a malformed action filter is 400 before any DB call.
func TestHandler_InvalidActionFilter(t *testing.T) {
	r := newRouter(nil, exportLicensed())
	req := httptest.NewRequest(http.MethodGet, "/v1/audit?action=not+valid%21", nil).WithContext(testKeyContext(uuid.New()))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

// TestHandler_Pagination asserts has_more/page/limit threading over 3 rows.
func TestHandler_Pagination(t *testing.T) {
	pool := newTestPostgres(t)
	cust := uuid.New()
	tag := uuid.NewString()[:8]
	for i := 0; i < 3; i++ {
		seedActorRow(t, pool, cust, fmt.Sprintf("%s.row-%d", tag, i))
	}

	r := newRouter(pool, exportLicensed())
	// The scope is a freshly-generated customer id with exactly 3 seeded rows, so
	// pagination is deterministic even against a shared DB.
	req := httptest.NewRequest(http.MethodGet, "/v1/audit?limit=2&page=1", nil).WithContext(testKeyContext(cust))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	resp := decode(t, rec)
	if resp.Limit != 2 {
		t.Errorf("limit: got %d, want 2", resp.Limit)
	}
	if len(resp.Data) != 2 || !resp.HasMore {
		t.Fatalf("page 1: got %d rows has_more=%v, want 2 rows has_more=true", len(resp.Data), resp.HasMore)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/v1/audit?limit=2&page=2", nil).WithContext(testKeyContext(cust))
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)
	resp2 := decode(t, rec2)
	if len(resp2.Data) != 1 || resp2.HasMore {
		t.Fatalf("page 2: got %d rows has_more=%v, want 1 row has_more=false", len(resp2.Data), resp2.HasMore)
	}
}

// TestHandler_CacheControl asserts 200 responses set Cache-Control: no-store.
func TestHandler_CacheControl(t *testing.T) {
	pool := newTestPostgres(t)
	cust := uuid.New()
	r := newRouter(pool, exportLicensed())
	req := httptest.NewRequest(http.MethodGet, "/v1/audit", nil).WithContext(testKeyContext(cust))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if strings.Contains(rec.Body.String(), `"actor_id"`) {
		t.Error("response exposed actor_id")
	}
}
