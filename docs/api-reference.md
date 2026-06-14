# Crucible API Reference

Metered API framework. Every endpoint requires an API key. Responses are JSON.

## Quick Start

**1. Sign up** at your product dashboard. Create an account with a magic-link email login.

**2. Generate an API key** from the dashboard. Keys look like `cru_live_A3F9NK4M7QHGBVTP`. You will see the full key only once. Store it securely.

**3. Make your first request:**

```bash
curl -X POST https://api.yourproduct.com/v1/echo \
  -H 'Authorization: Bearer cru_live_A3F9NK4M7QHGBVTP' \
  -H 'Content-Type: application/json' \
  -d '{"message": "hello"}'
```

Expected response:

```json
{
  "payload": {"echo": {"message": "hello"}, "operation": "echo"},
  "billable_units": 1
}
```

## Base URLs

| Environment | URL |
|---|---|
| Local development | `http://localhost:8080` |
| Production | `https://api.yourproduct.com` |

Replace `yourproduct.com` with your deployed domain. The gateway default port is `8080`.

## Authentication

All `/v1/*` endpoints require a Bearer token in the `Authorization` header.

```
Authorization: Bearer cru_live_A3F9NK4M7QHGBVTP
```

The key prefix (`cru_` by default) is configurable per product via the `API_KEY_PREFIX` environment variable. The `live_` segment distinguishes production keys from future test-key variants.

Keys are salted SHA-256 hashes stored in Postgres with a Redis hot cache for low-latency lookup. Invalid or missing keys receive a `401 Unauthorized` response.

### Key Format

```
{prefix}live_{base32-entropy}
```

- Prefix: configurable, default `cru_`
- Entropy: 192 bits (24 bytes) encoded as base32 without padding
- Display prefix: first 24 characters shown in the dashboard for identification

## Endpoints

### POST /v1/echo

Echo endpoint. Returns the request payload wrapped in an echo envelope. Use this to verify your integration is working.

**Headers:**

| Header | Required | Value |
|---|---|---|
| `Authorization` | Yes | `Bearer <api-key>` |
| `Content-Type` | Yes | `application/json` |
| `X-Request-ID` | No | Your request correlation ID (max 64 chars). Generated server-side if omitted. |

**Request body:** Any valid JSON object.

```json
{
  "message": "hello",
  "count": 42
}
```

**Response (200):**

```json
{
  "payload": {
    "echo": {
      "message": "hello",
      "count": 42
    },
    "operation": "echo"
  },
  "billable_units": 1
}
```

**Response headers:**

| Header | Description |
|---|---|
| `X-Request-ID` | Unique request identifier. Echoed back if you sent one, otherwise server-generated UUID. |
| `X-Billable-Units` | Number of units consumed by this request. |
| `X-Units-Label` | Human-readable label for billable units (e.g. "calls", "pages", "tokens"). Omitted if the worker does not provide one. |

**curl:**

```bash
curl -X POST https://api.yourproduct.com/v1/echo \
  -H 'Authorization: Bearer cru_live_A3F9NK4M7QHGBVTP' \
  -H 'Content-Type: application/json' \
  -d '{"message": "hello"}'
```

### POST /v1/{operation}

All product-specific operations are exposed at `/v1/{operation}`. The gateway forwards the request to the worker with the operation name. Available operations depend on your product.

**Headers:**

| Header | Required | Value |
|---|---|---|
| `Authorization` | Yes | `Bearer <api-key>` |
| `Content-Type` | Yes | `application/json` |
| `X-Request-ID` | No | Your request correlation ID (max 64 chars). |

**Request body:** Operation-specific JSON. The shape is defined by the worker for each operation.

```json
{
  "field1": "value",
  "field2": 123
}
```

**Response (200):**

```json
{
  "payload": {
    "operation_result": "...",
    "operation": "operation_name"
  },
  "billable_units": 1,
  "units_label": "calls"
}
```

The `billable_units` field indicates how many metered units this request consumed. Flat-rate operations return `1`. Per-item operations (pages parsed, images processed, tokens consumed) return the actual count.

**Response headers:**

| Header | Description |
|---|---|
| `X-Request-ID` | Unique request identifier. |
| `X-Billable-Units` | Units consumed. |
| `X-Units-Label` | Label for the units (e.g. "pages", "images", "tokens"). |

**curl:**

```bash
curl -X POST https://api.yourproduct.com/v1/validate-vat \
  -H 'Authorization: Bearer cru_live_A3F9NK4M7QHGBVTP' \
  -H 'Content-Type: application/json' \
  -d '{"vat_number": "DE123456789"}'
```

### GET /healthz

Public health check. No authentication required.

**Response (200):**

```json
{"status": "ok"}
```

### GET /readyz

