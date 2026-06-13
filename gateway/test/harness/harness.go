// Package harness provides NewGatewayTestServer: a test helper that boots the
// full gateway middleware chain against real Postgres and Redis with an in-process
// worker stub. DSN and RedisURL are required; callers set Options fields as needed.
package harness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

var testSalt string

// TestSalt returns the per-process API key hash salt used by all harness instances.
func TestSalt() string { return testSalt }

const (
	// TestAPIKeyPrefix is the key prefix configured in every harness server.
	TestAPIKeyPrefix = "cru_"

	defaultWorkerTimeoutMS = 5000
	defaultProxyPoolSize   = 8
	defaultBodyLimitBytes  = 1 << 20
	defaultDBPoolSize      = 5

	serverBootTimeout    = 30 * time.Second
	cleanupTimeout       = 60 * time.Second // budget for customer cleanup including retry loop
	maxCleanupRetries    = 3
	cleanupRetryTimeout  = 10 * time.Second
)

func init() {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("harness: failed to generate test salt: " + err.Error())
	}
	testSalt = hex.EncodeToString(b)
}

// routesMu guards temporary modifications to server.V1Routes.
var routesMu sync.Mutex

// migrateOnce runs migrations once per test process for speed. If the first
// attempt fails, migrateOnceErr remains set and all subsequent tests in the
// same process fail; callers must ensure Postgres is ready before running
// tests. The SQL files use IF NOT EXISTS / DROP IF EXISTS for idempotency.
var (
	migrateOnce    sync.Once
	migrateOnceErr error
)

// Options configures a gateway test server.
type Options struct {
	// Routes overrides server.V1Routes. Nil means use production routes.
	// Non-nil callers must not use t.Parallel.
	Routes []openapi.RouteDescriptor

	// WorkerHandler handles POST /invoke calls forwarded by the gateway proxy.
	WorkerHandler http.Handler

	// DSN is a real Postgres connection string.
	DSN string

	// RedisURL is a real Redis connection string.
	RedisURL string

	// WorkerTimeoutMS caps the gateway→worker call. Defaults to 5000 ms.
	WorkerTimeoutMS int
}

// TestServer is a running gateway backed by real Postgres and Redis.
type TestServer struct {
	// Server is the gateway httptest.Server.
	Server *httptest.Server
	// Worker is the in-process worker httptest.Server.
	Worker *httptest.Server
	// DB gives direct Postgres access for assertion queries.
	DB *pgxpool.Pool
	// Redis gives direct Redis access for assertion queries.
	Redis *redis.Client
}

