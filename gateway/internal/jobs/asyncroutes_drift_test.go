// External test package (not `package jobs`): server imports jobs, so a test
// file that imports server must live outside jobs's internal test binary to
// avoid an import cycle.
package jobs_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/config"
	"github.com/Unluckyathecking/crucible/gateway/internal/openapi"
	"github.com/Unluckyathecking/crucible/gateway/internal/server"
)

// mockChecker implements server.HealthChecker, mirroring the identical
// helper in server/routes_test.go (unexported there, so reimplemented here).
type mockChecker struct{}

func (m *mockChecker) Ping(_ context.Context) error { return nil }

// TestAsyncRoutesDriftGuard mirrors server.TestV1RoutesDriftGuard for the
// async opt-in table: every key in AsyncRoutes must correspond to a path
// declared in V1Routes (one table drives both branches — an AsyncRoutes
// entry for a path routes_table.go never declares would silently no-op),
// and GET /v1/jobs/{id} must be mounted in the real router and appear in the
// OpenAPI document actually served.
func TestAsyncRoutesDriftGuard(t *testing.T) {
	v1Paths := make(map[string]bool, len(server.V1Routes))
	for _, rt := range server.V1Routes {
		v1Paths[rt.Path] = true
	}
	for path := range server.AsyncRoutes {
		if !v1Paths[path] {
			t.Errorf("AsyncRoutes contains path %q which is not declared in V1Routes", path)
		}
	}

	// pgxpool.New is lazy — it validates the DSN shape but does not dial
	// until first use, so a reachable Postgres is not required to exercise
	// NewRouter's d.DB != nil route-registration branch (this test never
	// issues a query).
	pool, err := pgxpool.New(context.Background(), "postgres://u:p@127.0.0.1:1/db")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	d := &server.Deps{
		Cfg:   &config.Config{BodyLimitBytes: 1048576},
		Redis: &mockChecker{},
		PG:    &mockChecker{},
		DB:    pool,
	}
	router := server.NewRouter(d)
	cr, ok := router.(chi.Routes)
	if !ok {
		t.Fatalf("NewRouter returned %T which does not implement chi.Routes", router)
	}

	var found bool
	if err := chi.Walk(cr, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodGet && route == "/v1/jobs/{id}" {
			found = true
		}
		return nil
	}); err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	if !found {
		t.Error("GET /v1/jobs/{id} is not mounted in the router")
	}

	// jobsPathItems is layered onto the document by openapi.Handler (not
	// Build) — same as usagePathItem/keysPathItems/errorsPathItems — so
	// assert against the actually-served document.
	handler := openapi.Handler(server.V1Routes)
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)
	var served struct {
		Paths map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &served); err != nil {
		t.Fatalf("decode served openapi.json: %v", err)
	}
	if _, ok := served.Paths["/v1/jobs/{id}"]; !ok {
		t.Error("/v1/jobs/{id} does not appear in the served OpenAPI document")
	}
}
