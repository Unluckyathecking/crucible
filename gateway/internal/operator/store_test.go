package operator_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/operator"
)

// newTestPostgres returns a pool for the local test database, or skips the test
// if Postgres is not reachable. When TEST_DATABASE_URL or POSTGRES_DSN is set,
// failures are fatal (Postgres is expected to be available in CI).
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

// seedCustomer inserts a minimal customers row and returns the new ID.
// Registers a t.Cleanup to delete it (cascades to usage_events).
func seedCustomer(t *testing.T, pool *pgxpool.Pool, email, planID string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	err := pool.QueryRow(ctx,
		`INSERT INTO customers (email, plan_id) VALUES ($1, $2) RETURNING id`,
		email, planID,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seedCustomer: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM customers WHERE id = $1`, id)
	})
	return id
}

// seedUsageEvent inserts one usage_events row for a customer without needing a real API key.
// Uses a sentinel api_key_id (nil-safe via the FK; we create a placeholder key first).
func seedUsageEvent(t *testing.T, pool *pgxpool.Pool, customerID uuid.UUID, operation string, units int64) {
	t.Helper()
	ctx := context.Background()
	// Insert a throwaway api_key row to satisfy the FK.
	var keyID uuid.UUID
	_ = pool.QueryRow(ctx,
		`INSERT INTO api_keys (customer_id, prefix, hash) VALUES ($1, $2, $3) RETURNING id`,
		customerID, "test_pfx_"+uuid.New().String()[:8], []byte("testhash"),
	).Scan(&keyID)

	_, err := pool.Exec(ctx,
		`INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id)
		 VALUES ($1, $2, $3, $4, $5)`,
		customerID, keyID, operation, units, "req-"+uuid.New().String(),
	)
	if err != nil {
		t.Fatalf("seedUsageEvent: %v", err)
	}
}

// seedAuditEvent inserts one audit_log row.
func seedAuditEvent(t *testing.T, pool *pgxpool.Pool, actorID, action string) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	err := pool.QueryRow(ctx,
		`INSERT INTO audit_log (actor_type, actor_id, action) VALUES ('customer', $1, $2) RETURNING id`,
		actorID, action,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seedAuditEvent: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM audit_log WHERE id = $1`, id)
	})
	return id
}

func TestStore_Customers_Pagination(t *testing.T) {
	pool := newTestPostgres(t)
	s := operator.NewStore(pool)
	ctx := context.Background()

	// Seed three customers on the 'free' plan.
	seedCustomer(t, pool, "op-test-a@example.com", "free")
	seedCustomer(t, pool, "op-test-b@example.com", "free")
	seedCustomer(t, pool, "op-test-c@example.com", "free")

	page, err := s.Customers(ctx, operator.CustomersFilter{Page: 1, PerPage: 2})
	if err != nil {
		t.Fatalf("Customers: %v", err)
	}
	if page.Total < 3 {
		t.Fatalf("total: got %d, want >= 3", page.Total)
	}
	if len(page.Items) != 2 {
		t.Fatalf("items: got %d, want 2", len(page.Items))
	}
}

func TestStore_Customers_PlanFilter(t *testing.T) {
	pool := newTestPostgres(t)
	s := operator.NewStore(pool)
	ctx := context.Background()

	seedCustomer(t, pool, "op-plan-free@example.com", "free")
	seedCustomer(t, pool, "op-plan-pro@example.com", "pro")

	page, err := s.Customers(ctx, operator.CustomersFilter{PlanID: "pro"})
	if err != nil {
		t.Fatalf("Customers(plan=pro): %v", err)
	}
	for _, c := range page.Items {
		if c.PlanID != "pro" {
			t.Errorf("expected plan_id=pro, got %q", c.PlanID)
		}
	}
}

func TestStore_CustomerByID_Found(t *testing.T) {
	pool := newTestPostgres(t)
	s := operator.NewStore(pool)
	ctx := context.Background()

	id := seedCustomer(t, pool, "op-byid@example.com", "free")

	c, err := s.CustomerByID(ctx, id)
	if err != nil {
		t.Fatalf("CustomerByID: %v", err)
	}
	if c.ID != id {
		t.Errorf("id mismatch: got %v, want %v", c.ID, id)
	}
	if c.Email != "op-byid@example.com" {
		t.Errorf("email: got %q", c.Email)
	}
}

