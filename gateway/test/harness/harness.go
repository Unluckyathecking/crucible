// Package harness provides NewGatewayTestServer, a reusable test helper that boots
// the full gateway middleware chain (server.NewRouter) against real Postgres and Redis,
// plus an in-process worker stub. Product clones copy this package verbatim to assert
// end-to-end behavior and resulting DB state without mocking storage.
//
// Usage contract:
//   - DSN and RedisURL must point at real, running services (no mocks).
//   - NewGatewayTestServer is NOT safe for t.Parallel when Options.Routes is non-nil
//     because it temporarily swaps the package-level server.V1Routes.
//   - Each CreateCustomer call registers a t.Cleanup that removes all test rows for
//     that customer from usage_events, idempotency_keys, error_events, api_keys,
//     customers, and the customer's Redis quota/rate-limit keys.
//   - Each CreatePlan call registers a t.Cleanup that deletes the plan row.
package harness

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	"github.com/Unluckyathecking/crucible/gateway/internal/billing"
	"github.com/Unluckyathecking/crucible/gateway/internal/cache"
	"github.com/Unluckyathecking/crucible/gateway/internal/config"
	"github.com/Unluckyathecking/crucible/gateway/internal/db"
	"github.com/Unluckyathecking/crucible/gateway/internal/errorlog"
	"github.com/Unluckyathecking/crucible/gateway/internal/openapi"
	"github.com/Unluckyathecking/crucible/gateway/internal/proxy"
	"github.com/Unluckyathecking/crucible/gateway/internal/quota"
	"github.com/Unluckyathecking/crucible/gateway/internal/ratelimit"
	"github.com/Unluckyathecking/crucible/gateway/internal/server"
	"github.com/Unluckyathecking/crucible/gateway/internal/usage"
)

const (
	// TestSalt is the API key hash salt used by all harness instances.
	// Must be >= 32 bytes (config.Load enforces this at runtime).
	TestSalt = "crucible-harness-test-salt-min32!!"

	// TestAPIKeyPrefix is the API key prefix used by all harness instances.
	// Mirrors config.Config.APIKeyPrefix set inside NewGatewayTestServer so both
	// the gateway auth middleware and CreateCustomer use the same value.
	TestAPIKeyPrefix = "cru_"

	defaultWorkerTimeoutMS = 5000      // generous default; set low in Options to test timeout scenarios
	defaultProxyPoolSize   = 8
	defaultBodyLimitBytes  = 1 << 20 // 1 MB
	defaultDBPoolSize      = 5
)

// routesMu guards temporary modifications to server.V1Routes.
// Required because NewRouter reads the package-level var at call time, so any
// caller that injects custom routes must hold this lock across NewRouter.
var routesMu sync.Mutex

// Options configures a gateway test server.
type Options struct {
	// Routes overrides server.V1Routes for this server's lifetime.
	// Nil means use the production V1Routes unchanged.
	// When non-nil: NewGatewayTestServer holds routesMu across NewRouter; callers
	// must not call it concurrently with t.Parallel when Routes is set.
	Routes []openapi.RouteDescriptor

	// WorkerHandler handles POST /invoke calls the gateway proxies to the worker.
	WorkerHandler http.Handler

	// DSN is a real Postgres connection string (e.g. from POSTGRES_DSN env var).
	DSN string

	// RedisURL is a real Redis connection string (e.g. from REDIS_URL env var).
	RedisURL string

	// WorkerTimeoutMS caps the gateway→worker HTTP call. Defaults to 5000 ms.
	// Set this low (e.g. 100) combined with a slow WorkerHandler to trigger the
	// timeout scenario without blocking the test for a long time.
	WorkerTimeoutMS int
}

// TestServer is a running gateway backed by real Postgres and Redis.
// All resources are registered with t.Cleanup; callers need not close anything manually.
type TestServer struct {
	// Server is the gateway httptest.Server. Use Server.URL to construct request URLs.
	Server *httptest.Server
	// Worker is the in-process worker httptest.Server (available for inspection).
	Worker *httptest.Server
	// DB gives direct Postgres access for assertion queries (e.g. COUNT usage_events).
	DB *pgxpool.Pool
	// Redis gives direct Redis access for assertion queries.
	Redis *redis.Client
}