Readiness probe. Checks Redis and Postgres connectivity. No authentication required.

**Response (200):**

```json
{
  "status": "ok",
  "checks": {
    "redis": "ok",
    "postgres": "ok"
  }
}
```

Status is `"degraded"` if any dependency is unreachable.

## Error Reference

All errors follow a consistent envelope:

```json
{
  "error": {
    "code": "ERROR_CODE",
    "message": "Human-readable description",
    "retryable": true
  }
}
```

The `retryable` field indicates whether the same request might succeed if retried. It is present on worker errors and rate limit responses. Gateway errors omit `retryable`.

### Gateway Errors

| HTTP Status | Code | Meaning |
|---|---|---|
| 400 | `BAD_REQUEST` | Invalid JSON in the request body. |
| 401 | `UNAUTHORIZED` | Missing, malformed, or invalid API key. |
| 500 | `INTERNAL` | Auth lookup failed due to internal error (message: "auth lookup failed"). |
| 429 | `RATE_LIMITED` | Per-minute rate limit exceeded for your plan. Retry after 60 seconds. |
| 429 | `QUOTA_EXCEEDED` | Monthly billable-unit cap reached. Does not reset until the next billing cycle. |
| 500 | `INTERNAL` | Unexpected server error. Include `X-Request-ID` when reporting. |
| 502 | `WORKER_UNREACHABLE` | The worker process did not respond within the timeout (default 10s). |
| 502 | `WORKER_BAD_RESPONSE` | Worker returned success with `billable_units < 1`. Contract violation. |

### Worker Errors

Worker errors are returned with HTTP status `502 Bad Gateway`. The `retryable` field tells you whether to retry.

| Code | Meaning | Retryable |
|---|---|---|
| `WORKER_UNREACHABLE` | Worker process unavailable or timed out. | Yes |
| `WORKER_BAD_RESPONSE` | Worker contract violation (billable_units < 1 on success). | No |
| *operation-specific* | Defined by the worker per operation. Check your product docs. | Varies |

When `WORKER_ERROR_EXPOSURE` is set to `full`, the worker's original error code and message are passed through. When set to `sanitized` (the default), all worker errors are collapsed to `WORKER_UNREACHABLE` with a generic message.

### Error Response Examples

**Rate limited:**

```json
{
  "error": {
    "code": "RATE_LIMITED",
    "message": "rate limit exceeded",
    "retryable": true
  }
}
```

**Quota exceeded:**

```json
{
  "error": {
    "code": "QUOTA_EXCEEDED",
    "message": "monthly usage quota reached",
    "retryable": false
  }
}
```

**Invalid API key:**

```json
{
  "error": {
    "code": "UNAUTHORIZED",
    "message": "invalid api key"
  }
}
```

## Rate Limits

Rate limits are enforced per customer on a **sliding window** (last 60 seconds). The limit depends on your plan tier.

### Plan Tiers

| Plan | Requests/min | Monthly cap |
|---|---|---|
| Free | 60 | 1,000 units |
| Pro | Configurable | Configurable |
| Business | Configurable | Unlimited |

Exact limits for Pro and Business tiers are set by the product operator. Check your dashboard or plan documentation for your specific limits.

### Rate Limit Headers

Every response includes these headers:

| Header | Description |
|---|---|
| `X-Request-ID` | Unique identifier for this request. Use when reporting issues. |
| `X-Billable-Units` | Units consumed by this request (success responses only). |
| `X-Units-Label` | Human label for billable units (success responses only). |

When rate limited (HTTP 429), the response also includes:

| Header | Description |
|---|---|
| `Retry-After` | Seconds to wait before retrying (always `60`). |

### Sliding Window Behavior

The sliding window prevents burst abuse at minute boundaries. Unlike fixed-window limiters, you cannot send a full batch at second 59 and another at second 61. The window always covers the most recent 60 seconds.

If Redis is unreachable, the rate limiter **fails open** and allows the request through. This is intentional: refusing legitimate traffic on a Redis blip is worse than allowing a few excess requests.

## Monthly Quotas

In addition to per-minute rate limits, each plan has a monthly billable-unit cap (except Business, which is unlimited).

- Quotas are tracked via atomic Redis counters keyed by customer and month.
- When the cap is reached, further requests receive `429 QUOTA_EXCEEDED` with `retryable: false`.
- If a request reserves quota but the worker fails, the reserve is refunded automatically.
- Quotas reset at the start of each billing cycle.

## Webhooks

The Stripe webhook endpoint (`POST /webhooks/stripe`) is an internal integration point. It is not part of the customer-facing API. Customers do not need to interact with it.

Stripe webhooks handle:
- Subscription creation and updates (plan changes)
- Subscription cancellations (downgrade to free tier)
- Metered billing events (unit consumption reporting)