func TestStore_CustomerByID_NotFound(t *testing.T) {
	pool := newTestPostgres(t)
	s := operator.NewStore(pool)
	ctx := context.Background()

	_, err := s.CustomerByID(ctx, uuid.New())
	if err != pgx.ErrNoRows {
		t.Fatalf("expected ErrNoRows, got %v", err)
	}
}

func TestStore_CustomerUsage_CurrentPeriod(t *testing.T) {
	pool := newTestPostgres(t)
	s := operator.NewStore(pool)
	ctx := context.Background()

	id := seedCustomer(t, pool, "op-usage@example.com", "free")
	seedUsageEvent(t, pool, id, "analyze", 10)
	seedUsageEvent(t, pool, id, "analyze", 5)
	seedUsageEvent(t, pool, id, "summarize", 3)

	result, err := s.CustomerUsage(ctx, id, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("CustomerUsage: %v", err)
	}
	if result.TotalUnits < 18 {
		t.Errorf("total_units: got %d, want >= 18", result.TotalUnits)
	}
	if result.TotalCalls < 3 {
		t.Errorf("total_calls: got %d, want >= 3", result.TotalCalls)
	}
	if len(result.Breakdown) == 0 {
		t.Error("breakdown is empty")
	}
}

func TestStore_AuditEvents_Unfiltered(t *testing.T) {
	pool := newTestPostgres(t)
	s := operator.NewStore(pool)
	ctx := context.Background()

	id := seedCustomer(t, pool, "op-audit@example.com", "free")
	seedAuditEvent(t, pool, id.String(), "api_key.created")

	page, err := s.AuditEvents(ctx, operator.AuditFilter{Page: 1, PerPage: 20})
	if err != nil {
		t.Fatalf("AuditEvents: %v", err)
	}
	if page.Total < 1 {
		t.Error("expected at least 1 audit event")
	}
}

func TestStore_AuditEvents_FilterByCustomerAndAction(t *testing.T) {
	pool := newTestPostgres(t)
	s := operator.NewStore(pool)
	ctx := context.Background()

	id := seedCustomer(t, pool, "op-audit-filter@example.com", "free")
	seedAuditEvent(t, pool, id.String(), "api_key.created")
	seedAuditEvent(t, pool, id.String(), "plan.changed")

	page, err := s.AuditEvents(ctx, operator.AuditFilter{
		CustomerID: id.String(),
		Action:     "api_key.created",
	})
	if err != nil {
		t.Fatalf("AuditEvents(filtered): %v", err)
	}
	for _, ev := range page.Items {
		if ev.Action != "api_key.created" {
			t.Errorf("unexpected action %q", ev.Action)
		}
	}
}

func TestStore_Plans(t *testing.T) {
	pool := newTestPostgres(t)
	s := operator.NewStore(pool)
	ctx := context.Background()

	plans, err := s.Plans(ctx)
	if err != nil {
		t.Fatalf("Plans: %v", err)
	}
	if len(plans) == 0 {
		t.Fatal("expected at least one plan (seeded in init migration)")
	}
}

// TestStore_NoSecretColumns verifies that the customers result never includes api_keys.hash
// or any other secret-bearing field. It's a compile-time guarantee enforced by the struct
// types, but this test double-checks the JSON serialisation surface.
func TestStore_NoSecretColumns(t *testing.T) {
	pool := newTestPostgres(t)
	s := operator.NewStore(pool)
	ctx := context.Background()

	id := seedCustomer(t, pool, "op-nosecret@example.com", "free")
	c, err := s.CustomerByID(ctx, id)
	if err != nil {
		t.Fatalf("CustomerByID: %v", err)
	}
	b, _ := json.Marshal(c)
	raw := string(b)
	for _, bad := range []string{"hash", "salt", "secret"} {
		if containsKey(raw, bad) {
			t.Errorf("JSON output contains forbidden key %q: %s", bad, raw)
		}
	}
}