// NewGatewayTestServer boots the full gateway middleware chain via server.NewRouter against
// real Postgres and Redis and returns the started test server. Migrations are applied on
// every call; each migration file must be idempotent (CREATE TABLE IF NOT EXISTS,
// INSERT ON CONFLICT DO NOTHING). Do not call concurrently against the same schema.
func NewGatewayTestServer(t *testing.T, opts Options) *TestServer {
	t.Helper()
	ctx := context.Background()

	if opts.WorkerHandler == nil {
		t.Fatal("harness: WorkerHandler is required")
	}
	if opts.DSN == "" {
		t.Fatal("harness: DSN is required")
	}
	if opts.RedisURL == "" {
		t.Fatal("harness: RedisURL is required")
	}

	if opts.WorkerTimeoutMS <= 0 {
		opts.WorkerTimeoutMS = defaultWorkerTimeoutMS
	}

	// In-process worker: the gateway proxies /invoke calls here.
	workerSrv := httptest.NewServer(opts.WorkerHandler)
	t.Cleanup(workerSrv.Close)

	// Real Postgres. Apply runs every migration file; each file uses
	// CREATE TABLE IF NOT EXISTS / INSERT ON CONFLICT DO NOTHING (see
	// gateway/migrations/*.sql), so it is safe and fast to call repeatedly.
	// Running it here ensures local test runs work without a separate setup step;
	// in CI the workflow pre-applies migrations, making this call a quick no-op.
	pool, err := db.NewPool(ctx, opts.DSN, defaultDBPoolSize)
	if err != nil {
		t.Fatalf("harness: open postgres: %v", err)
	}
	if err := db.Apply(ctx, pool); err != nil {
		t.Fatalf("harness: apply migrations: %v", err)
	}
	t.Cleanup(pool.Close)

	// Real Redis.
	rdb, err := cache.NewRedis(ctx, opts.RedisURL)
	if err != nil {
		t.Fatalf("harness: open redis: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })

	// Minimal config: only the fields NewRouter and the middleware chain read.
	// We build it directly (without config.Load) to avoid requiring Stripe env vars
	// that are irrelevant for end-to-end functional testing.
	cfg := &config.Config{
		BodyLimitBytes:  defaultBodyLimitBytes,
		DashboardOrigin: "http://localhost:3001",
		ErrorExposure:   "full", // expose worker error details in tests
		APIKeyPrefix:    TestAPIKeyPrefix,
		APIKeyHashSalt:  TestSalt,
	}

	authStore := auth.NewStore(pool, rdb, TestSalt)
	t.Cleanup(authStore.Close)

	workerClient := proxy.New(
		workerSrv.URL,
		time.Duration(opts.WorkerTimeoutMS)*time.Millisecond,
		defaultProxyPoolSize,
	)

	bucket := ratelimit.New(rdb)
	plans := billing.NewPlanCache(pool)
	quotaTracker := quota.New(rdb)
	recorder := usage.NewRecorder(pool, quotaTracker)
	// dummy secret: no real Stripe calls are made in e2e tests; the /webhooks/stripe
	// endpoint is not exercised by the scenario suite.
	webhook := billing.NewWebhook("test-webhook-secret-dummy", pool)

	deps := &server.Deps{
		Cfg:           cfg,
		Proxy:         workerClient,
		Auth:          authStore,
		Bucket:        bucket,
		Plans:         plans,
		Recorder:      recorder,
		Webhook:       webhook,
		Quota:         quotaTracker,
		Redis:         &redisPinger{rdb},
		PG:            &pgPinger{pool},
		// DB non-nil: activates the idempotency middleware and webhookDeliveries route.
		DB:            pool,
		ErrorRecorder: errorlog.New(pool),
	}

	// routesMu is always held across NewRouter so a concurrent default-path call
	// cannot read V1Routes while a custom-routes caller has swapped them.
	// Defers are LIFO: the V1Routes restore (if registered below) runs before Unlock.
	routesMu.Lock()
	defer routesMu.Unlock()
	if opts.Routes != nil {
		// Shallow copy is correct here: we replace the slice variable entirely
		// (server.V1Routes = opts.Routes / server.V1Routes = backup) and never mutate
		// individual RouteDescriptor fields. Go strings are immutable; *Schema pointers
		// are read by NewRouter but not mutated. Callers must treat opts.Routes as
		// immutable after this call.
		backup := append([]openapi.RouteDescriptor(nil), server.V1Routes...)
		defer func() { server.V1Routes = backup }()
		server.V1Routes = opts.Routes
	}
	handler := server.NewRouter(deps)

	gw := httptest.NewServer(handler)
	t.Cleanup(gw.Close)

	return &TestServer{
		Server: gw,
		Worker: workerSrv,
		DB:     pool,
		Redis:  rdb,
	}
}

// CreatePlan inserts or updates a plan row for use in tests.
// ratePerMinute=0 means unlimited. monthlyCap=0 means unlimited: the column is
// stored as NULL, which pgx scans into int64(0); quota.Tracker.Reserve treats
// cap<=0 as "always admitted" (no ceiling). Pass monthlyCap>0 for a finite cap.
// Uses ON CONFLICT DO UPDATE so repeated test runs against the same DB are safe.
// Registers t.Cleanup to restore the plan to its pre-test state: rows that did not
// exist are deleted; pre-existing rows (e.g. seeded "free" / "pro" plans) have their
// original rate_limit_per_minute and monthly_unit_cap restored rather than being
// deleted, so the shared plan table is not corrupted for subsequent tests.
func (ts *TestServer) CreatePlan(t *testing.T, id string, ratePerMinute int, monthlyCap int64) {
	t.Helper()
	if id == "" {
		t.Fatal("harness: CreatePlan id must be non-empty")
	}
	ctx := context.Background()

	// Snapshot any pre-existing plan so cleanup can restore it rather than deleting a
	// row that wasn't created by this call (e.g. a seeded "free" or "pro" plan).
	var (
		prevRate int
		prevCap  *int64
		existed  bool
	)
	if err := ts.DB.QueryRow(ctx,
		`SELECT rate_limit_per_minute, monthly_unit_cap FROM plans WHERE id = $1`, id,
	).Scan(&prevRate, &prevCap); err == nil {
		existed = true
	}

	// NULL signals unlimited in the schema; the quota middleware reads this as 0 (always admit).
	var capPtr *int64
	if monthlyCap > 0 {
		capPtr = &monthlyCap
	}
	if _, err := ts.DB.Exec(ctx, `
		INSERT INTO plans (id, display_name, rate_limit_per_minute, monthly_unit_cap)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE
		  SET rate_limit_per_minute = EXCLUDED.rate_limit_per_minute,
		      monthly_unit_cap      = EXCLUDED.monthly_unit_cap
	`, id, fmt.Sprintf("Test Plan %s", id), ratePerMinute, capPtr); err != nil {
		t.Fatalf("harness: create plan %q: %v", id, err)
	}

	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if existed {
			// Restore the original rate/cap rather than deleting a pre-existing plan row.
			if _, err := ts.DB.Exec(cctx,
				`UPDATE plans SET rate_limit_per_minute = $2, monthly_unit_cap = $3 WHERE id = $1`,
				id, prevRate, prevCap,
			); err != nil {
				t.Logf("harness: restore plan %q: %v", id, err)
			}
		} else {
			if _, err := ts.DB.Exec(cctx, `DELETE FROM plans WHERE id = $1`, id); err != nil {
				t.Logf("harness: cleanup plan %q: %v", id, err)
			}
		}
	})
}

