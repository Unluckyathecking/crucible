// Package server wires the HTTP router and per-route handlers for the Crucible gateway.
//
// Per-product clones edit ONE location: the "per-product routes" block in NewRouter.
// One line per endpoint maps an HTTP path to an opaque operation string forwarded to the worker.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"
	"github.com/go-chi/httprate"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"

	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/Unluckyathecking/crucible/gateway/internal/apierror"
	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	"github.com/Unluckyathecking/crucible/gateway/internal/billing"
	"github.com/Unluckyathecking/crucible/gateway/internal/config"
	"github.com/Unluckyathecking/crucible/gateway/internal/errorlog"
	"github.com/Unluckyathecking/crucible/gateway/internal/idempotency"
	"github.com/Unluckyathecking/crucible/gateway/internal/jobs"
	mw "github.com/Unluckyathecking/crucible/gateway/internal/middleware"
	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
	"github.com/Unluckyathecking/crucible/gateway/internal/openapi"
	"github.com/Unluckyathecking/crucible/gateway/internal/operator"
	"github.com/Unluckyathecking/crucible/gateway/internal/paging"
	"github.com/Unluckyathecking/crucible/gateway/internal/proxy"
	"github.com/Unluckyathecking/crucible/gateway/internal/quota"
	"github.com/Unluckyathecking/crucible/gateway/internal/ratelimit"
	"github.com/Unluckyathecking/crucible/gateway/internal/respcache"
	"github.com/Unluckyathecking/crucible/gateway/internal/selferrors"
	"github.com/Unluckyathecking/crucible/gateway/internal/selfusage"
	"github.com/Unluckyathecking/crucible/gateway/internal/selfusagedetail"
	"github.com/Unluckyathecking/crucible/gateway/internal/tracing"
	"github.com/Unluckyathecking/crucible/gateway/internal/usage"
	"github.com/Unluckyathecking/crucible/gateway/internal/validate"
	"github.com/Unluckyathecking/crucible/gateway/internal/webhookout"
)

// planIDRE mirrors the dashboard's PLAN_ID_RE: lowercase alphanumeric + hyphens, max 32 chars.
// The gateway is the trust boundary; revalidating here prevents DB probing via arbitrary plan IDs.
var planIDRE = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// HealthChecker wraps a dependency that can be pinged for connectivity verification.
type HealthChecker interface {
	Ping(ctx context.Context) error
}

// BillingService is the subset of billing.CheckoutClient used by the billing
// route handlers. Extracted as an interface so tests can inject stubs without
// requiring a live Stripe API or database.
type BillingService interface {
	CreateCheckoutSession(ctx context.Context, customerID, planID string) (string, error)
	CreatePortalSession(ctx context.Context, stripeCustomerID string) (string, error)
	LookupStripeCustomerID(ctx context.Context, customerID string) (string, error)
}

// readyzResponse is the JSON envelope for the readiness endpoint.
type readyzResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

// Deps bundles the constructed components the router needs. Easier to evolve than a long arg list.
type Deps struct {
	Cfg      *config.Config
	Proxy    *proxy.Client
	Auth     *auth.Store
	Bucket   *ratelimit.Bucket
	Plans    *billing.PlanCache
	Recorder *usage.Recorder
	Webhook  *billing.Webhook
	Quota    *quota.Tracker
	Redis    HealthChecker
	PG       HealthChecker
	// DB is optional. When set, the idempotency middleware is active on /v1 routes.
	// When nil (default when main.go is unmodified), the middleware is a pass-through.
	DB *pgxpool.Pool
	// Emitter is the shared outbound webhook emitter. When set (main.go's
	// production wiring), NewRouter reuses this single instance instead of
	// constructing its own — the same instance cmd/gateway/main.go also injects
	// into jobs.NewExecutor, so job.succeeded/job.failed webhooks share one
	// delivery worker with every other outbound event rather than a second,
	// independent Emitter. When nil (e.g. a test harness that only sets DB),
	// NewRouter falls back to constructing one from d.DB itself, preserving the
	// previous default behavior.
	Emitter *webhookout.Emitter
	// RespCache is optional. When set, /v1 routes that declare a positive TTL in
	// RespCacheTTLSeconds (routes_table.go) are served from the gateway's
	// content-addressed result cache on a hit. When nil (default when main.go is
	// unmodified), the respcache middleware is a strict pass-through.
	RespCache *respcache.Store
	// Checkout is optional. When set, POST /v1/billing/checkout and
	// POST /v1/billing/portal are active. When nil (default when main.go is
	// unmodified), both endpoints return 503 so the rest of the gateway is unaffected.
	Checkout BillingService
	// TracerProvider is optional. When nil, a noop tracer is used (default-off).
	TracerProvider oteltrace.TracerProvider
	// ErrorRecorder is optional. When set, non-2xx responses on /v1 worker routes
	// are recorded asynchronously into error_events for the customer error-history view.
	// Billing routes (/v1/billing/*) are registered in a separate subrouter and are
	// intentionally excluded — they are framework infrastructure, not worker calls.
	// When nil (default when main.go is unmodified), events are silently dropped.
	ErrorRecorder *errorlog.ErrorRecorder
	// OperatorStore and OperatorToken together gate the /v1/admin/* read-only subrouter.
	// Both must be non-nil / non-empty for the admin routes to be registered.
	// The operator path is completely separate from the customer auth.Middleware path.
	OperatorStore *operator.Store
	OperatorToken string
}

// jobsAdminAdapter bridges jobs.Store's admin methods to
// operator.JobsAdminStore, translating jobs.AdminJob into operator.AdminJob.
// Lives here — not in package operator or package jobs — because package
// jobs already imports webhookout, and webhookout's own test suite
// (adminhttp_test.go) imports package operator for its cross-endpoint
// coverage; operator importing jobs directly would create an import cycle
// in webhookout's test binary. Package server already imports both without
// issue, since nothing imports package server.
type jobsAdminAdapter struct{ store *jobs.Store }

