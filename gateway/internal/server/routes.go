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
	"github.com/Unluckyathecking/crucible/gateway/internal/license"
	mw "github.com/Unluckyathecking/crucible/gateway/internal/middleware"
	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
	"github.com/Unluckyathecking/crucible/gateway/internal/openapi"
	"github.com/Unluckyathecking/crucible/gateway/internal/operator"
	"github.com/Unluckyathecking/crucible/gateway/internal/paging"
	"github.com/Unluckyathecking/crucible/gateway/internal/proxy"
	"github.com/Unluckyathecking/crucible/gateway/internal/quota"
	"github.com/Unluckyathecking/crucible/gateway/internal/ratelimit"
	"github.com/Unluckyathecking/crucible/gateway/internal/respcache"
	"github.com/Unluckyathecking/crucible/gateway/internal/selfaudit"
	"github.com/Unluckyathecking/crucible/gateway/internal/selferrors"
	"github.com/Unluckyathecking/crucible/gateway/internal/selfusage"
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
	// License is the verified deployment license, or nil for community edition.
	// Wave-2 modules read it via License.Has(feature) to gate Pro/Business/
	// Enterprise endpoints; this package registers no license-gated routes itself.
	License *license.License
}

// NewRouter builds the gateway router: public health + stripe webhook, plus auth+ratelimit-gated /v1 routes.
//
// The outbound webhook emitter is constructed here from d.DB (nil-safe: no worker
// is started and no routes are registered when d.DB is nil). This keeps cmd/gateway/main.go
// byte-disjoint with PR #48 while still starting the worker as part of server setup.
func NewRouter(d *Deps) http.Handler {
	// Construct the outbound webhook emitter from the DB dep.
	// context.Background() keeps the delivery worker alive for the process lifetime;
	// process exit (SIGTERM/SIGKILL) stops the goroutine. The worker's per-delivery
	// timeout (10 s) bounds how long an individual POST can hold a DB connection.
	emitter := webhookout.NewEmitter(context.Background(), d.DB)
	// Wire the emitter into the framework components whose Emit call-sites live
	// outside this package. Both are nil-safe if unset (e.g. in tests that build
	// a partial Deps and never call NewRouter's webhook-serving paths).
	if d.Webhook != nil {
		d.Webhook.SetEmitter(emitter)
	}
	if d.Auth != nil {
		d.Auth.SetEmitter(emitter)
	}
	// Snapshot V1Routes once so both openapi.Handler and the registration loop
	// see the same stable slice for the lifetime of this router.
	routes := make([]openapi.RouteDescriptor, len(V1Routes))
	copy(routes, V1Routes)

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

		// === Customer audit-export self-service route (Enterprise Edition;
		// auth gated; active when DB is set; read-only) ===
		// GET /v1/audit: the caller's own audit_log rows — actions the customer
		// performed or actions performed against them — newest-first,
		// action-filterable, paginated. Same middleware chain and registration
		// idiom as GET /v1/errors above. The handler itself enforces the
		// license.FeatureAuditExport gate (403 FEATURE_NOT_LICENSED when
		// unlicensed) and derives the customer strictly from auth.FromContext, so
		// it can only ever return the caller's own history.
		r.With(auth.Middleware(d.Auth)).Get("/v1/audit", selfaudit.Handler(d.DB, d.License))
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
		// Operator-token authentication. The static OPERATOR_TOKEN is the
		// community path and always works. When the DB is wired, use the EE
		// TokenMiddleware, which additionally accepts DB-backed operator_tokens
		// — but only when the deployment license grants FeatureOperatorTokens
		// (nil license = community = DB never consulted, behaviour identical to
		// the static-only Middleware). Without a DB there is nowhere to store
		// tokens, so the static-only Middleware is used.
		adminAuth := operator.Middleware(d.OperatorToken)
		var tokenStore *operator.TokenStore
		if d.DB != nil {
			tokenStore = operator.NewTokenStore(d.DB)
			adminAuth = operator.TokenMiddleware(d.OperatorToken, tokenStore, d.Cfg.APIKeyHashSalt, d.License)
		}
		r.Route("/v1/admin", func(r chi.Router) {
			r.Use(adminAuth)
			r.Get("/customers", operator.ListCustomersHandler(d.OperatorStore))
			r.Get("/customers/{id}", operator.GetCustomerHandler(d.OperatorStore))
			r.Get("/customers/{id}/usage", operator.GetCustomerUsageHandler(d.OperatorStore))
			r.Get("/audit", operator.ListAuditEventsHandler(d.OperatorStore))
			r.Get("/plans", operator.ListPlansHandler(d.OperatorStore))
			if d.DB != nil {
				r.Get("/webhooks/deadletters", webhookout.ListDeadLettersHandler(d.DB))
				r.Post("/webhooks/deadletters/{id}/replay", webhookout.ReplaySingleHandler(d.DB))
				r.Post("/webhooks/deadletters/replay", webhookout.ReplayBulkHandler(d.DB))

				// === Operator multi-token management (Enterprise Edition) ===
				// DB-backed operator tokens with per-token names and revocation.
				// Each handler enforces the license.FeatureOperatorTokens gate
				// (403 FEATURE_NOT_LICENSED when unlicensed); the static
				// OPERATOR_TOKEN bearer bootstraps access to mint the first one.
				r.Post("/tokens", operator.CreateTokenHandler(tokenStore, d.DB, d.Cfg.APIKeyHashSalt, d.License))
				r.Get("/tokens", operator.ListTokensHandler(tokenStore, d.License))
				r.Delete("/tokens/{id}", operator.RevokeTokenHandler(tokenStore, d.DB, d.License))
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
			errCode, errMsg, errRetryable := apierror.WORKER_UNREACHABLE, "worker unavailable", true
			if errorExposure == "full" {
				errCode = resp.Error.Code
				if errCode == "" {
					// UNKNOWN is reserved for Prometheus metric labels; WORKER_BAD_RESPONSE
					// is the correct customer-facing fallback for a worker that omits the code.
					errCode = apierror.WORKER_BAD_RESPONSE
				}
				errMsg, errRetryable = resp.Error.Message, resp.Error.Retryable
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