func containsKey(jsonStr, key string) bool {
	// Naive but sufficient: look for `"<key>":`  in the JSON output.
	return len(jsonStr) > len(key)+3 &&
		(func() bool {
			needle := `"` + key + `":`
			for i := 0; i <= len(jsonStr)-len(needle); i++ {
				if jsonStr[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		})()
}

// --- Handler integration tests ---

func TestHandler_ListCustomers_RequiresToken(t *testing.T) {
	pool := newTestPostgres(t)
	s := operator.NewStore(pool)

	r := chi.NewRouter()
	r.Use(operator.Middleware("secret-token"))
	r.Get("/v1/admin/customers", operator.ListCustomersHandler(s))

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/customers", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestHandler_ListCustomers_ValidToken(t *testing.T) {
	pool := newTestPostgres(t)
	s := operator.NewStore(pool)

	r := chi.NewRouter()
	r.Use(operator.Middleware("secret-token"))
	r.Get("/v1/admin/customers", operator.ListCustomersHandler(s))

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/customers", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d — body: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Items []any `json:"items"`
		Total int64 `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func TestHandler_GetCustomer_NotFound(t *testing.T) {
	pool := newTestPostgres(t)
	s := operator.NewStore(pool)

	r := chi.NewRouter()
	r.Use(operator.Middleware("tok"))
	r.Get("/v1/admin/customers/{id}", operator.GetCustomerHandler(s))

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/customers/"+uuid.New().String(), nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestHandler_GetCustomerUsage(t *testing.T) {
	pool := newTestPostgres(t)
	s := operator.NewStore(pool)

	id := seedCustomer(t, pool, "op-handler-usage@example.com", "free")
	seedUsageEvent(t, pool, id, "test-op", 7)

	r := chi.NewRouter()
	r.Use(operator.Middleware("tok"))
	r.Get("/v1/admin/customers/{id}/usage", operator.GetCustomerUsageHandler(s))

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/customers/"+id.String()+"/usage", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d — body: %s", rec.Code, rec.Body.String())
	}
	var resp operator.CustomerUsageResult
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.TotalUnits < 7 {
		t.Errorf("total_units: got %d, want >= 7", resp.TotalUnits)
	}
}

func TestHandler_Middleware_WrongToken(t *testing.T) {
	// Middleware auth runs before any Store call; no Postgres needed.
	noop := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	r := chi.NewRouter()
	r.Use(operator.Middleware("correct-token"))
	r.Get("/v1/admin/plans", noop)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/plans", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for wrong token, got %d", rec.Code)
	}
}

func TestHandler_Middleware_EmptyConfiguredToken(t *testing.T) {
	// Even a well-formed bearer must be rejected when OPERATOR_TOKEN is empty.
	noop := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	r := chi.NewRouter()
	r.Use(operator.Middleware("")) // empty = not configured
	r.Get("/v1/admin/plans", noop)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/plans", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 when OPERATOR_TOKEN not set, got %d", rec.Code)
	}
}

func TestHandler_Middleware_ValidToken_PassesThrough(t *testing.T) {
	// Correct token must reach the downstream handler.
	reached := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})

	r := chi.NewRouter()
	r.Use(operator.Middleware("my-secret"))
	r.Get("/v1/admin/plans", handler)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/plans", nil)
	req.Header.Set("Authorization", "Bearer my-secret")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if !reached {
		t.Error("handler was not called")
	}
}

func TestHandler_Middleware_MissingHeader(t *testing.T) {
	noop := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	r := chi.NewRouter()
	r.Use(operator.Middleware("tok"))
	r.Get("/v1/admin/plans", noop)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/plans", nil) // no Authorization header
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for missing header, got %d", rec.Code)
	}
}

func TestHandler_ListPlans(t *testing.T) {
	pool := newTestPostgres(t)
	s := operator.NewStore(pool)

	r := chi.NewRouter()
	r.Use(operator.Middleware("tok"))
	r.Get("/v1/admin/plans", operator.ListPlansHandler(s))

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/plans", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	var plans []operator.Plan
	if err := json.NewDecoder(rec.Body).Decode(&plans); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(plans) == 0 {
		t.Error("expected at least one plan")
	}
}