func (a jobsAdminAdapter) AdminList(ctx context.Context, status *string, limit, offset int) ([]operator.AdminJob, int64, error) {
	rows, total, err := a.store.AdminList(ctx, status, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	out := make([]operator.AdminJob, len(rows))
	for i, j := range rows {
		out[i] = adminJobFromStoreRow(j)
	}
	return out, total, nil
}

func (a jobsAdminAdapter) AdminGet(ctx context.Context, id uuid.UUID) (operator.AdminJob, bool, error) {
	j, ok, err := a.store.AdminGet(ctx, id)
	if err != nil || !ok {
		return operator.AdminJob{}, ok, err
	}
	return adminJobFromStoreRow(j), true, nil
}

func (a jobsAdminAdapter) Requeue(ctx context.Context, id uuid.UUID) error {
	return a.store.Requeue(ctx, id)
}

func (a jobsAdminAdapter) ReleaseClaimed(ctx context.Context, instanceID uuid.UUID) (int64, error) {
	return a.store.ReleaseClaimed(ctx, instanceID)
}

func adminJobFromStoreRow(j jobs.AdminJob) operator.AdminJob {
	return operator.AdminJob{
		ID:            j.ID,
		CustomerID:    j.CustomerID,
		Operation:     j.Operation,
		Status:        j.Status,
		Result:        j.Result,
		UnitsLabel:    j.UnitsLabel,
		BillableUnits: j.BillableUnits,
		ErrorCode:     j.ErrorCode,
		ErrorMessage:  j.ErrorMessage,
		ClaimedBy:     j.ClaimedBy,
		ClaimedAt:     j.ClaimedAt,
		CreatedAt:     j.CreatedAt,
		UpdatedAt:     j.UpdatedAt,
	}
}

// NewRouter builds the gateway router: public health + stripe webhook, plus auth+ratelimit-gated /v1 routes.
//
// The outbound webhook emitter is normally supplied via d.Emitter (main.go
// constructs the single shared instance and also injects it into
// jobs.NewExecutor). When d.Emitter is nil, NewRouter falls back to
// constructing one from d.DB itself (nil-safe: no worker is started and no
// routes are registered when d.DB is nil) — this keeps callers that only set
// DB (test harnesses) working unmodified.
func NewRouter(d *Deps) http.Handler {
	emitter := d.Emitter
	if emitter == nil {
		// context.Background() keeps the delivery worker alive for the process
		// lifetime; process exit (SIGTERM/SIGKILL) stops the goroutine. The
		// worker's per-delivery timeout (10 s) bounds how long an individual
		// POST can hold a DB connection.
		emitter = webhookout.NewEmitter(context.Background(), d.DB)
	}
	// SetFailureThreshold is the wiring point for WEBHOOK_ENDPOINT_FAILURE_THRESHOLD
	// regardless of whether emitter came from d.Emitter (main.go's construction,
	// which predates this knob and has no option for it) or was just constructed
	// above — safe to call unconditionally since it's a nil-safe, race-free setter
	// (see Emitter.SetFailureThreshold's doc comment).
	emitter.SetFailureThreshold(d.Cfg.WebhookEndpointFailureThreshold)
	// jobStore is nil-safe (jobs.NewStore returns nil when d.DB is nil), matching
	// the framework's optional-Deps pattern — every exported Store method
	// nil-checks its receiver. Used both by the async-opted-in branch of the
	// per-product loop below and by GET /v1/jobs/{id}.
	jobStore := jobs.NewStore(d.DB)
	// Wire the emitter into the framework components whose Emit call-sites live
	// outside this package. Both are nil-safe if unset (e.g. in tests that build
	// a partial Deps and never call NewRouter's webhook-serving paths).
	if d.Webhook != nil {
		d.Webhook.SetEmitter(emitter)
	}
	if d.Auth != nil {
		d.Auth.SetEmitter(emitter)
	}
	// Snapshot V1Routes with Async flags applied via AnnotatedRoutes so both
	// openapi.Handler and the registration loop see the same stable slice.
	// AnnotatedRoutes is also what spec-dump uses — a single source of truth
	// ensures /openapi.json and clients/openapi.json can never diverge.
	routes := AnnotatedRoutes()

	r := chi.NewRouter()

	r.Use(mw.RequestID)
	r.Use(tracing.Middleware(d.TracerProvider)) // after RequestID, before AccessLog
	r.Use(mw.AccessLog)
	r.Use(mw.Recovery)
	r.Use(observability.Middleware)
	r.Use(mw.SecurityHeaders)
	r.Use(mw.BodyLimit(d.Cfg.BodyLimitBytes))
	// AllowedMethods includes DELETE and PATCH for the browser-facing preflight
	// on DELETE and PATCH /v1/webhooks/endpoints/{id} (go-chi/cors withholds
	// Access-Control-Allow-Origin/Methods on a preflight whose
	// Access-Control-Request-Method isn't in this list).
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{d.Cfg.DashboardOrigin},
		AllowedMethods:   []string{"GET", "POST", "DELETE", "PATCH", "OPTIONS"},
		AllowedHeaders:   []string{"Authorization", "Content-Type", "X-Request-ID", "Idempotency-Key"},
		ExposedHeaders:   []string{"X-Idempotent-Replayed", "Retry-After", "RateLimit-Limit", "RateLimit-Remaining", "RateLimit-Reset", "X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset", "X-Quota-Limit", "X-Quota-Remaining", "X-Quota-Reset"},
		AllowCredentials: false,
		MaxAge:           300,
	}))

	// Public routes (no auth, no rate limit).
	r.Get("/healthz", healthz)
	r.Get("/readyz", readyz(d.Redis, d.PG))
	r.Get("/openapi.json", openapi.Handler(routes))

	// The Stripe webhook is mounted outside auth/quota gating, so it carries no
	// per-customer rate limit. Add a lightweight IP-based limiter (60 req/min/IP,
	// keyed on X-Forwarded-For/RemoteAddr) in front of ONLY this route to blunt
	// unauthenticated flooding. Signature verification, the replay window, and the
	// dispatch-first/record-after ordering inside Handle are untouched.
	//
	// Defense-in-depth: strip True-Client-IP and X-Real-IP before the rate limiter
	// runs. httprate.KeyByRealIP prefers those headers over X-Forwarded-For; if they
	// reached the limiter an attacker could rotate them to mint a fresh bucket per
	// request and defeat the cap (audit #11). Caddy already strips them at the edge,
	// but doing it here too means a Caddyfile mis-config cannot silently re-open the
	// bypass.
	stripSpoofableIPHeaders := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Header.Del("True-Client-IP")
			r.Header.Del("X-Real-IP")
			next.ServeHTTP(w, r)
		})
	}
	r.With(stripSpoofableIPHeaders, httprate.LimitByRealIP(60, time.Minute)).Post("/webhooks/stripe", d.Webhook.Handle)

	// === Framework billing routes (auth gated, no quota/rate-limit) ===
	// Self-serve Stripe Checkout and Billing Portal redirect endpoints.
	// Separated from the per-product block so per-product clones never need to
	// edit this block — it is framework infrastructure every clone inherits.
	//
	// Idempotency middleware is intentionally omitted: Stripe Checkout sessions
	// are one-shot redirect URLs (not repeatable POST operations), and the
	// portal creates a session scoped to the authenticated customer — retries
	// are harmless because Stripe's own session dedup prevents double-billing.
	r.Route("/v1/billing", func(r chi.Router) {
		r.Use(auth.Middleware(d.Auth))
		r.Post("/checkout", billingCheckoutHandler(d))
		r.Post("/portal", billingPortalHandler(d))
	})

	// === Outbound webhook delivery log + endpoint management (auth gated; active when DB is set) ===
	// The emitter is wired above (into d.Webhook, d.Auth, and quota.Middleware);
	// these routes use the DB directly so they're decoupled from the worker
	// goroutine lifecycle. The endpoint CRUD routes are the write-counterpart to
	// the read-only delivery log: registering/listing/deleting webhook_endpoints
	// rows was previously reachable only through the NextAuth dashboard.
	if d.DB != nil {
		r.Route("/v1/webhooks", func(r chi.Router) {
			r.Use(auth.Middleware(d.Auth))
			r.Get("/deliveries", webhookDeliveriesHandler(d.DB))
			r.Post("/endpoints", webhookout.CreateEndpointHandler(d.DB, authCustomerID))
			r.Get("/endpoints", webhookout.ListEndpointsHandler(d.DB, authCustomerID))
			r.Delete("/endpoints/{id}", webhookout.DeleteEndpointHandler(d.DB, authCustomerID))
			r.Patch("/endpoints/{id}", webhookout.UpdateEndpointSubscriptionHandler(d.DB, authCustomerID))
			r.Post("/endpoints/{id}/rotate-secret", webhookout.RotateEndpointSecretHandler(d.DB, authCustomerID))
			r.Post("/endpoints/{id}/enable", webhookout.EnableEndpointHandler(d.DB, authCustomerID))
		})

		// === Customer API-key self-management (auth gated; active when DB is set) ===
		// GET/rotate/DELETE parity with the dashboard's key management UI
		// (dashboard/app/api/keys) for API-key-only (headless) customers.
		// Registered here, not in per-product V1Routes, for the same reason as
		// /v1/webhooks above: framework infra every clone inherits.
		r.Route("/v1/keys", func(r chi.Router) {
			r.Use(auth.Middleware(d.Auth))
			r.Get("/", auth.ListKeysHandler(d.Auth))
			r.Post("/{id}/rotate", auth.RotateKeysHandler(d.Auth, d.Cfg.APIKeyPrefix))
			r.Delete("/{id}", auth.RevokeKeysHandler(d.Auth))
		})

		// === Customer error-history self-service route (auth gated; active
		// when DB is set; read-only) ===
		// GET /v1/errors: the caller's own error_events rows recorded by
		// v1ErrorCapture below on every non-2xx /v1 response — newest-first,
		// date/operation/code-filtered, paginated. The API-key-authenticated
		// counterpart to the dashboard's error-history view
		// (dashboard/app/api/errors). Framework infra, not a per-product
		// invoke route: no customer_id parameter — the customer comes
		// strictly from auth.FromContext, so this can only ever return the
		// caller's own error history.
		//
		// Registered as a direct, method-specific r.With(...).Get rather than
		// r.Route(...), for the same reason as GET /v1/usage below: r.Route
		// mounts a subrouter via chi's Mount, which claims ALL HTTP methods at
		// that exact path. A method-specific registration lets the static
		// "/v1/errors" node match ahead of the "/v1/*" wildcard the
		// per-product block below is mounted under, without capturing
		// POST/other methods at this path.
		r.With(auth.Middleware(d.Auth)).Get("/v1/errors", selferrors.Handler(d.DB))

		// === Async job status polling (auth gated; active when DB is set; read-only) ===
		// GET /v1/jobs/{id}: the result of a job enqueued by a POST /v1/<op>
		// route opted into AsyncRoutes (routes_table.go). Framework infra, not
		// a per-product invoke route — kept out of the r.Route("/v1", ...)
		// per-product block below (same reason as GET /v1/errors above: that
		// block's POST-only invariant is asserted by TestV1RoutesDriftGuard),
		// registered here as a direct, method-specific r.With(...).Get so it
		// coexists with any per-product POST at another /v1/... path. Scoped
		// strictly to the caller's own jobs — see jobs.Store.Get's SQL-level
		// customer_id scoping.
		r.With(auth.Middleware(d.Auth)).Get("/v1/jobs/{id}", jobsGetHandler(jobStore))

		// === Async job cancellation (auth gated; active when DB is set; customer-owned mutation) ===
		// POST /v1/jobs/{id}/cancel: withdraws the caller's own still-queued
		// job, mirroring GET /v1/jobs/{id}'s registration rationale (framework
		// infra, kept out of the per-product POST-only r.Route("/v1", ...)
		// block below — that block's invariant is asserted by
		// TestV1RoutesDriftGuard). Scoped strictly to the caller's own jobs —
		// see jobs.Store.CancelQueued's SQL-level customer_id scoping. This is
		// the one customer-facing mutation among the framework's /v1/jobs
		// routes; unlike the admin requeue/release primitives (gated behind
		// OPERATOR_TOKEN below), a customer may only ever cancel their own
		// queued submission, never touch another customer's or a running one.
		r.With(auth.Middleware(d.Auth)).Post("/v1/jobs/{id}/cancel", jobsCancelHandler(jobStore))

		// === Async job history listing (auth gated; active when DB is set; read-only) ===
		// GET /v1/jobs: a paginated history of the caller's own enqueued jobs,
		// the enumerate-many counterpart to GET /v1/jobs/{id} above. Same
		// registration rationale (framework infra, direct method-specific
		// r.With(...).Get rather than r.Route, kept out of the per-product
		// block). Scoped strictly to the caller's own jobs — see
		// jobs.Store.List's SQL-level customer_id scoping.
		r.With(auth.Middleware(d.Auth)).Get("/v1/jobs", jobsListHandler(jobStore))

		// === Per-event usage export (auth gated; active when DB is set; read-only) ===
		// GET /v1/usage/events: the caller's own usage_events rows (id, operation,
		// billable_units, created_at), newest-first, date/operation-filtered,
		// paginated JSON or RFC-4180 CSV (?format=csv / Accept: text/csv). The
		// API-key-authenticated counterpart to the dashboard's usage export
		// (dashboard/app/api/usage), for programmatic customers reconciling
		// against Stripe invoices. Framework infra, not a per-product invoke
		// route: no customer_id parameter — the customer comes strictly from
		// auth.FromContext, so this can only ever return the caller's own usage.
		// Kept out of the per-product r.Route("/v1", ...) POST loop below for
		// the same reason as GET /v1/errors and GET /v1/jobs above (that block's
		// POST-only invariant is asserted by TestV1RoutesDriftGuard).
		r.With(auth.Middleware(d.Auth)).Get("/v1/usage/events", selfusagedetail.Handler(d.DB))
	}

	// === Framework customer usage self-service route (auth gated; read-only) ===
	// GET /v1/usage: the caller's own current-period consumption, quota cap,
	// remaining balance, and per-operation breakdown — derived from the same
	// quota.Tracker/billing.PlanCache signals the quota middleware enforces
	// against. Framework infra, not a product /invoke route: no metering, no
	// quota/rate-limit/idempotency gating, and no customer_id parameter — the
	// customer comes strictly from auth.FromContext, so this can only ever
	// return the caller's own usage.
	//
	// Registered as a direct, method-specific r.Get rather than r.Route(...):
	// r.Route mounts a subrouter via chi's Mount, which claims ALL HTTP methods
	// at that exact path (verified empirically — chi's radix tree matches the
	// static "/v1/usage" node before ever considering the "/v1/*" wildcard the
	// per-product block below is mounted under). If a product clone ever names
	// an invoke route "/usage", a Route-based mount here would 405 every
	// POST /v1/usage before it reached the product handler, even though both
	// "coexist" from chi's registration API — they don't coexist in the actual
	// tree walk. r.With(...).Get registers only the GET method at this node, so
	// a same-path product POST mounted afterward is unaffected.
	//
	// Gated on d.Auth rather than d.DB: unlike webhookDeliveriesHandler (which
	// dereferences its *pgxpool.Pool directly and would panic if nil),
	// selfusage.Handler is nil-DB/nil-Quota/nil-Plans-safe by design — an unset
	// dependency degrades its slice of the response instead of erroring. d.Auth
	// is the one dependency every real deployment always sets (cmd/gateway/main.go
	// constructs it unconditionally), so gating on it means this route is live in
	// every real gateway today — not just once Deps.DB is wired — while still
	// leaving it unregistered for the synthetic no-Auth Deps NewRouter's own
	// tests build.
	if d.Auth != nil {
		r.With(auth.Middleware(d.Auth)).Get("/v1/usage", selfusage.Handler(d.DB, d.Quota, d.Plans))
	}

	// === Framework operator/admin read-only routes (operator-token gated) ===
	// Separate from the customer API-key path. No mutation of customer/plan/key/billing
	// state is permitted here — see gateway/internal/operator/store.go.
	// Routes are only registered when both OperatorStore and OperatorToken are set,
	// so a missing OPERATOR_TOKEN env simply leaves /v1/admin/* unregistered.
	//
	// The webhook dead-letter replay routes are the one mutation admitted into this
	// otherwise read-only subrouter: they requeue rows for the existing Emitter worker
	// to redeliver (see webhookout/replay.go) rather than opening a new send path. They
	// additionally require d.DB (mirroring the d.DB-gated /v1/webhooks block above)
	// since, unlike operator.Store, they talk to Postgres directly.
	if d.OperatorStore != nil && d.OperatorToken != "" {
		r.Route("/v1/admin", func(r chi.Router) {
			r.Use(operator.Middleware(d.OperatorToken))
			r.Get("/customers", operator.ListCustomersHandler(d.OperatorStore))
			r.Get("/customers/{id}", operator.GetCustomerHandler(d.OperatorStore))
			r.Get("/customers/{id}/usage", operator.GetCustomerUsageHandler(d.OperatorStore))
			r.Get("/audit", operator.ListAuditEventsHandler(d.OperatorStore))
			r.Get("/plans", operator.ListPlansHandler(d.OperatorStore))
			if d.DB != nil {
				r.Get("/webhooks/deadletters", webhookout.ListDeadLettersHandler(d.DB))
				r.Post("/webhooks/deadletters/{id}/replay", webhookout.ReplaySingleHandler(d.DB))
				r.Post("/webhooks/deadletters/replay", webhookout.ReplayBulkHandler(d.DB))
				// Cross-customer job recovery: exposes the jobs.Store.Requeue /
				// ReleaseClaimed primitives (reserved for "future operator tooling"
				// since the async subsystem shipped) behind the same
				// OPERATOR_TOKEN-gated pattern as the dead-letter replay routes
				// above. jobStore is nil-safe but is only ever nil when d.DB is
				// nil, so this is gated on d.DB for symmetry with those routes.
				// Wrapped in jobsAdminAdapter (below) rather than passed
				// directly: package operator can't import package jobs without
				// creating an import cycle in webhookout's test binary (jobs
				// imports webhookout; webhookout's own test suite imports
				// operator) — see operator.JobsAdminStore's doc comment.
				jobsAdmin := jobsAdminAdapter{store: jobStore}
				r.Get("/jobs", operator.AdminListJobsHandler(jobsAdmin))
				r.Get("/jobs/{id}", operator.AdminGetJobHandler(jobsAdmin))
				r.Post("/jobs/{id}/requeue", operator.AdminRequeueJobHandler(jobsAdmin, d.DB))
				r.Post("/jobs/release", operator.AdminReleaseJobsHandler(jobsAdmin, d.DB))
			}
		})
	}

	// === Per-product routes (auth + rate-limit gated) ===
	// Each line maps an HTTP path to an opaque worker operation. Add a line per new endpoint.
	//
	// Middleware order: chi executes earlier-registered middleware outermost.
	//   v1ErrorCapture — wraps response writer to capture /v1 errors for logging.
	//   ratelimit      — counts every request including replays and later-rejected bodies.
	//   idempotency    — replays exit here; before validation and quota.
	//   validate       — rejects malformed bodies before quota is reserved.
	//   quota          — only reached by genuine, well-formed, non-replay requests.
	//   respcache      — mounted per-route, immediately in front of invoke; a hit
	//                    still reserves quota (already done above) and still
	//                    meters, it only skips the worker HTTP round-trip.

	// Pre-compile every regex pattern in RequestSchemas to catch RE2-incompatible
	// patterns (e.g. ECMAScript lookaheads) at startup. An invalid pattern is a
	// schema authoring bug in the clone; serving the route would reject every
	// request with a pattern-leaking 400. Fail fast instead.
	if err := validate.CompileSchemaPatterns(routes); err != nil {
		log.Fatal().Err(err).Str("hint", "check RequestSchema.Pattern in V1Routes").Msg("invalid regex pattern in RequestSchema")
	}

	// Cross-check every declared SampleRequest against its own RequestSchema at
	// startup. A drifted sample would otherwise ship silently: as a misleading
	// example in /openapi.json and as a payload that makes
	// gateway/test/acceptance pass without actually exercising the route's
	// validation. Fail fast instead, mirroring CompileSchemaPatterns above.
	if err := validate.ValidateSampleRequests(routes); err != nil {
		log.Fatal().Err(err).Str("hint", "check SampleRequest in V1Routes").Msg("route SampleRequest fails validation against its RequestSchema")
	}

	idempStore := idempotency.NewStore(d.DB) // nil-safe: pass-through when d.DB is nil
	// Snapshot config values used inside the /v1 subrouter before entering the closure.
	capturePayloadEnabled := d.Cfg.ErrorPayloadCapture
	capturePayloadMaxBytes := d.Cfg.ErrorPayloadMaxBytes
	errorExposure := d.Cfg.ErrorExposure
	r.Route("/v1", func(r chi.Router) {
		r.Use(auth.Middleware(d.Auth))
		// Capture errors after auth (so customer context is populated) but before
		// ratelimit/quota so their 429s are recorded with the correct status.
		r.Use(v1ErrorCapture(d.ErrorRecorder, capturePayloadEnabled, capturePayloadMaxBytes))
		r.Use(ratelimit.Middleware(d.Bucket, d.Plans))
		r.Use(idempotency.Middleware(idempStore))
		r.Use(validate.Middleware(routes))
		r.Use(quota.Middleware(d.Quota, d.Plans, emitter))
		for _, rt := range routes {
			// A route present in AsyncRoutes (routes_table.go) enqueues a durable
			// job and returns 202 {job_id} instead of invoking the worker inline;
			// one table drives both branches. Registered inside this same loop
			// (not a separate subrouter) so it inherits the identical
			// auth/error-capture/ratelimit/idempotency/validate/quota middleware
			// chain as every synchronous /v1 route — including the quota.Reserve
			// admission check on every request.
			if timeoutSeconds, async := AsyncRoutes[rt.Path]; async {
				r.Post(rt.Path, enqueueAsync(jobStore, rt.Operation, timeoutSeconds, d.Cfg.JobMaxQueuedPerCustomer))
				continue
			}
			ttl := d.Cfg.ClampRespCacheTTL(RespCacheTTLSeconds[rt.Path])
			cacheMW := respcache.Middleware(d.RespCache, d.Recorder, rt.Operation, ttl, nil)
			r.With(cacheMW).Post(rt.Path, invoke(d.Proxy, d.Recorder, errorExposure, rt.Operation))
		}
	})

	return r
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func readyz(redis, pg HealthChecker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		redisStatus := "ok"
		if err := redis.Ping(ctx); err != nil {
			redisStatus = "error"
		}

		pgStatus := "ok"
		if err := pg.Ping(ctx); err != nil {
			pgStatus = "error"
		}

		overall := "ok"
		if redisStatus != "ok" || pgStatus != "ok" {
			overall = "degraded"
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(readyzResponse{
			Status: overall,
			Checks: map[string]string{
				"redis":    redisStatus,
				"postgres": pgStatus,
			},
		})
	}
}

