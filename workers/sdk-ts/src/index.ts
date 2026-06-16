// Package crucible is the TypeScript SDK for Crucible workers.
//
// A worker is just an HTTP server with two endpoints:
//
//   POST /invoke  — handles one Invoke() request, returns the result + billable_units
//   GET  /healthz — returns 200 OK when ready
//
// This SDK provides the boilerplate so a complete worker is one function:
//
//   import { serve } from '@crucible/worker-sdk-ts';
//
//   serve(8081, (req) => ({
//     payload: { hello: 'world' },
//     billable_units: 1,
//   }));

import * as crypto from 'crypto';
import * as http from 'http';

/** Mirrors InvokeRequest from the frozen worker proto contract. */
export interface Request {
  request_id: string;
  customer_id: string;
  operation: string;
  payload: unknown;
  plan: string;
  metadata: Record<string, string>;
}

/** What a WorkerHandler returns on success. billable_units defaults to 1 if zero or absent. */
export interface Response {
  payload: unknown;
  billable_units?: number;
  units_label?: string;
}

/**
 * Structured error a handler can throw to surface a stable code to the caller.
 * A plain Error is also accepted — the SDK wraps it as a generic INTERNAL error
 * (the real cause is logged but never surfaced in the response).
 */
export class WorkerError extends Error {
  readonly code: string;
  readonly retryable: boolean;

  constructor(code: string, message: string, retryable = false) {
    super(message);
    this.name = 'WorkerError';
    this.code = code;
    this.retryable = retryable;
  }
}

/** The worker's single entry point — handle one Invoke call, return a Response or throw. */
export type WorkerHandler = (req: Request) => Promise<Response> | Response;

/**
 * Optional configuration for the worker HTTP server.
 * All fields are optional; the zero value preserves today's behaviour.
 */
export interface ServerConfig {
  /**
   * HMAC-SHA256 key for inbound /invoke request verification.
   * When set, unsigned or forged calls are rejected with UNAUTHORIZED.
   * Empty/undefined (the default) disables verification — opt-in only.
   * Falls back to WORKER_SHARED_SECRET from the environment when omitted.
   */
  sharedSecret?: string;
}

// ---------------------------------------------------------------------------
// Prometheus metrics — zero runtime dependencies, manual text exposition format.
// Metric names are byte-identical across Go/Rust/TS (parity contract).
// ---------------------------------------------------------------------------

export const METRIC_REQUESTS_TOTAL = 'crucible_worker_requests_total';
export const METRIC_ERRORS_TOTAL = 'crucible_worker_errors_total';
export const METRIC_DURATION_SECS = 'crucible_worker_request_duration_seconds';

// Default Prometheus histogram bucket boundaries in seconds — mirrors Go SDK's prometheus.DefBuckets.
const HISTOGRAM_BUCKETS = [0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10];

interface LabelSet {
  operation: string;
  outcome: string;
}

function labelKey(labels: LabelSet): string {
  // NUL separator keeps keys collision-free without JSON overhead.
  return `${labels.operation}\x00${labels.outcome}`;
}

interface HistogramState {
  sum: number;
  count: number;
  buckets: number[]; // per-bucket count, parallel to HISTOGRAM_BUCKETS
}

// WorkerMetrics is intentionally not exported — callers interact with it only through
// createServerWithMetrics (internal) and serve() (public). The Prometheus text-format
// emitter is self-contained so no runtime dependency is needed.
class WorkerMetrics {
  private readonly requests = new Map<string, { labels: LabelSet; count: number }>();
  private readonly errors = new Map<string, { labels: LabelSet; count: number }>();
  private readonly durations = new Map<string, { labels: LabelSet; state: HistogramState }>();