// NewGatewayTestServer boots the full gateway middleware chain via server.NewRouter
// against real Postgres and Redis and returns the started test server.
func NewGatewayTestServer(t *testing.T, opts Options) *TestServer {
	t.Helper()
	if opts.WorkerHandler == nil {
		t.Fatal("harness: WorkerHandler is required")
	}
	if opts.DSN == "" {
		t.Fatal("harness: DSN is required")
	}
	if !strings.HasPrefix(opts.DSN, "postgres://") && !strings.HasPrefix(opts.DSN, "postgresql://") {
		t.Fatalf("harness: DSN must be a postgres:// or postgresql:// URL, got: %s", opts.DSN)
	}
	if opts.RedisURL == "" {
		t.Fatal("harness: RedisURL is required")
	}
	if !strings.HasPrefix(opts.RedisURL, "redis://") && !strings.HasPrefix(opts.RedisURL, "rediss://") {
		t.Fatalf("harness: RedisURL must be a redis:// or rediss:// URL, got: %s", opts.RedisURL)
	}

	if opts.WorkerTimeoutMS <= 0 {
		opts.WorkerTimeoutMS = defaultWorkerTimeoutMS
	}

	workerSrv := httptest.NewServer(opts.WorkerHandler)
	t.Cleanup(workerSrv.Close)

	pool, err := db.NewPool(context.Background(), opts.DSN, defaultDBPoolSize)
	if err != nil {
		t.Fatalf("harness: open postgres: %v", err)
	}
	migrateOnce.Do(func() {
		applyCtx, applyCancel := context.WithTimeout(context.Background(), serverBootTimeout)
		migrateOnceErr = db.Apply(applyCtx, pool)
		applyCancel()
	})
	if migrateOnceErr != nil {
		pool.Close()
		t.Fatalf("harness: apply migrations: %v", migrateOnceErr)
	}
	t.Cleanup(pool.Close)

	redisCtx, redisCancel := context.WithTimeout(context.Background(), serverBootTimeout)
	rdb, err := cache.NewRedis(redisCtx, opts.RedisURL)
	redisCancel()
	if err != nil {
		t.Fatalf("harness: open redis: %v", err)
	}
	t.Cleanup(func() {
		if err := rdb.Close(); err != nil {
			t.Logf("harness: redis close: %v", err)
		}
	})

	cfg := &config.Config{
		BodyLimitBytes:  defaultBodyLimitBytes,
		DashboardOrigin: "http://localhost:3001",
		ErrorExposure:   "full",
		APIKeyPrefix:    TestAPIKeyPrefix,
		APIKeyHashSalt:  testSalt,
	}

	authStore := auth.NewStore(pool, rdb, testSalt)
	// authStore.Close stops the background last_used_at goroutine and drains its queue.
	// It does NOT close the injected pool or rdb; those are cleaned up by their own t.Cleanup above.
	t.Cleanup(authStore.Close)

	// proxy.Client has no Close() method; its http.Transport closes idle connections
	// automatically when workerSrv is shut down and the IdleConnTimeout (90 s) elapses.
	workerClient := proxy.New(
		workerSrv.URL,
		time.Duration(opts.WorkerTimeoutMS)*time.Millisecond,
		defaultProxyPoolSize,
	)

	bucket := ratelimit.New(rdb)
	plans := billing.NewPlanCache(pool)
	quotaTracker := quota.New(rdb)
	recorder := usage.NewRecorder(pool, quotaTracker)
	// dummy secret: no real Stripe webhook calls in tests; prefix differs per process.
	webhook := billing.NewWebhook("whsec_test_"+uuid.New().String(), pool)

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
		DB:            pool,
		ErrorRecorder: errorlog.New(pool),
	}

	// routesMu guards the swap of server.V1Routes. Lock only when custom routes
	// are provided; callers with opts.Routes != nil must NOT call t.Parallel.
	// Synchronous Lock/Unlock (no defer) keeps the critical section explicit:
	// deep-copy the backup before swapping, restore before releasing the lock.
	var handler http.Handler
	if opts.Routes != nil {
		routesMu.Lock()
		backup := append([]openapi.RouteDescriptor(nil), server.V1Routes...)
		server.V1Routes = opts.Routes
		handler = server.NewRouter(deps)
		server.V1Routes = backup
		routesMu.Unlock()
	} else {
		handler = server.NewRouter(deps)
	}

	gw := httptest.NewServer(handler)
	t.Cleanup(gw.Close)

	return &TestServer{
		Server: gw,
		Worker: workerSrv,
		DB:     pool,
		Redis:  rdb,
	}
}

