// Package harness provides NewGatewayTestServer, a reusable test helper that boots
// the full gateway middleware chain (server.NewRouter) against real Postgres and Redis,
// plus an in-process worker stub. Tests that exercise real storage catch schema–query
// drift and middleware interaction effects that mocks cannot.
//
// Usage contract:
//   - DSN and RedisURL must point at real, running services (no mocks).
//   - NewGatewayTestServer is NOT safe for t.Parallel when Options.Routes is non-nil
//     because it temporarily swaps the package-level server.V1Routes.
//   - Each CreateCustomer call registers a t.Cleanup that removes all test rows for
//     that customer from usage_events, idempotency_keys, error_events,
//     webhook_deliveries, webhook_endpoints, api_keys, customers, and the customer's
//     Redis quota/rate-limit keys.
//   - Each CreatePlan call registers a t.Cleanup that deletes or restores the plan row.
package harness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
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

// TestSalt is the API key hash salt used by all harness instances.
// Generated at process start via crypto/rand; guaranteed ≥ 64 chars (32 bytes hex-encoded).
// Each test process gets a unique salt; auth.Hash(TestSalt, ...) and Store lookups both
// use this value, so per-process uniqueness is safe.
var TestSalt string

const (
	// TestAPIKeyPrefix is the API key prefix used by all harness instances.
	// Mirrors config.Config.APIKeyPrefix set inside NewGatewayTestServer so both
	// the gateway auth middleware and CreateCustomer use the identical value.
	TestAPIKeyPrefix = "cru_"

	defaultWorkerTimeoutMS = 5000      // generous default; set low in Options to test timeout scenarios
	defaultProxyPoolSize   = 8
	defaultBodyLimitBytes  = 1 << 20 // 1 MB
	defaultDBPoolSize      = 5
)

func init() {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("harness: failed to generate test salt: " + err.Error())
	}
	TestSalt = hex.EncodeToString(b) // 64 hex chars, well above 32-byte minimum
}