// CreateCustomer inserts a customer on planID, generates and persists an API key hashed
// with TestSalt, and returns (customerID, rawAPIKey). rawAPIKey is the full Bearer token
// value; use it directly in Authorization headers.
//
// t.Cleanup removes all DB rows belonging to this customer and flushes their Redis keys.
func (ts *TestServer) CreateCustomer(t *testing.T, email, planID string) (uuid.UUID, string) {
	t.Helper()
	ctx := context.Background()
	// Capture the month now so the cleanup closure deletes the same month's quota key
	// even if the test happens to span a UTC month boundary.
	createdMonth := time.Now().UTC().Format("2006-01")

	customerID := uuid.New()
	_, err := ts.DB.Exec(ctx,
		`INSERT INTO customers (id, email, plan_id) VALUES ($1, $2, $3)`,
		customerID, email, planID,
	)
	if err != nil {
		t.Fatalf("harness: insert customer: %v", err)
	}

	full, prefix, err := auth.Generate(TestAPIKeyPrefix)
	if err != nil {
		t.Fatalf("harness: generate api key: %v", err)
	}
	hash := auth.Hash(TestSalt, full)
	_, err = ts.DB.Exec(ctx,
		`INSERT INTO api_keys (customer_id, prefix, hash) VALUES ($1, $2, $3)`,
		customerID, prefix, hash,
	)
	if err != nil {
		t.Fatalf("harness: insert api key: %v", err)
	}

	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		logErr := func(table string, e error) {
			if e != nil {
				t.Logf("harness: cleanup %s for customer %s: %v", table, customerID, e)
			}
		}
		// Explicit child-before-parent ordering. api_keys comes before error_events because
		// error_events.api_key_id uses ON DELETE SET NULL (0013_error_events_fk_setnull
		// migration): deleting api_keys nullifies any error_events reference rather than
		// blocking with a FK constraint. This also closes the async-goroutine race in
		// errorlog.Record — any in-flight insert fires after api_keys are gone, hits a FK
		// violation on the now-absent api_key_id, and logs "error event record failed"
		// (the existing graceful-failure path), so no row is orphaned.
		var err error
		_, err = ts.DB.Exec(cctx, `DELETE FROM usage_events      WHERE customer_id = $1`, customerID)
		logErr("usage_events", err)
		_, err = ts.DB.Exec(cctx, `DELETE FROM idempotency_keys   WHERE customer_id = $1`, customerID)
		logErr("idempotency_keys", err)
		_, err = ts.DB.Exec(cctx, `DELETE FROM webhook_deliveries WHERE endpoint_id IN (SELECT id FROM webhook_endpoints WHERE customer_id = $1)`, customerID)
		logErr("webhook_deliveries", err)
		_, err = ts.DB.Exec(cctx, `DELETE FROM webhook_endpoints  WHERE customer_id = $1`, customerID)
		logErr("webhook_endpoints", err)
		_, err = ts.DB.Exec(cctx, `DELETE FROM api_keys           WHERE customer_id = $1`, customerID)
		logErr("api_keys", err)
		_, err = ts.DB.Exec(cctx, `DELETE FROM error_events       WHERE customer_id = $1`, customerID)
		logErr("error_events", err)
		_, err = ts.DB.Exec(cctx, `DELETE FROM customers WHERE id = $1`, customerID)
		logErr("customers", err)
		// Flush quota counter and rate-limit sorted set to avoid polluting next run.
		// Formats verified against production source (do not change without updating both):
		//   quota key:    quota/tracker.go monthKey()  → "quota:<uuid>:<YYYY-MM>"
		//   ratelimit key: ratelimit/bucket.go Allow()  → "rl:<uuid>" (fmt.Sprintf("rl:%s", id))
		quotaKey := "quota:" + customerID.String() + ":" + createdMonth
		rlKey := "rl:" + customerID.String()
		if err := ts.Redis.Del(cctx, quotaKey, rlKey).Err(); err != nil {
			// Log but do not fail: stale Redis keys expire naturally and UUID-scoped
			// keys cannot pollute other customers.
			t.Logf("harness: redis cleanup for customer %s: %v", customerID, err)
		}
	})

	return customerID, full
}