func invoke(p *proxy.Client, recorder *usage.Recorder, errorExposure string, operation string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mw.RequestIDKey).(string)
		key := auth.FromContext(r.Context())

		var payload json.RawMessage
		// r.Body is already wrapped by http.MaxBytesReader via mw.BodyLimit at the router level;
		// reads beyond cfg.BodyLimitBytes return an error the decoder propagates here as BAD_REQUEST.
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "invalid json body", false) // false: malformed JSON is permanent; retrying with the same body will not succeed
			return
		}

		req := &proxy.InvokeRequest{RequestID: rid, Operation: operation, Payload: payload}
		// key is nil in tests that bypass auth.Middleware; skip customer fields safely.
		if key != nil {
			req.CustomerID = key.Customer.ID.String()
			req.Plan = key.Customer.Plan
		}

		resp, err := p.Invoke(r.Context(), req)
		if err != nil {
			log.Error().
				Err(err).
				Str("request_id", rid).
				Str("operation", operation).
				Msg("worker invocation failed")
			observability.WorkerErrorsTotal.WithLabelValues(apierror.WORKER_UNREACHABLE).Inc()
			apierror.Write(w, rid, http.StatusBadGateway, apierror.WORKER_UNREACHABLE, "worker unavailable", true)
			return
		}

		// Worker structured errors: expose or sanitize based on config.
		if resp.Error != nil {
			// Guard the metric label against an empty Code from a buggy or non-SDK worker:
			// an empty Code would open an unlabelled code="" series.
			metricCode := resp.Error.Code
			if metricCode == "" {
				metricCode = apierror.UNKNOWN
			}
			observability.WorkerErrorsTotal.WithLabelValues(metricCode).Inc()
			// Sanitized mode always returns WORKER_UNREACHABLE — the customer-facing
			// contract must not change regardless of what the worker sends.
			// Full mode passes the worker's error verbatim; guard empty Code so
			// customers never receive "code":"" (an empty code is not correlatable).
			// jobs.SanitizeWorkerError is the single definition of this policy — the
			// async Executor applies it identically before persisting a failed job's
			// error_code/error_message, so GET /v1/jobs/{id} can't leak what this
			// path hides.
			errCode, errMsg := jobs.SanitizeWorkerError(errorExposure, resp.Error.Code, resp.Error.Message)
			errRetryable := true
			if errorExposure == "full" {
				errRetryable = resp.Error.Retryable
			}
			apierror.Write(w, rid, http.StatusBadGateway, errCode, errMsg, errRetryable)
			return
		}

		// Contract check: a successful worker response MUST report billable_units >= 1.
		// Otherwise a buggy or malicious non-SDK worker could let customers consume service for free.
		// The SDK enforces this client-side, but the gateway is the trust boundary.
		if resp.BillableUnits < 1 {
			log.Warn().
				Str("request_id", rid).
				Str("operation", operation).
				Msg("worker returned success with billable_units<1 — rejecting")
			observability.WorkerErrorsTotal.WithLabelValues(apierror.WORKER_BAD_RESPONSE).Inc()
			apierror.Write(w, rid, http.StatusBadGateway, apierror.WORKER_BAD_RESPONSE, "worker contract violation", false)
			return
		}

		// Record usage on successful (non-error) responses. Best-effort; do not fail the customer on write error.
		// key is nil in tests that bypass auth.Middleware; skip customer fields safely.
		if key != nil {
			if err := recorder.Record(r.Context(), key.Customer.ID, key.ID, operation, rid, resp.BillableUnits); err != nil {
				log.Warn().Err(err).Str("request_id", rid).Msg("usage record failed")
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if resp.BillableUnits > 0 {
			w.Header().Set("X-Billable-Units", strconv.FormatUint(resp.BillableUnits, 10))
		}
		if resp.UnitsLabel != "" {
			w.Header().Set("X-Units-Label", resp.UnitsLabel)
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// asyncJobResponse is the JSON shape GET /v1/jobs/{id} returns.
type asyncJobResponse struct {
	JobID         string          `json:"job_id"`
	Status        string          `json:"status"`
	Result        json.RawMessage `json:"result,omitempty"`
	Error         *asyncJobError  `json:"error,omitempty"`
	BillableUnits uint64          `json:"billable_units,omitempty"`
	UnitsLabel    string          `json:"units_label,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

type asyncJobError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// jobBacklogExceededCode is enqueueAsync's stable error code for a
// JOB_MAX_QUEUED_PER_CUSTOMER rejection. Declared here rather than added to
// apierror's shared constant block (out of this feature's file scope) —
// apierror.Write accepts any string code, so a package-local constant gives
// the same stability/discoverability for this one route without touching
// that file.
const jobBacklogExceededCode = "JOB_BACKLOG_EXCEEDED"

// enqueueAsync handles POST /v1/<op> for a route opted into AsyncRoutes: it
// decodes the request body exactly like invoke does, then persists a queued
// job row instead of calling the worker inline, and returns 202 {job_id}.
// The worker invocation, billable_units contract check, and usage.Recorder
// call happen later, out of band, in jobs.Executor.process — the caller
// polls GET /v1/jobs/{id} for the eventual result.
//
// maxQueuedPerCustomer <= 0 (the config.Config.JobMaxQueuedPerCustomer
// zero-value default) admits unconditionally, exactly as before this knob
// existed. > 0 ceilings the caller's queued+running backlog
// (jobs.Store.CountActive); once reached, enqueue is rejected with 429
// jobBacklogExceededCode rather than growing an unbounded backlog for one
// customer at every other tenant's expense.
func enqueueAsync(store *jobs.Store, operation string, timeoutSeconds, maxQueuedPerCustomer int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mw.RequestIDKey).(string)
		if store == nil {
			apierror.Write(w, rid, http.StatusServiceUnavailable, apierror.NOT_CONFIGURED, "async jobs not configured", false)
			return
		}
		key := auth.FromContext(r.Context())
		if key == nil {
			apierror.Write(w, rid, http.StatusUnauthorized, apierror.UNAUTHORIZED, "no auth context", false)
			return
		}

		if maxQueuedPerCustomer > 0 {
			active, err := store.CountActive(r.Context(), key.Customer.ID)
			if err != nil {
				log.Error().Err(err).Str("request_id", rid).Str("operation", operation).Msg("jobs: backlog check failed")
				apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "enqueue failed", false)
				return
			}
			if active >= int64(maxQueuedPerCustomer) {
				observability.JobsCustomerThrottledTotal.WithLabelValues("backlog_ceiling").Inc()
				apierror.Write(w, rid, http.StatusTooManyRequests, jobBacklogExceededCode, "async job backlog limit exceeded", true)
				return
			}
		}

		var payload json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "invalid json body", false)
			return
		}

		// Passing the Idempotency-Key header through to Store.Enqueue (not
		// consuming/stripping it — idempotency.Middleware still owns the
		// response-replay behavior above this handler) closes a narrower
		// race: if that middleware's own finalize step fails after this
		// insert already committed, it releases the key so a client retry
		// reaches this handler again; Enqueue returns the existing job's id
		// instead of inserting a duplicate for the retry.
		idempotencyKey := r.Header.Get("Idempotency-Key")
		id, err := store.Enqueue(r.Context(), key.Customer.ID, key.ID, operation, rid, key.Customer.Plan, payload, timeoutSeconds, idempotencyKey)
		if err != nil {
			log.Error().Err(err).Str("request_id", rid).Str("operation", operation).Msg("jobs: enqueue failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "enqueue failed", false)
			return
		}
		observability.JobsEnqueuedTotal.WithLabelValues(operation).Inc()
		// quota.Middleware already reserved +1 up front and will refund it the
		// instant this handler returns unless the signal is flipped — a plain
		// enqueue-and-return-202 would let a capped-plan customer enqueue an
		// unbounded number of jobs (each reserve is refunded before the next
		// request's Reserve ever sees it), bypassing the admission check this
		// route is supposed to inherit. Marking recorded makes the +1 reserve
		// stick per enqueued job — mirroring the sync path's own "1 (reserve) +
		// units (recorder)" per-call overhead (see quota.Tracker.Reserve's doc
		// comment) — and the real billable_units still land on top of it when
		// the job completes (usage.Recorder.Record, called from the executor).
		// A job that later fails still consumes this 1 unit; that's a
		// deliberate fail-closed tradeoff over silently exempting async
		// enqueues from the cap entirely.
		quota.MarkRecorded(r.Context())

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"job_id": id.String()})
	}
}

// jobsGetHandler handles GET /v1/jobs/{id}: the caller's own job status and
// result, scoped at the SQL level by jobs.Store.Get (IDOR-safe — a job id
// owned by another customer returns 404, indistinguishable from a
// nonexistent id).
func jobsGetHandler(store *jobs.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mw.RequestIDKey).(string)
		if store == nil {
			apierror.Write(w, rid, http.StatusServiceUnavailable, apierror.NOT_CONFIGURED, "async jobs not configured", false)
			return
		}
		key := auth.FromContext(r.Context())
		if key == nil {
			apierror.Write(w, rid, http.StatusUnauthorized, apierror.UNAUTHORIZED, "no auth context", false)
			return
		}

		id, err := uuid.Parse(chi.URLParam(r, "id"))
		if err != nil {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "invalid job id", false)
			return
		}

		job, ok, err := store.Get(r.Context(), id, key.Customer.ID)
		if err != nil {
			log.Error().Err(err).Str("request_id", rid).Msg("jobs: get failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "lookup failed", false)
			return
		}
		if !ok {
			apierror.Write(w, rid, http.StatusNotFound, "NOT_FOUND", "job not found", false)
			return
		}

		resp := asyncJobResponse{
			JobID:      job.ID.String(),
			Status:     job.Status,
			Result:     job.Result,
			UnitsLabel: job.UnitsLabel,
			CreatedAt:  job.CreatedAt,
			UpdatedAt:  job.UpdatedAt,
		}
		if job.Status == jobs.StatusSucceeded {
			resp.BillableUnits = job.BillableUnits
		}
		if job.Status == jobs.StatusFailed {
			resp.Error = &asyncJobError{Code: job.ErrorCode, Message: job.ErrorMessage}
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// jobsCancelHandler handles POST /v1/jobs/{id}/cancel: withdraws the
// caller's own job while it is still queued, via jobs.Store.CancelQueued's
// single atomic UPDATE ... WHERE id=$1 AND customer_id=$2 AND status='queued'
// (IDOR-safe and status-guarded in one statement, unlike the admin requeue
// handler's separate lookup-then-update — see AdminRequeueJobHandler). When
// CancelQueued reports no row changed, a follow-up Get (itself IDOR-safe)
// distinguishes the two possible causes: the job doesn't exist or isn't
// owned by this customer (404, indistinguishable by design, matching
// jobsGetHandler), or it exists but is no longer queued (409
// JOB_NOT_CANCELLABLE, mirroring AdminRequeueJobHandler's
// JOB_NOT_REQUEUABLE guard for the same reason — a running job may already
// be mid-flight, and a succeeded/failed/cancelled job is already terminal).
// This cycle has no cooperative cancel of a RUNNING job: it always 409s.
func jobsCancelHandler(store *jobs.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mw.RequestIDKey).(string)
		if store == nil {
			apierror.Write(w, rid, http.StatusServiceUnavailable, apierror.NOT_CONFIGURED, "async jobs not configured", false)
			return
		}
		key := auth.FromContext(r.Context())
		if key == nil {
			apierror.Write(w, rid, http.StatusUnauthorized, apierror.UNAUTHORIZED, "no auth context", false)
			return
		}

		id, err := uuid.Parse(chi.URLParam(r, "id"))
		if err != nil {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "invalid job id", false)
			return
		}

		cancelled, err := store.CancelQueued(r.Context(), id, key.Customer.ID)
		if err != nil {
			log.Error().Err(err).Str("request_id", rid).Msg("jobs: cancel failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "cancel failed", false)
			return
		}

		if !cancelled {
			_, ok, err := store.Get(r.Context(), id, key.Customer.ID)
			if err != nil {
				log.Error().Err(err).Str("request_id", rid).Msg("jobs: cancel lookup failed")
				apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "lookup failed", false)
				return
			}
			if !ok {
				apierror.Write(w, rid, http.StatusNotFound, "NOT_FOUND", "job not found", false)
				return
			}
			apierror.Write(w, rid, http.StatusConflict, "JOB_NOT_CANCELLABLE", "job cannot be cancelled in its current state", false)
			return
		}

		job, ok, err := store.Get(r.Context(), id, key.Customer.ID)
		if err != nil || !ok {
			log.Error().Err(err).Str("request_id", rid).Msg("jobs: cancel re-fetch failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "cancel succeeded but re-fetch failed", false)
			return
		}

		resp := asyncJobResponse{
			JobID:      job.ID.String(),
			Status:     job.Status,
			Result:     job.Result,
			UnitsLabel: job.UnitsLabel,
			CreatedAt:  job.CreatedAt,
			UpdatedAt:  job.UpdatedAt,
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// validJobStatuses is the set of accepted ?status= filter values for
// jobsListHandler, mirrored from jobs.Status* so an unrecognized value can be
// rejected as 400 rather than silently matching zero rows.
var validJobStatuses = map[string]bool{
	jobs.StatusQueued:    true,
	jobs.StatusRunning:   true,
	jobs.StatusSucceeded: true,
	jobs.StatusFailed:    true,
	jobs.StatusCancelled: true,
}

const (
	jobsListDefaultPageSize = 20
	jobsListMaxPageSize     = 100
)

// jobListItem is one row of GET /v1/jobs's "items" list. Mirrors
// asyncJobResponse's status-conditional fields (billable_units only once
// succeeded, error only once failed) plus operation, so a caller can tell
// which POST /v1/<op> route enqueued each job without a second round trip;
// omits result/payload to keep the list response bounded regardless of how
// large an individual job's worker output is.
type jobListItem struct {
	JobID         string         `json:"job_id"`
	Operation     string         `json:"operation"`
	Status        string         `json:"status"`
	BillableUnits uint64         `json:"billable_units,omitempty"`
	UnitsLabel    string         `json:"units_label,omitempty"`
	Error         *asyncJobError `json:"error,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

// jobsListHandler handles GET /v1/jobs: a paginated history of the caller's
// own async jobs, scoped at the SQL level by jobs.Store.List (IDOR-safe,
// mirroring jobsGetHandler), optionally narrowed by ?status= (validated
// against jobs.Status*) and/or ?operation= (opaque, unvalidated — matches an
// exact enqueued operation string). Paginated via the shared paging package,
// same page/per_page contract as webhookDeliveriesHandler.
func jobsListHandler(store *jobs.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mw.RequestIDKey).(string)
		if store == nil {
			apierror.Write(w, rid, http.StatusServiceUnavailable, apierror.NOT_CONFIGURED, "async jobs not configured", false)
			return
		}
		key := auth.FromContext(r.Context())
		if key == nil {
			apierror.Write(w, rid, http.StatusUnauthorized, apierror.UNAUTHORIZED, "no auth context", false)
			return
		}

		q := r.URL.Query()
		var status *string
		if v := q.Get("status"); v != "" {
			if !validJobStatuses[v] {
				apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "unknown status filter", false)
				return
			}
			status = &v
		}
		var operation *string
		if v := q.Get("operation"); v != "" {
			operation = &v
		}

		pp := paging.ParseQuery(q, "per_page")
		page, perPage := paging.Clamp(pp.Page, pp.PerPage, jobsListDefaultPageSize, jobsListMaxPageSize)
		offset, err := paging.Offset(page, perPage)
		if err != nil {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "page too large", false)
			return
		}

		jobRows, total, err := store.List(r.Context(), key.Customer.ID, status, operation, perPage, offset)
		if err != nil {
			log.Error().Err(err).Str("request_id", rid).Msg("jobs: list failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "list failed", false)
			return
		}

		items := make([]jobListItem, 0, len(jobRows))
		for _, j := range jobRows {
			it := jobListItem{
				JobID:      j.ID.String(),
				Operation:  j.Operation,
				Status:     j.Status,
				UnitsLabel: j.UnitsLabel,
				CreatedAt:  j.CreatedAt,
				UpdatedAt:  j.UpdatedAt,
			}
			if j.Status == jobs.StatusSucceeded {
				it.BillableUnits = j.BillableUnits
			}
			if j.Status == jobs.StatusFailed {
				it.Error = &asyncJobError{Code: j.ErrorCode, Message: j.ErrorMessage}
			}
			items = append(items, it)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(paging.Page[jobListItem]{Items: items, Total: total})
	}
}

// v1ErrorCapture returns a middleware that wraps /v1 responses with an
// errorlog.Capture recorder and fires an async error_events insert on status >= 400.
// When capturePayload is true, the request body is buffered before dispatch and
// stored on the row. The payload never appears in logs or metric labels.
func v1ErrorCapture(rec *errorlog.ErrorRecorder, capturePayload bool, maxPayloadBytes int) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if rec == nil {
				next.ServeHTTP(w, r)
				return
			}
			// Capture route pattern before handler runs; chi populates it at
			// dispatch time and the value is stable for the lifetime of the request.
			op := chi.RouteContext(r.Context()).RoutePattern()
			if op == "" {
				op = r.URL.Path
			}
			// MaybeCaptureRequestBody is a no-op (nil, zero allocs) when capturePayload
			// is false. Runs before ratelimit so 429s are also recorded with payloads.
			var reqPayload []byte
			if capturePayload {
				reqPayload = errorlog.MaybeCaptureRequestBody(r, maxPayloadBytes)
			}
			capture := errorlog.NewCapture(w)
			next.ServeHTTP(capture, r)
			if capture.Status() < 400 {
				return
			}
			key := auth.FromContext(r.Context())
			if key == nil || key.Customer.ID == uuid.Nil {
				return
			}
			rid, _ := r.Context().Value(mw.RequestIDKey).(string)
			errCode, errMsg := capture.ParseErrorFields()
			rec.Record(context.Background(), key.Customer.ID, key.ID, op, errCode, rid, errMsg, capture.Status(), reqPayload)
		})
	}
}

