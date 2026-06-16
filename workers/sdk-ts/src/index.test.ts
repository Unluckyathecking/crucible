import { test } from 'node:test';
import assert from 'node:assert/strict';
import * as crypto from 'node:crypto';
import * as http from 'node:http';
import { createServer, serve, WorkerError, METRIC_REQUESTS_TOTAL, METRIC_ERRORS_TOTAL, METRIC_DURATION_SECS } from './index';
import type { ServerConfig, WorkerHandler } from './index';

/** Send an HTTP request and return status + parsed JSON body. */
function rawRequest(
  port: number,
  path: string,
  method: string,
  bodyStr?: string,
  extraHeaders?: Record<string, string>,
): Promise<{ status: number; data: unknown }> {
  return new Promise((resolve, reject) => {
    const headers: Record<string, string | number> = { ...extraHeaders };
    if (bodyStr !== undefined) {
      headers['Content-Type'] = 'application/json';
      headers['Content-Length'] = Buffer.byteLength(bodyStr);
    }
    const req = http.request(
      { hostname: '127.0.0.1', port, path, method, headers },
      (res) => {
        const chunks: Buffer[] = [];
        res.on('data', (c: Buffer) => chunks.push(c));
        res.on('end', () =>
          resolve({ status: res.statusCode ?? 0, data: JSON.parse(Buffer.concat(chunks).toString()) }),
        );
      },
    );
    req.on('error', reject);
    if (bodyStr !== undefined) req.write(bodyStr);
    req.end();
  });
}

function request(
  port: number,
  path: string,
  method: string,
  body?: unknown,
  extraHeaders?: Record<string, string>,
): Promise<{ status: number; data: unknown }> {
  return rawRequest(port, path, method, body !== undefined ? JSON.stringify(body) : undefined, extraHeaders);
}

async function withServer(handler: WorkerHandler, fn: (port: number) => Promise<void>): Promise<void> {
  const server = createServer(handler);
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const port = (server.address() as { port: number }).port;
  try {
    await fn(port);
  } finally {
    await new Promise<void>((resolve, reject) =>
      server.close((err) => (err ? reject(err) : resolve())),
    );
  }
}

async function withServerConfig(
  handler: WorkerHandler,
  config: ServerConfig,
  fn: (port: number) => Promise<void>,
): Promise<void> {
  const server = createServer(handler, config);
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const port = (server.address() as { port: number }).port;
  try {
    await fn(port);
  } finally {
    await new Promise<void>((resolve, reject) =>
      server.close((err) => (err ? reject(err) : resolve())),
    );
  }
}

/**
 * Build a valid X-Worker-Signature header for the given raw body string and secret.
 * Signing scheme: HMAC-SHA256(secret, ts + "." + body) — byte-identical to Go/Rust.
 */
function makeSignature(bodyStr: string, secret: string): string {
  const ts = Math.floor(Date.now() / 1000).toString();
  const mac = crypto.createHmac('sha256', Buffer.from(secret, 'utf8'));
  mac.update(ts + '.');
  mac.update(Buffer.from(bodyStr, 'utf8'));
  return `t=${ts},v1=${mac.digest('hex')}`;
}

// --- existing tests ----------------------------------------------------------

test('GET /healthz returns 200 with status ok', async () => {
  await withServer(() => ({ payload: {} }), async (port) => {
    const { status, data } = await request(port, '/healthz', 'GET');
    assert.equal(status, 200);
    assert.deepEqual(data, { status: 'ok' });
  });
});

test('POST /invoke returns payload and billable_units', async () => {
  const handler: WorkerHandler = (req) => ({
    payload: { echo: req.payload },
    billable_units: 2,
  });
  await withServer(handler, async (port) => {
    const { status, data } = await request(port, '/invoke', 'POST', {
      operation: 'echo',
      payload: { x: 1 },
    });
    assert.equal(status, 200);
    assert.deepEqual(data, { payload: { echo: { x: 1 } }, billable_units: 2 });
  });
});

test('billable_units defaults to 1 when absent', async () => {
  await withServer(() => ({ payload: 'ok' }), async (port) => {
    const { data } = await request(port, '/invoke', 'POST', { operation: 'test' });
    assert.equal((data as { billable_units: number }).billable_units, 1);
  });
});

test('WorkerError surfaces as structured error envelope', async () => {
  const handler: WorkerHandler = () => {
    throw new WorkerError('NOT_FOUND', 'thing not found', false);
  };
  await withServer(handler, async (port) => {
    const { status, data } = await request(port, '/invoke', 'POST', { operation: 'test' });
    assert.equal(status, 200);
    assert.deepEqual(data, {
      error: { code: 'NOT_FOUND', message: 'thing not found', retryable: false },
    });
  });
});