// CreatePlan inserts or updates a plan row. ratePerMinute=0 means unlimited;
// monthlyCap=0 means unlimited (stored as NULL). Registers t.Cleanup to restore
// the plan to its pre-test state.
func (ts *TestServer) CreatePlan(t *testing.T, id string, ratePerMinute int64, monthlyCap int64) {
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

	var (
		prevRate int64
		prevCap  pgtype.Int8
		prevName string
		existed  bool
	)
	if err := ts.DB.QueryRow(ctx,
		`SELECT rate_limit_per_minute, monthly_unit_cap, display_name FROM plans WHERE id = $1`, id,
	).Scan(&prevRate, &prevCap, &prevName); err == nil {
		existed = true
	} else if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("harness: snapshot plan %q: %v", id, err)
	}

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
	`, id, "Test Plan "+id, ratePerMinute, capPtr); err != nil {
		t.Fatalf("harness: create plan %q: %v", id, err)
	}

	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if existed {
			var restoredCap *int64
			if prevCap.Valid {
				v := prevCap.Int64
				restoredCap = &v
			}
			if _, err := ts.DB.Exec(cctx,
				`UPDATE plans SET rate_limit_per_minute = $2, monthly_unit_cap = $3, display_name = $4 WHERE id = $1`,
				id, prevRate, restoredCap, prevName,
			); err != nil {
				t.Logf("harness: restore plan %q: %v", id, err)
				return
			}
		} else {
			if _, err := ts.DB.Exec(cctx, `DELETE FROM plans WHERE id = $1`, id); err != nil {
				t.Logf("harness: cleanup plan %q: %v", id, err)
				return
			}
		}
	})
}

// CreateCustomer inserts a customer on planID, generates and persists an API key,
// and returns (customerID, rawAPIKey). t.Cleanup removes all rows and Redis keys.
func (ts *TestServer) CreateCustomer(t *testing.T, email, planID string) (uuid.UUID, string) {
	t.Helper()
	if email == "" {
		t.Fatal("harness: CreateCustomer email must be non-empty")
	}
	if _, err := mail.ParseAddress(email); err != nil {
		t.Fatalf("harness: CreateCustomer email %q is not a valid RFC 5322 address: %v", email, err)
	}
	if planID == "" {
		t.Fatal("harness: CreateCustomer planID must be non-empty")
	}
	// Existence check: SELECT 1 + Scan(new(int)) is the idiomatic pgx check.
	var planFound int
	planCtx, planCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer planCancel()
	err := ts.DB.QueryRow(planCtx,
		`SELECT 1 FROM plans WHERE id = $1`, planID,
	).Scan(&planFound)
	if errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("harness: CreateCustomer planID %q does not exist", planID)
	}
	if err != nil {
		t.Fatalf("harness: CreateCustomer planID %q lookup failed: %v", planID, err)
	}
	customerID := uuid.New()
	// Capture month after customerID so createdMonth is conceptually part of this customer's record.
	createdMonth := time.Now().UTC().Format("2006-01")
	insertCtx, insertCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer insertCancel()
	_, err = ts.DB.Exec(insertCtx,
		`INSERT INTO customers (id, email, plan_id) VALUES ($1, $2, $3)`,
		customerID, email, planID,
	)
	if err != nil {
		t.Fatalf("harness: insert customer %s: %v", customerID, err)
	}

	full, prefix, err := auth.Generate(TestAPIKeyPrefix)
	if err != nil {
		t.Fatalf("harness: generate api key: %v", err)
	}
	hash := auth.Hash(testSalt, full)
	_, err = ts.DB.Exec(insertCtx,
		`INSERT INTO api_keys (customer_id, prefix, hash) VALUES ($1, $2, $3)`,
		customerID, prefix, hash,
	)
	if err != nil {
		t.Fatalf("harness: insert api key for customer %s: %v", customerID, err)
	}

	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		cleanupErr := func(table string, e error) {
			if e == nil {
				return
			}
			// Transient FK violations (23503) from the async errorlog.Record goroutine are
			// expected and logged only; the retry loop below handles the api_keys case.
			var pgErr *pgconn.PgError
			if errors.As(e, &pgErr) && pgErr.Code == "23503" {
				t.Logf("harness: cleanup FK violation %s for customer %s: %v", table, customerID, e)
				return
			}
			// Context cancellation/deadline means the cleanup budget expired; log but don't fail.
			if errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) {
				t.Logf("harness: cleanup timeout %s for customer %s: %v", table, customerID, e)
				return
			}
			t.Errorf("harness: cleanup %s for customer %s: %v", table, customerID, e)
		}
		// Delete children before parents (error_events.api_key_id REFERENCES api_keys ON DELETE NO ACTION).
		_, err := ts.DB.Exec(cctx, `DELETE FROM usage_events WHERE customer_id = $1`, customerID)
		cleanupErr("usage_events", err)
		_, err = ts.DB.Exec(cctx, `DELETE FROM idempotency_keys WHERE customer_id = $1`, customerID)
		cleanupErr("idempotency_keys", err)
		_, err = ts.DB.Exec(cctx, `DELETE FROM error_events WHERE customer_id = $1`, customerID)
		cleanupErr("error_events", err)
		_, err = ts.DB.Exec(cctx, `DELETE FROM webhook_deliveries WHERE endpoint_id IN (SELECT id FROM webhook_endpoints WHERE customer_id = $1)`, customerID)
		cleanupErr("webhook_deliveries", err)
		_, err = ts.DB.Exec(cctx, `DELETE FROM webhook_endpoints WHERE customer_id = $1`, customerID)
		cleanupErr("webhook_endpoints", err)
		// Retry deleting api_keys: the async errorlog goroutine (2s timeout) may insert an
		// error_events row after the DELETE above, causing a transient FK violation.
		var finalKeyErr error
		for attempt := 1; attempt <= maxCleanupRetries; attempt++ {
			if cctx.Err() != nil {
				finalKeyErr = cctx.Err()
				break
			}
			retryCtx, retryCancel := context.WithTimeout(cctx, cleanupRetryTimeout)
			_, retryErr := ts.DB.Exec(retryCtx, `DELETE FROM api_keys WHERE customer_id = $1`, customerID)
			retryCancel()
			if retryErr == nil {
				finalKeyErr = nil
				break
			}
			var pgErr *pgconn.PgError
			if errors.As(retryErr, &pgErr) && pgErr.Code == "23503" {
				fixCtx, fixCancel := context.WithTimeout(cctx, 5*time.Second)
				_, delErr := ts.DB.Exec(fixCtx, `DELETE FROM error_events WHERE customer_id = $1`, customerID)
				fixCancel()
				if delErr != nil {
					t.Logf("harness: cleanup error_events retry for customer %s: %v", customerID, delErr)
				}
				if attempt == maxCleanupRetries {
					finalKeyErr = retryErr
				}
				continue
			}
			// Non-transient error; stop retrying.
			finalKeyErr = retryErr
			break
		}
		if finalKeyErr != nil {
			t.Logf("harness: cleanup api_keys for customer %s: %v", customerID, finalKeyErr)
		}
		_, err = ts.DB.Exec(cctx, `DELETE FROM customers WHERE id = $1`, customerID)
		cleanupErr("customers", err)
		// Key formats (must match production source):
		//   quota:<uuid>:<YYYY-MM>  quota/tracker.go monthKey()
		//   rl:<uuid>               ratelimit/bucket.go Allow()
		//   auth:<prefix>           auth/store.go cacheKey()
		quotaKey := "quota:" + customerID.String() + ":" + createdMonth
		rlKey := "rl:" + customerID.String()
		authKey := "auth:" + prefix
		redisKeys := []string{quotaKey, rlKey, authKey}
		// Guard against tests spanning a UTC month boundary: also delete the current-month quota key.
		if nowMonth := time.Now().UTC().Format("2006-01"); nowMonth != createdMonth {
			redisKeys = append(redisKeys, "quota:"+customerID.String()+":"+nowMonth)
		}
		if delErr := ts.Redis.Del(cctx, redisKeys...).Err(); delErr != nil {
			t.Logf("harness: cleanup redis keys for customer %s: %v", customerID, delErr)
		}
	})

	return customerID, full
}

// CountUsageEvents returns the number of usage_events rows for customerID.
func (ts *TestServer) CountUsageEvents(t *testing.T, customerID uuid.UUID) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int
	err := ts.DB.QueryRow(ctx,
		`SELECT COUNT(*) FROM usage_events WHERE customer_id = $1`, customerID,
	).Scan(&n)
	if err != nil {
		t.Fatalf("harness: count usage_events: %v", err)
	}
	return n
}

// CountErrorEvents returns the number of error_events rows for customerID.
func (ts *TestServer) CountErrorEvents(t *testing.T, customerID uuid.UUID) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int
	err := ts.DB.QueryRow(ctx,
		`SELECT COUNT(*) FROM error_events WHERE customer_id = $1`, customerID,
	).Scan(&n)
	if err != nil {
		t.Fatalf("harness: count error_events: %v", err)
	}
	return n
}

// CountIdempotencyKeys returns the number of idempotency_keys rows for customerID and key.
// Column name idempotency_key matches the schema (migrations/0007_idempotency_keys.sql).
func (ts *TestServer) CountIdempotencyKeys(t *testing.T, customerID uuid.UUID, idempotencyKey string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int
	err := ts.DB.QueryRow(ctx,
		`SELECT COUNT(*) FROM idempotency_keys WHERE customer_id = $1 AND idempotency_key = $2`,
		customerID, idempotencyKey,
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