  observe(operation: string, outcome: string, elapsedSecs: number): void {
    const labels: LabelSet = { operation, outcome };
    const key = labelKey(labels);

    // requests counter
    const req = this.requests.get(key);
    if (req) {
      req.count++;
    } else {
      this.requests.set(key, { labels, count: 1 });
    }

    // errors counter
    if (outcome === 'error') {
      const err = this.errors.get(key);
      if (err) {
        err.count++;
      } else {
        this.errors.set(key, { labels, count: 1 });
      }
    }

    // histogram
    let hist = this.durations.get(key);
    if (!hist) {
      hist = {
        labels,
        state: { sum: 0, count: 0, buckets: new Array(HISTOGRAM_BUCKETS.length).fill(0) as number[] },
      };
      this.durations.set(key, hist);
    }
    // Clamp to zero in case of clock skew (Date.now() subtraction can theoretically
    // return a negative value on some platforms or when the clock is stepped back).
    const elapsed = Math.max(0, elapsedSecs);
    hist.state.sum += elapsed;
    hist.state.count++;
    // Increment only the first bucket boundary that fits the observation.
    // renderText accumulates these non-cumulatively to produce the Prometheus
    // cumulative _bucket output, so each bucket must store a non-cumulative count.
    for (let i = 0; i < HISTOGRAM_BUCKETS.length; i++) {
      if (elapsed <= (HISTOGRAM_BUCKETS[i] as number)) {
        hist.state.buckets[i] = (hist.state.buckets[i] as number) + 1;
        break;
      }
    }
  }

