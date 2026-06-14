import { test } from 'node:test';
import assert from 'node:assert/strict';
import * as crypto from 'node:crypto';
import * as http from 'node:http';
import { createServer, WorkerError } from './index';
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