test('plain Error becomes generic INTERNAL error', async () => {
  const handler: WorkerHandler = () => {
    throw new Error('secret internal detail');
  };
  await withServer(handler, async (port) => {
    const { status, data } = await request(port, '/invoke', 'POST', { operation: 'test' });
    assert.equal(status, 200);
    assert.deepEqual(data, {
      error: { code: 'INTERNAL', message: 'internal error', retryable: true },
    });
  });
});

// --- HMAC-SHA256 channel-auth tests ------------------------------------------
// Test matrix mirrors billing/webhook_test.go: valid, missing, wrong-secret,
// tampered-body, stale-timestamp, and disabled-path.

test('signature: valid signature accepted', async () => {
  const secret = 'ts-test-secret-valid';
  const bodyStr = JSON.stringify({ operation: 'test', payload: {} });
  const sig = makeSignature(bodyStr, secret);

  await withServerConfig(() => ({ payload: 'ok' }), { sharedSecret: secret }, async (port) => {
    const { status, data } = await rawRequest(port, '/invoke', 'POST', bodyStr, {
      'x-worker-signature': sig,
    });
    assert.equal(status, 200);
    assert.ok(
      !(data as Record<string, unknown>)['error'],
      `expected no error, got ${JSON.stringify(data)}`,
    );
  });
});

test('signature: missing signature rejected', async () => {
  const bodyStr = JSON.stringify({ operation: 'test' });

  await withServerConfig(() => ({ payload: 'ok' }), { sharedSecret: 'ts-test-secret-missing' }, async (port) => {
    // No x-worker-signature header
    const { data } = await rawRequest(port, '/invoke', 'POST', bodyStr);
    assert.equal(
      (data as Record<string, { code: string }>)['error']?.code,
      'UNAUTHORIZED',
    );
  });
});

test('signature: wrong secret rejected', async () => {
  const bodyStr = JSON.stringify({ operation: 'test' });
  const wrongSig = makeSignature(bodyStr, 'wrong-secret');

  await withServerConfig(() => ({ payload: 'ok' }), { sharedSecret: 'correct-secret' }, async (port) => {
    const { data } = await rawRequest(port, '/invoke', 'POST', bodyStr, {
      'x-worker-signature': wrongSig,
    });
    assert.equal(
      (data as Record<string, { code: string }>)['error']?.code,
      'UNAUTHORIZED',
    );
  });
});

test('signature: tampered body rejected', async () => {
  const secret = 'ts-tamper-secret';
  const originalBody = JSON.stringify({ operation: 'original' });
  const tamperedBody = JSON.stringify({ operation: 'TAMPERED' });
  // Sign the original body but send the tampered body — HMAC must fail.
  const sig = makeSignature(originalBody, secret);

  await withServerConfig(() => ({ payload: 'ok' }), { sharedSecret: secret }, async (port) => {
    const { data } = await rawRequest(port, '/invoke', 'POST', tamperedBody, {
      'x-worker-signature': sig,
    });
    assert.equal(
      (data as Record<string, { code: string }>)['error']?.code,
      'UNAUTHORIZED',
    );
  });
});

test('signature: stale timestamp rejected', async () => {
  const secret = 'ts-stale-secret';
  const bodyStr = JSON.stringify({ operation: 'test' });

  // Build a signature with a timestamp 10 minutes in the past (outside the 5-minute window).
  const staleTs = Math.floor(Date.now() / 1000) - 600;
  const mac = crypto.createHmac('sha256', Buffer.from(secret, 'utf8'));
  mac.update(staleTs.toString() + '.');
  mac.update(Buffer.from(bodyStr, 'utf8'));
  const staleSig = `t=${staleTs},v1=${mac.digest('hex')}`;

  await withServerConfig(() => ({ payload: 'ok' }), { sharedSecret: secret }, async (port) => {
    const { data } = await rawRequest(port, '/invoke', 'POST', bodyStr, {
      'x-worker-signature': staleSig,
    });
    assert.equal(
      (data as Record<string, { code: string }>)['error']?.code,
      'UNAUTHORIZED',
    );
  });
});

test('signature: disabled path — unsigned call succeeds when no secret configured', async () => {
  await withServerConfig(() => ({ payload: 'ok' }), {}, async (port) => {
    // No signature header and no secret configured — must succeed (today's behaviour).
    const { status, data } = await request(port, '/invoke', 'POST', { operation: 'test' });
    assert.equal(status, 200);
    assert.ok(
      !(data as Record<string, unknown>)['error'],
      `expected no error (signing disabled), got ${JSON.stringify(data)}`,
    );
  });
});

// --- Prometheus metrics tests ------------------------------------------------

// Parity: metric name constants must be byte-identical across Go/Rust/TS.
test('metrics: name constants match cross-SDK parity contract', () => {
  assert.equal(METRIC_REQUESTS_TOTAL, 'crucible_worker_requests_total');
  assert.equal(METRIC_ERRORS_TOTAL, 'crucible_worker_errors_total');
  assert.equal(METRIC_DURATION_SECS, 'crucible_worker_request_duration_seconds');
});

