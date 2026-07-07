package paging_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	"github.com/Unluckyathecking/crucible/gateway/internal/operator"
	"github.com/Unluckyathecking/crucible/gateway/internal/selferrors"
	"github.com/Unluckyathecking/crucible/gateway/internal/webhookout"
)

// newTestPostgres mirrors the identical helper duplicated across
// operator/webhookout/selferrors's own test suites: skip when Postgres is
// unreachable, unless a DSN was explicitly requested (CI), in which case
// failure is fatal.
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

const testOperatorToken = "cross-endpoint-test-operator-token"

// crossEndpoint describes one of the four list endpoints under test, so a
// single table-driven test can exercise all of them identically.
type crossEndpoint struct {
	name          string
	path          string
	router        http.Handler
	authorize     func(*http.Request)
	envelopeKeys  []string // top-level JSON keys the response envelope must have
	pageParamName string   // query param name that carries the 1-indexed page
}

func newOperatorCrossEndpoints(t *testing.T, pool *pgxpool.Pool) []crossEndpoint {
	t.Helper()
	s := operator.NewStore(pool)

	customersRouter := chi.NewRouter()
	customersRouter.Use(operator.Middleware(testOperatorToken))
	customersRouter.Get("/v1/admin/customers", operator.ListCustomersHandler(s))

	auditRouter := chi.NewRouter()
	auditRouter.Use(operator.Middleware(testOperatorToken))
	auditRouter.Get("/v1/admin/audit", operator.ListAuditEventsHandler(s))

	deadLettersRouter := chi.NewRouter()
	deadLettersRouter.Use(operator.Middleware(testOperatorToken))
	deadLettersRouter.Get("/v1/admin/webhooks/deadletters", webhookout.ListDeadLettersHandler(pool))

	bearer := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+testOperatorToken) }

	return []crossEndpoint{
		{
			name:          "admin_customers",
			path:          "/v1/admin/customers",
			router:        customersRouter,
			authorize:     bearer,
			envelopeKeys:  []string{"items", "total"},
			pageParamName: "page",
		},
		{
			name:          "admin_audit",
			path:          "/v1/admin/audit",
			router:        auditRouter,
			authorize:     bearer,
			envelopeKeys:  []string{"items", "total"},
			pageParamName: "page",
		},
		{
			name:          "admin_webhooks_deadletters",
			path:          "/v1/admin/webhooks/deadletters",
			router:        deadLettersRouter,
			authorize:     bearer,
			envelopeKeys:  []string{"items", "total"},
			pageParamName: "page",
		},
	}
}

func newErrorsCrossEndpoint(pool *pgxpool.Pool) crossEndpoint {
	r := chi.NewRouter()
	r.Get("/v1/errors", selferrors.Handler(pool))
	return crossEndpoint{
		name:          "v1_errors",
		path:          "/v1/errors",
		router:        r,
		authorize:     func(*http.Request) {}, // context is injected via WithContext below
		envelopeKeys:  []string{"data", "has_more", "page", "limit"},
		pageParamName: "page",
	}
}

// doGet issues a GET to ep.path+query, authorized per ep.authorize, optionally
// carrying an auth.Key in context (used only by the /v1/errors endpoint,
// which authenticates via auth.FromContext rather than a bearer token).
func doGet(t *testing.T, ep crossEndpoint, query string, ctx context.Context) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, ep.path+query, nil)
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	ep.authorize(req)
	rec := httptest.NewRecorder()
	ep.router.ServeHTTP(rec, req)
	return rec
}

// TestCrossEndpoint_EnvelopeKeysAndPageParamConsistency asserts that every
// /v1 and /v1/admin list endpoint built on the shared paging package (a)
// accepts a "page" query param with the same 1-indexed, defaults-to-1
// semantics, and (b) responds with its documented envelope's top-level keys.
// The two operator/webhookout-style envelopes ({items,total}) and selferrors'
// distinct ({data,has_more,page,limit}) are intentionally different shapes —
// selferrors' contract is explicitly protected from changing — but every
// endpoint shares the same "page" parameter behavior via paging.ParseQuery.
func TestCrossEndpoint_EnvelopeKeysAndPageParamConsistency(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCrossEndpointCustomer(t, pool, "free")

	endpoints := newOperatorCrossEndpoints(t, pool)
	endpoints = append(endpoints, newErrorsCrossEndpoint(pool))

	for _, ep := range endpoints {
		t.Run(ep.name, func(t *testing.T) {
			var ctx context.Context
			if ep.name == "v1_errors" {
				ctx = errorsTestContext(cust)
			}

			// Absent page defaults to page 1 — never a 400.
			rec := doGet(t, ep, "", ctx)
			if rec.Code != http.StatusOK {
				t.Fatalf("absent page: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			for _, key := range ep.envelopeKeys {
				if _, ok := body[key]; !ok {
					t.Errorf("response envelope missing documented key %q: %s", key, rec.Body.String())
				}
			}

			// page=0 is out-of-range (1-indexed) and must be treated identically
			// to an absent page (default to page 1), not rejected.
			recZero := doGet(t, ep, "?"+ep.pageParamName+"=0", ctx)
			if recZero.Code != http.StatusOK {
				t.Errorf("page=0: status = %d, want 200 (defaults to page 1); body=%s", recZero.Code, recZero.Body.String())
			}
		})
	}
}

// TestCrossEndpoint_OversizedPageRejected asserts every list endpoint built
// on the shared paging package rejects a page magnitude that would push the
// SQL OFFSET past paging.MaxOffset with 400, never a 500 (the previous
// operator/webhookout behavior for a huge ?page= before this package
// introduced the guard framework-wide, mirroring selferrors' pre-existing
// protection).
func TestCrossEndpoint_OversizedPageRejected(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCrossEndpointCustomer(t, pool, "free")

	endpoints := newOperatorCrossEndpoints(t, pool)
	endpoints = append(endpoints, newErrorsCrossEndpoint(pool))

	const absurdPage = "9999999999999"

	for _, ep := range endpoints {
		t.Run(ep.name, func(t *testing.T) {
			var ctx context.Context
			if ep.name == "v1_errors" {
				ctx = errorsTestContext(cust)
			}

			rec := doGet(t, ep, "?"+ep.pageParamName+"="+absurdPage, ctx)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("page=%s: status = %d, want 400; body=%s", absurdPage, rec.Code, rec.Body.String())
			}
		})
	}
}

// errorsTestContext builds an auth.Key context for custID, exactly the way
// selferrors' own test suite does (see selferrors/handler_test.go's
// testKeyContext) — duplicated here since that helper is unexported.
func errorsTestContext(custID uuid.UUID) context.Context {
	key := &auth.Key{
		ID: uuid.New(),
		Customer: auth.Customer{
			ID:    custID,
			Email: "paging-cross-endpoint@example.com",
			Plan:  "free",
		},
	}
	return auth.WithTestKey(context.Background(), key)
}

// seedCrossEndpointCustomer inserts a minimal customers row and registers
// cleanup, mirroring the seedCustomer helpers duplicated across
// operator/webhookout/selferrors's own test suites.
func seedCrossEndpointCustomer(t *testing.T, pool *pgxpool.Pool, planID string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	email := fmt.Sprintf("paging-cross-endpoint-%s@example.com", uuid.New().String()[:8])
	err := pool.QueryRow(ctx,
		`INSERT INTO customers (email, plan_id) VALUES ($1, $2) RETURNING id`,
		email, planID,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seedCrossEndpointCustomer: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM error_events WHERE customer_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM customers WHERE id = $1`, id)
	})
	return id
}