All webhook events are HMAC-verified and deduplicated against the `webhook_events` table.

## Verifying Webhooks

When you register a webhook endpoint in the dashboard, every delivery is signed with HMAC-SHA256 so you can verify the request originated from the gateway and has not been tampered with.

### Headers

| Header | Description |
|---|---|
| `X-Crucible-Signature` | `t=<unix_ts>,v1=<hex_hmac_sha256>` — signature and timestamp in one header |
| `X-Crucible-Timestamp` | Unix timestamp (seconds). **Do not use for verification** — use `t=` in `X-Crucible-Signature`. Provided for logging/tracing convenience only. |
| `X-Webhook-Event-ID` | UUID for idempotent delivery. Use this to deduplicate at-least-once deliveries. |
| `X-Webhook-Event-Type` | Event type string (e.g. `invoice.paid`). |

### Verification Algorithm

1. Extract `t=<ts>` and one or more `v1=<sig>` values from `X-Crucible-Signature`.
2. Reject if the timestamp `t` is older than your tolerance window (default: 5 minutes).
3. Compute `HMAC-SHA256(secret_bytes, ts + "." + raw_body)`.
4. Constant-time compare the digest against each `v1=` candidate.

The signing secret (`secret_hex`) is shown once at endpoint creation time in the dashboard. Store it as an environment variable.

**Important:** Always pass the raw request body bytes to the verifier before any JSON parsing. Re-serialising the parsed body changes whitespace and field order, which invalidates the signature.

### Go

```go
import (
    "io"
    "net/http"
    "os"
    "time"

    crucible "github.com/Unluckyathecking/crucible/clients/go"
)

func handleWebhook(w http.ResponseWriter, r *http.Request) {
    body, err := io.ReadAll(r.Body)
    if err != nil {
        http.Error(w, "bad request", http.StatusBadRequest)
        return
    }
    if err := crucible.VerifyWebhook(
        os.Getenv("WEBHOOK_SECRET"),
        r.Header.Get(crucible.SignatureHeader),
        body,
        5*time.Minute,
    ); err != nil {
        http.Error(w, "invalid signature", http.StatusUnauthorized)
        return
    }
    // process event ...
}
```

### TypeScript / Node.js

```typescript
import { verifyWebhook, WebhookVerificationError, SIGNATURE_HEADER } from "@crucible/client";

// Express example — ensure you use express.raw() or similar to capture the raw body.
app.post("/webhook", express.raw({ type: "application/json" }), (req, res) => {
  const secret = process.env.WEBHOOK_SECRET;
  if (!secret) {
    res.status(500).json({ error: "webhook secret not configured" });
    return;
  }
  const sigHeader = req.headers[SIGNATURE_HEADER.toLowerCase()];
  if (typeof sigHeader !== "string") {
    res.status(401).json({ error: "missing signature header" });
    return;
  }
  try {
    verifyWebhook(secret, sigHeader, req.body as Buffer);
  } catch (err) {
    if (err instanceof WebhookVerificationError) {
      res.status(401).json({ error: "invalid signature" });
      return;
    }
    throw err;
  }
  // process event ...
});
```

### Security Notes

- The gateway caps the number of `v1=` candidates it parses to prevent header-stuffing DoS attacks. Your verifier does the same.
- The 5-minute tolerance window matches the gateway's inbound Stripe webhook replay protection. Do not widen it.
- Use constant-time comparison (`hmac.Equal` in Go, `crypto.timingSafeEqual` in Node.js) — the SDK helpers handle this for you.

## Request ID

Every request receives an `X-Request-ID` header. If you send one (max 64 characters), it is echoed back. Otherwise the gateway generates a UUID.

Use `X-Request-ID` to correlate logs, trace requests across gateway and worker, and reference specific requests when reporting issues.

## Body Size Limit

The default maximum request body size is **1 MB** (1,048,576 bytes). Requests exceeding this limit are rejected with `400 Bad Request`. Per-route limits can be configured in the handler.

## Security Headers

Every response includes these security headers:

| Header | Value |
|---|---|
| `Strict-Transport-Security` | `max-age=63072000; includeSubDomains` |
| `X-Content-Type-Options` | `nosniff` |
| `X-Frame-Options` | `DENY` |
| `X-XSS-Protection` | `0` |
| `Referrer-Policy` | `strict-origin-when-cross-origin` |
| `Permissions-Policy` | `camera=(), microphone=(), geolocation=(), interest-cohort=()` |

## CORS

The gateway allows cross-origin requests from the configured dashboard origin only (`DASHBOARD_ORIGIN` env var, default `http://localhost:3001`). Allowed methods: `GET`, `POST`, `OPTIONS`. Allowed headers: `Authorization`, `Content-Type`, `X-Request-ID`. Credentials are not supported for cross-origin requests.