// routesMu guards temporary modifications to server.V1Routes.
// NewRouter reads the package-level var at call time, so any caller that
// injects custom routes must hold this lock across the NewRouter call.
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

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
	// endpoint is not exercised by the scenario suite. Use a random suffix so the
	// secret differs per test process and is clearly not a reused static value.
	webhook := billing.NewWebhook("test-webhook-secret-"+uuid.New().String(), pool)

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

	// When Routes is non-nil, hold routesMu across the V1Routes swap and the
	// NewRouter call so the two are atomic. Nil-Routes callers do not touch
	// V1Routes and need no lock. Callers with non-nil Routes must not use
	// t.Parallel (see Options doc). Defers are LIFO: V1Routes is restored before
	// the mutex is released.
	if opts.Routes != nil {
		routesMu.Lock()
		defer routesMu.Unlock()
		// Copy the current slice so we can restore it after NewRouter reads the var.
		// We only ever replace the slice variable itself, never mutate individual elements.
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
	if ratePerMinute < 0 {
		t.Fatal("harness: CreatePlan ratePerMinute must be >= 0 (use 0 for unlimited)")
	}
	if monthlyCap < 0 {
		t.Fatal("harness: CreatePlan monthlyCap must be >= 0 (use 0 for unlimited)")
	}
	ctx := context.Background()

	// Snapshot any pre-existing plan so cleanup can restore it rather than deleting a
	// row that wasn't created by this call (e.g. a seeded "free" or "pro" plan).
	var (
		prevRate int
		prevCap  pgtype.Int8 // nullable int8; Valid=false when NULL (unlimited)
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
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if existed {
			// Restore the original rate/cap. prevCap.Valid=false means the original
			// cap was NULL (unlimited), so we restore NULL by passing a nil *int64.
			var restoredCap *int64
			if prevCap.Valid {
				v := prevCap.Int64
				restoredCap = &v
			}
			if _, err := ts.DB.Exec(cctx,
				`UPDATE plans SET rate_limit_per_minute = $2, monthly_unit_cap = $3 WHERE id = $1`,
				id, prevRate, restoredCap,
			); err != nil {
				t.Errorf("harness: restore plan %q: %v", id, err)
			}
		} else {
			if _, err := ts.DB.Exec(cctx, `DELETE FROM plans WHERE id = $1`, id); err != nil {
				t.Errorf("harness: cleanup plan %q: %v", id, err)
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
	if email == "" {
		t.Fatal("harness: CreateCustomer email must be non-empty")
	}
	if planID == "" {
		t.Fatal("harness: CreateCustomer planID must be non-empty")
	}
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
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		logErr := func(table string, e error) {
			if e == nil {
				return
			}
			// Transient FK violations (PostgreSQL code 23503) are expected when the async
			// errorlog.Record goroutine inserts an error_events row between the
			// error_events DELETE and the api_keys DELETE. Log only; the retry loop
			// handles the FK case. Unexpected errors (schema bugs, connection failures,
			// permission issues) are failures and must not be silently swallowed.
			var pgErr *pgconn.PgError
			if errors.As(e, &pgErr) && pgErr.Code == "23503" {
				t.Logf("harness: cleanup FK violation %s for customer %s: %v", table, customerID, e)
				return
			}
			t.Errorf("harness: cleanup %s for customer %s: %v", table, customerID, e)
		}
		// Delete children before parents to satisfy FK constraints.
		// error_events.api_key_id REFERENCES api_keys(id) NO ACTION — must delete
		// error_events before api_keys.
		_, err := ts.DB.Exec(cctx, `DELETE FROM usage_events      WHERE customer_id = $1`, customerID)
		logErr("usage_events", err)
		_, err = ts.DB.Exec(cctx, `DELETE FROM idempotency_keys   WHERE customer_id = $1`, customerID)
		logErr("idempotency_keys", err)
		_, err = ts.DB.Exec(cctx, `DELETE FROM error_events       WHERE customer_id = $1`, customerID)
		logErr("error_events", err)
		_, err = ts.DB.Exec(cctx, `DELETE FROM webhook_deliveries WHERE endpoint_id IN (SELECT id FROM webhook_endpoints WHERE customer_id = $1)`, customerID)
		logErr("webhook_deliveries", err)
		_, err = ts.DB.Exec(cctx, `DELETE FROM webhook_endpoints  WHERE customer_id = $1`, customerID)
		logErr("webhook_endpoints", err)
		// Bounded retry with fresh per-attempt context: the async errorlog.Record goroutine
		// (2 s timeout) may insert an error_events row between the error_events DELETE
		// above and this api_keys DELETE, causing a transient FK violation. Re-delete
		// error_events then retry api_keys. Each attempt gets its own context so an
		// expired parent deadline does not doom subsequent retries.
		var apiKeyErr error
		for attempt := 1; attempt <= 3; attempt++ {
			retryCtx, retryCancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, apiKeyErr = ts.DB.Exec(retryCtx, `DELETE FROM api_keys WHERE customer_id = $1`, customerID)
			retryCancel()
			if apiKeyErr == nil {
				break
			}
			logErr(fmt.Sprintf("api_keys (attempt %d)", attempt), apiKeyErr)
			fixCtx, fixCancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, delErr := ts.DB.Exec(fixCtx, `DELETE FROM error_events WHERE customer_id = $1`, customerID)
			fixCancel()
			if delErr != nil {
				logErr("error_events (retry)", delErr)
			}
		}
		if apiKeyErr != nil {
			logErr("api_keys", apiKeyErr)
		}
		_, err = ts.DB.Exec(cctx, `DELETE FROM customers WHERE id = $1`, customerID)
		logErr("customers", err)
		// Flush quota counter and rate-limit key to avoid polluting next run.
		// Formats verified against production source (do not change without updating both):
		//   quota key:     quota/tracker.go monthKey()  → "quota:<uuid>:<YYYY-MM>"
		//   ratelimit key: ratelimit/bucket.go Allow()  → "rl:<uuid>"
		quotaKey := "quota:" + customerID.String() + ":" + createdMonth
		rlKey := "rl:" + customerID.String()
		if delErr := ts.Redis.Del(cctx, quotaKey).Err(); delErr != nil {
			t.Logf("harness: cleanup quota key for customer %s: %v", customerID, delErr)
		}
		if delErr := ts.Redis.Del(cctx, rlKey).Err(); delErr != nil {
			t.Logf("harness: cleanup rate-limit key for customer %s: %v", customerID, delErr)
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
// the given customerID and key value. The table is created by
// gateway/migrations/0007_idempotency_keys.sql with a UNIQUE(customer_id, idempotency_key)
// constraint, so this count is always 0 or 1 for a successful call.
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