// Disabled path: when WORKER_METRICS_PORT is unset, createServer binds only one listener
// and /invoke behaves identically to today's behaviour (no metrics side effects).
test('metrics: disabled path — no extra listener, /invoke unchanged', async () => {
  const saved = process.env['WORKER_METRICS_PORT'];
  delete process.env['WORKER_METRICS_PORT'];

  try {
    await withServer(() => ({ payload: 'ok', billable_units: 1 }), async (port) => {
      const { status, data } = await request(port, '/invoke', 'POST', { operation: 'test' });
      assert.equal(status, 200);
      assert.ok(
        !(data as Record<string, unknown>)['error'],
        `expected success with metrics disabled, got ${JSON.stringify(data)}`,
      );
      assert.equal((data as Record<string, number>)['billable_units'], 1);
    });
  } finally {
    if (saved !== undefined) process.env['WORKER_METRICS_PORT'] = saved;
  }
});

// End-to-end metrics test: start the real serve() so initMetrics() wires the /metrics
// listener, fire one /invoke request, then scrape /metrics and assert bounded labels.
test('metrics: /metrics endpoint serves text-format with bounded {operation,outcome} labels', async () => {
  function allocPort(): Promise<number> {
    return new Promise<number>((resolve, reject) => {
      const s = http.createServer();
      s.listen(0, '127.0.0.1', () => {
        const p = (s.address() as { port: number }).port;
        s.close((err) => (err ? reject(err) : resolve(p)));
      });
    });
  }

  const [invokePort, metricsPort] = await Promise.all([allocPort(), allocPort()]);
  process.env['WORKER_METRICS_PORT'] = String(metricsPort);

  try {
    // serve() calls initMetrics() which starts the /metrics listener on metricsPort.
    // Don't await: serve() blocks until a signal fires.
    const servePromise = serve(invokePort, () => ({ payload: 'ok', billable_units: 1 }));

    // Poll both ports until they accept connections — more reliable than a fixed sleep
    // on slow CI runners where loopback bind can take longer than a fixed delay.
    async function waitForPort(port: number): Promise<void> {
      for (let i = 0; i < 20; i++) {
        try {
          await new Promise<void>((resolve, reject) => {
            const req = http.request({ hostname: '127.0.0.1', port, method: 'GET', path: '/' },
              (res) => { res.resume(); resolve(); });
            req.on('error', reject);
            req.end();
          });
          return;
        } catch {
          await new Promise<void>((r) => setTimeout(r, 10 * (i + 1)));
        }
      }
      throw new Error(`port ${port} did not become ready in time`);
    }
    await Promise.all([waitForPort(invokePort), waitForPort(metricsPort)]);

    // One /invoke request with Connection: close so the server drains immediately on shutdown.
    await request(invokePort, '/invoke', 'POST', { operation: 'e2e_op' }, { 'Connection': 'close' });

    // Scrape /metrics from the dedicated metrics port.
    const metricsText = await new Promise<string>((resolve, reject) => {
      http.get({ hostname: '127.0.0.1', port: metricsPort, path: '/metrics' }, (res) => {
        const chunks: Buffer[] = [];
        res.on('data', (c: Buffer) => chunks.push(c));
        res.on('end', () => resolve(Buffer.concat(chunks).toString()));
      }).on('error', reject);
    });

    // Assert bounded cardinality: only operation and outcome labels appear.
    assert.ok(metricsText.includes('operation="e2e_op"'), `operation label missing:\n${metricsText}`);
    assert.ok(metricsText.includes('outcome="ok"'), `outcome label missing:\n${metricsText}`);
    for (const forbidden of ['request_id', 'customer_id', 'payload', 'plan', 'metadata']) {
      assert.ok(
        !metricsText.includes(`${forbidden}="`),
        `forbidden label ${forbidden} found in /metrics:\n${metricsText}`,
      );
    }
    // Confirm Prometheus text format TYPE comment is present.
    assert.ok(
      metricsText.includes(`# TYPE ${METRIC_REQUESTS_TOTAL} counter`),
      `TYPE comment missing:\n${metricsText}`,
    );

    // Gracefully stop serve() by emitting the signal. process.emit does NOT send a
    // real OS signal — it only calls listeners registered with process.once('SIGTERM').
    process.emit('SIGTERM');
    await servePromise;
  } finally {
    delete process.env['WORKER_METRICS_PORT'];
  }
});

// billable_units contract: metrics recording does not alter the default (0→1).
test('metrics: billable_units contract unchanged', async () => {
  await withServer(() => ({ payload: 'ok' }), async (port) => {
    // Handler returns no billable_units (undefined) — must default to 1.
    const { data } = await request(port, '/invoke', 'POST', { operation: 'units_test' });
    assert.equal((data as Record<string, number>)['billable_units'], 1);
  });
});