  renderText(): string {
    const lines: string[] = [];

    // requests counter
    lines.push(`# HELP ${METRIC_REQUESTS_TOTAL} Total /invoke requests handled by the worker.`);
    lines.push(`# TYPE ${METRIC_REQUESTS_TOTAL} counter`);
    for (const { labels, count } of this.requests.values()) {
      lines.push(
        `${METRIC_REQUESTS_TOTAL}{operation="${labels.operation}",outcome="${labels.outcome}"} ${count}`,
      );
    }

    // errors counter
    lines.push(`# HELP ${METRIC_ERRORS_TOTAL} Total /invoke requests that returned an error envelope.`);
    lines.push(`# TYPE ${METRIC_ERRORS_TOTAL} counter`);
    for (const { labels, count } of this.errors.values()) {
      lines.push(
        `${METRIC_ERRORS_TOTAL}{operation="${labels.operation}",outcome="${labels.outcome}"} ${count}`,
      );
    }

    // duration histogram
    lines.push(`# HELP ${METRIC_DURATION_SECS} Latency of /invoke handler calls in seconds.`);
    lines.push(`# TYPE ${METRIC_DURATION_SECS} histogram`);
    for (const { labels, state } of this.durations.values()) {
      const lstr = `operation="${labels.operation}",outcome="${labels.outcome}"`;
      let cumulative = 0;
      for (let i = 0; i < HISTOGRAM_BUCKETS.length; i++) {
        cumulative += state.buckets[i] as number;
        lines.push(`${METRIC_DURATION_SECS}_bucket{${lstr},le="${HISTOGRAM_BUCKETS[i]!}"} ${cumulative}`);
      }
      lines.push(`${METRIC_DURATION_SECS}_bucket{${lstr},le="+Inf"} ${state.count}`);
      lines.push(`${METRIC_DURATION_SECS}_sum{${lstr}} ${state.sum}`);
      lines.push(`${METRIC_DURATION_SECS}_count{${lstr}} ${state.count}`);
    }

    return lines.join('\n') + '\n';
  }
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

/**
 * Creates a Node.js HTTP server pre-wired to the worker contract:
 *   POST /invoke  — decodes, verifies signature (if configured), calls handler.
 *   GET  /healthz — returns 200 OK.
 *
 * Use serve() for the standard lifecycle (signal handling, graceful drain).
 * Use createServer() when you need to manage the server lifecycle yourself.
 */
export function createServer(handler: WorkerHandler, config: ServerConfig = {}): http.Server {
  const secret = config.sharedSecret ?? process.env['WORKER_SHARED_SECRET'] ?? '';
  return http.createServer((req, res) => {
    void dispatch(req, res, handler, secret, undefined);
  });
}

/**
 * Runs the worker on the given port and blocks until SIGINT/SIGTERM,
 * then drains in-flight requests for up to 10 s.
 * When WORKER_METRICS_PORT is set, a second server serving /metrics is started on
 * that port before the main server begins accepting connections. Both servers are
 * shut down gracefully when the signal fires.
 */
export function serve(port: number, handler: WorkerHandler, config: ServerConfig = {}): Promise<void> {
  const secret = config.sharedSecret ?? process.env['WORKER_SHARED_SECRET'] ?? '';

  const metricsResult = initMetrics();
  const metrics = metricsResult?.metrics;
  const mServer = metricsResult?.server;

  const server = http.createServer((req, res) => {
    void dispatch(req, res, handler, secret, metrics);
  });

  return new Promise<void>((resolve, reject) => {
    server.listen(port, () => {
      log('info', { port, msg: 'worker listening' });
    });

    server.on('error', reject);

    const shutdown = (): void => {
      log('info', { msg: 'worker shutting down' });
      // Hard deadline: resolve after 10 s even if servers are slow to drain.
      const timer = setTimeout(resolve, 10_000);
      if (typeof (timer as NodeJS.Timeout).unref === 'function') {
        (timer as NodeJS.Timeout).unref();
      }
      // Close both the invoke server and the metrics server (when running).
      let pending = mServer ? 2 : 1;
      const onClose = (err?: Error): void => {
        if (err) { clearTimeout(timer); reject(err); return; }
        if (--pending === 0) { clearTimeout(timer); resolve(); }
      };
      server.close(onClose);
      mServer?.close(onClose);
    };

    process.once('SIGINT', shutdown);
    process.once('SIGTERM', shutdown);
  });
}

// initMetrics reads WORKER_METRICS_PORT and, if valid, creates a WorkerMetrics instance
// and starts a /metrics HTTP server on that port. Returns null when the env var is unset
// or invalid — keeping metrics off by default so existing clones and smoke tests are unchanged.
// The returned server must be closed by the caller during shutdown so it doesn't leak.
function initMetrics(): { metrics: WorkerMetrics; server: http.Server } | null {
  const portStr = process.env['WORKER_METRICS_PORT'];
  if (!portStr) return null;
  const port = parseInt(portStr, 10);
  if (!Number.isFinite(port) || port <= 0 || port > 65535) return null;

  const metrics = new WorkerMetrics();

  const mServer = http.createServer((req, res) => {
    if (req.url === '/metrics' && req.method === 'GET') {
      try {
        const body = metrics.renderText();
        res.writeHead(200, { 'Content-Type': 'text/plain; version=0.0.4; charset=utf-8' });
        res.end(body);
      } catch {
        res.writeHead(500, { 'Content-Type': 'text/plain' });
        res.end('Internal Server Error');
      }
    } else {
      res.writeHead(404);
      res.end();
    }
  });

  mServer.listen(port, () => {
    log('info', { metrics_port: port, msg: 'worker metrics listening' });
  });

  return { metrics, server: mServer };
}

/** Maximum age (or future skew) of a signed request in seconds. Mirrors the Stripe replay window. */
const WORKER_SIG_WINDOW = 300;

/** Header carrying the inbound channel-auth signature (lowercase — Node normalises). */
const WORKER_SIG_HEADER = 'x-worker-signature';

/**
 * Verify the X-Worker-Signature header against body using secret.
 * Throws on any verification failure. Returns void on success.
 * The thrown error detail is never forwarded to the caller.
 *
 * Signing scheme (byte-identical across Go/Rust/TS):
 *   HMAC-SHA256(secret, timestamp + "." + body)
 *   Header: t=<unix-seconds>,v1=<hex-digest>
 */
function verifyWorkerSig(header: string, body: Buffer, secret: string): void {
  let tsStr: string | undefined;
  let sigHex: string | undefined;

  for (const part of header.split(',')) {
    if (part.startsWith('t=')) tsStr = part.slice(2);
    else if (part.startsWith('v1=')) sigHex = part.slice(3);
  }

  if (!tsStr || !sigHex) throw new Error('missing timestamp or signature in header');

  const ts = parseInt(tsStr, 10);
  if (!Number.isFinite(ts)) throw new Error('invalid timestamp');

  const now = Math.floor(Date.now() / 1000);
  if (Math.abs(now - ts) > WORKER_SIG_WINDOW) throw new Error('stale timestamp');

  // Compute expected HMAC-SHA256(secret, ts + "." + body).
  const expected = crypto
    .createHmac('sha256', Buffer.from(secret, 'utf8'))
    .update(tsStr + '.')
    .update(body)
    .digest();

  const provided = Buffer.from(sigHex, 'hex');
  if (provided.length !== 32) throw new Error('invalid signature length');

  // Constant-time comparison — must be same-length buffers.
  if (!crypto.timingSafeEqual(expected, provided)) throw new Error('signature mismatch');
}

async function dispatch(
  req: http.IncomingMessage,
  res: http.ServerResponse,
  handler: WorkerHandler,
  secret: string,
  metrics: WorkerMetrics | undefined,
): Promise<void> {
  if (req.url === '/healthz' && req.method === 'GET') {
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ status: 'ok' }));
    return;
  }

  if (req.url !== '/invoke' || req.method !== 'POST') {
    res.writeHead(404);
    res.end();
    return;
  }

  let rawBuffer: Buffer;
  try {
    rawBuffer = await readBody(req, 10 * 1024 * 1024);
  } catch {
    writeError(res, { code: 'BAD_REQUEST', message: 'request body too large', retryable: false });
    return;
  }

  // Verify the HMAC-SHA256 channel-auth signature when configured.
  // Empty secret → skip verification → today's behaviour preserved (opt-in only).
  if (secret) {
    const sigHeader = req.headers[WORKER_SIG_HEADER];
    try {
      verifyWorkerSig(
        typeof sigHeader === 'string' ? sigHeader : '',
        rawBuffer,
        secret,
      );
    } catch {
      // Surface only a stable code; the signature detail is never echoed.
      writeError(res, { code: 'UNAUTHORIZED', message: 'invalid request signature', retryable: false });
      return;
    }
  }

  let workerReq: Request;
  try {
    workerReq = JSON.parse(rawBuffer.toString('utf8')) as Request;
  } catch {
    writeError(res, { code: 'BAD_REQUEST', message: 'invalid request body', retryable: false });
    return;
  }

  // Metric tracking starts after successful decode — operation is now a bounded
  // product-defined string, never a raw URL path or per-request identifier.
  const start = Date.now();

  let result: Response;
  try {
    result = await Promise.resolve(handler(workerReq));
  } catch (err) {
    const elapsedSecs = (Date.now() - start) / 1000;
    if (err instanceof WorkerError) {
      log('info', {
        request_id: workerReq.request_id,
        operation: workerReq.operation,
        code: err.code,
        msg: 'handler returned structured error',
      });
      metrics?.observe(workerReq.operation, 'error', elapsedSecs);
      writeError(res, { code: err.code, message: err.message, retryable: err.retryable });
      return;
    }
    log('error', {
      request_id: workerReq.request_id,
      operation: workerReq.operation,
      msg: 'handler failed',
    });
    metrics?.observe(workerReq.operation, 'error', elapsedSecs);
    writeError(res, { code: 'INTERNAL', message: 'internal error', retryable: true });
    return;
  }

  const elapsedSecs = (Date.now() - start) / 1000;

  if (!result.billable_units || result.billable_units < 1) {
    result = { ...result, billable_units: 1 };
  }

  metrics?.observe(workerReq.operation, 'ok', elapsedSecs);

  res.writeHead(200, { 'Content-Type': 'application/json' });
  res.end(JSON.stringify(result));
}

function readBody(req: http.IncomingMessage, maxBytes: number): Promise<Buffer> {
  return new Promise<Buffer>((resolve, reject) => {
    const chunks: Buffer[] = [];
    let size = 0;
    req.on('data', (chunk: Buffer) => {
      size += chunk.length;
      if (size > maxBytes) {
        reject(new Error('body too large'));
        req.destroy();
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

interface ErrorShape {
  code: string;
  message: string;
  retryable: boolean;
}

function writeError(res: http.ServerResponse, err: ErrorShape): void {
  res.writeHead(200, { 'Content-Type': 'application/json' });
  res.end(JSON.stringify({ error: err }));
}

function log(level: string, data: Record<string, unknown>): void {
  process.stdout.write(JSON.stringify({ level, time: Date.now(), ...data }) + '\n');
}