// CountUsageEvents returns the number of usage_events rows written for customerID.
// Useful in assertions after a request to verify billing side-effects.
func (ts *TestServer) CountUsageEvents(t *testing.T, customerID uuid.UUID) int {
	t.Helper()
	var n int
	err := ts.DB.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM usage_events WHERE customer_id = $1`, customerID,
	).Scan(&n)
	if err != nil {
		t.Fatalf("harness: count usage_events: %v", err)
	}
	return n
}

// CountErrorEvents returns the number of error_events rows written for customerID.
// Useful in isolation assertions to verify that one customer's errors don't bleed
// into another customer's error history.
func (ts *TestServer) CountErrorEvents(t *testing.T, customerID uuid.UUID) int {
	t.Helper()
	var n int
	err := ts.DB.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM error_events WHERE customer_id = $1`, customerID,
	).Scan(&n)
	if err != nil {
		t.Fatalf("harness: count error_events: %v", err)
	}
	return n
}

// CountIdempotencyKeys returns the number of idempotency_keys rows stored for
// the given customerID and idempotency key value.
func (ts *TestServer) CountIdempotencyKeys(t *testing.T, customerID uuid.UUID, key string) int {
	t.Helper()
	var n int
	err := ts.DB.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM idempotency_keys WHERE customer_id = $1 AND idempotency_key = $2`,
		customerID, key,
	).Scan(&n)
	if err != nil {
		t.Fatalf("harness: count idempotency_keys: %v", err)
	}
	return n
}

// redisPinger adapts *redis.Client to server.HealthChecker.
type redisPinger struct{ c *redis.Client }

func (r *redisPinger) Ping(ctx context.Context) error { return r.c.Ping(ctx).Err() }

// pgPinger adapts *pgxpool.Pool to server.HealthChecker.
type pgPinger struct{ p *pgxpool.Pool }

func (p *pgPinger) Ping(ctx context.Context) error { return p.p.Ping(ctx) }