// authCustomerID adapts auth.FromContext to webhookout.CustomerIDFunc. Defined
// here (rather than in webhookout) because webhookout cannot import auth — see
// webhookout.CustomerIDFunc's doc comment for why.
func authCustomerID(r *http.Request) (uuid.UUID, bool) {
	key := auth.FromContext(r.Context())
	if key == nil {
		return uuid.Nil, false
	}
	return key.Customer.ID, true
}

// webhookDeliveriesHandler returns a paginated list of the authenticated
// customer's outbound webhook deliveries across all their registered endpoints.
// GET /v1/webhooks/deliveries?page=1&per_page=20
func webhookDeliveriesHandler(db *pgxpool.Pool) http.HandlerFunc {
	type item struct {
		ID               string    `json:"id"`
		EventID          string    `json:"event_id"`
		EndpointURL      string    `json:"endpoint_url"`
		Status           string    `json:"status"`
		Attempts         int       `json:"attempts"`
		LastResponseCode *int      `json:"last_response_code,omitempty"`
		CreatedAt        time.Time `json:"created_at"`
	}
	const (
		// default matches the previous hardcoded LIMIT 100 to avoid silently
		// truncating callers that omit per_page; explicit per_page can go lower.
		defaultPageSize = 100
		maxPageSize     = 100
	)
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mw.RequestIDKey).(string)
		key := auth.FromContext(r.Context())
		if key == nil {
			apierror.Write(w, rid, http.StatusUnauthorized, apierror.UNAUTHORIZED, "no auth context", false)
			return
		}

		pp := paging.ParseQuery(r.URL.Query(), "per_page")
		page, perPage := paging.Clamp(pp.Page, pp.PerPage, defaultPageSize, maxPageSize)
		offset, err := paging.Offset(page, perPage)
		if err != nil {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "page too large", false)
			return
		}

		var total int64
		if err := db.QueryRow(r.Context(), `
			SELECT COUNT(*)
			FROM webhook_deliveries d
			JOIN webhook_endpoints we ON we.id = d.endpoint_id
			WHERE we.customer_id = $1
		`, key.Customer.ID).Scan(&total); err != nil {
			log.Error().Err(err).Str("request_id", rid).Msg("webhook deliveries count failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "query failed", false)
			return
		}

		rows, err := db.Query(r.Context(), `
			SELECT d.id::text, d.event_id, we.url, d.status, d.attempts,
			       d.last_response_code, d.created_at
			FROM webhook_deliveries d
			JOIN webhook_endpoints we ON we.id = d.endpoint_id
			WHERE we.customer_id = $1
			ORDER BY d.created_at DESC, d.id DESC
			LIMIT $2 OFFSET $3
		`, key.Customer.ID, perPage, offset)
		if err != nil {
			log.Error().Err(err).Str("request_id", rid).Msg("webhook deliveries query failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "query failed", false)
			return
		}
		defer rows.Close()
		items := []item{}
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.ID, &it.EventID, &it.EndpointURL, &it.Status,
				&it.Attempts, &it.LastResponseCode, &it.CreatedAt); err != nil {
				log.Error().Err(err).Str("request_id", rid).Msg("webhook deliveries scan failed")
				apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "scan failed", false)
				return
			}
			items = append(items, it)
		}
		if err := rows.Err(); err != nil {
			log.Error().Err(err).Str("request_id", rid).Msg("webhook deliveries rows error")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "rows error", false)
			return
		}

		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(paging.Page[item]{Items: items, Total: total})
	}
}

// billingCheckoutHandler creates a Stripe Checkout session and returns the redirect URL.
// POST /v1/billing/checkout body: {"plan_id":"pro"}
// Response: {"url":"https://checkout.stripe.com/..."}
func billingCheckoutHandler(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mw.RequestIDKey).(string)
		if d.Checkout == nil {
			apierror.Write(w, rid, http.StatusServiceUnavailable, apierror.NOT_CONFIGURED, "billing not configured", false)
			return
		}
		key := auth.FromContext(r.Context())
		if key == nil {
			apierror.Write(w, rid, http.StatusUnauthorized, apierror.UNAUTHORIZED, "no auth context", false)
			return
		}

		var body struct {
			PlanID string `json:"plan_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.PlanID == "" {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "plan_id required", false)
			return
		}
		if !planIDRE.MatchString(body.PlanID) {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "invalid plan_id", false)
			return
		}

		redirectURL, err := d.Checkout.CreateCheckoutSession(r.Context(), key.Customer.ID.String(), body.PlanID)
		if err != nil {
			if errors.Is(err, billing.ErrPlanNotFound) {
				apierror.Write(w, rid, http.StatusUnprocessableEntity, apierror.PLAN_NOT_FOUND, "plan not found or not upgradeable", false)
				return
			}
			log.Error().Err(err).Msg("create checkout session failed")
			apierror.Write(w, rid, http.StatusBadGateway, apierror.STRIPE_ERROR, "billing unavailable", false)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"url": redirectURL})
	}
}

// billingPortalHandler creates a Stripe Billing Portal session and returns the redirect URL.
// POST /v1/billing/portal
// Response: {"url":"https://billing.stripe.com/..."}
func billingPortalHandler(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mw.RequestIDKey).(string)
		if d.Checkout == nil {
			apierror.Write(w, rid, http.StatusServiceUnavailable, apierror.NOT_CONFIGURED, "billing not configured", false)
			return
		}
		key := auth.FromContext(r.Context())
		if key == nil {
			apierror.Write(w, rid, http.StatusUnauthorized, apierror.UNAUTHORIZED, "no auth context", false)
			return
		}

		stripeCustomerID, err := d.Checkout.LookupStripeCustomerID(r.Context(), key.Customer.ID.String())
		if err != nil {
			log.Error().Err(err).Msg("lookup stripe customer id failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "lookup failed", false)
			return
		}
		if stripeCustomerID == "" {
			apierror.Write(w, rid, http.StatusPaymentRequired, apierror.NO_STRIPE_CUSTOMER, "complete checkout first", false)
			return
		}

		redirectURL, err := d.Checkout.CreatePortalSession(r.Context(), stripeCustomerID)
		if err != nil {
			log.Error().Err(err).Msg("create portal session failed")
			apierror.Write(w, rid, http.StatusBadGateway, apierror.STRIPE_ERROR, "billing unavailable", false)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"url": redirectURL})
	}
}
