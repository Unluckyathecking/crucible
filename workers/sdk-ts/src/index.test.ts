import { test } from 'node:test';
import assert from 'node:assert/strict';
import * as http from 'node:http';
import { createServer, WorkerError } from './index';
import type { WorkerHandler } from './index';

function request(
  port: number,
  path: string,
  method: string,
  body?: unknown,
): Promise<{ status: number; data: unknown }> {
  return new Promise((resolve, reject) => {
    const payload = body !== undefined ? JSON.stringify(body) : undefined;
    const opts: http.RequestOptions = {
      hostname: '127.0.0.1',
      port,
      path,
      method,
      headers: payload
        ? { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(payload) }
        : {},
    };
    const req = http.request(opts, (res) => {
      const chunks: Buffer[] = [];
      res.on('data', (c: Buffer) => chunks.push(c));
      res.on('end', () => {
        resolve({ status: res.statusCode ?? 0, data: JSON.parse(Buffer.concat(chunks).toString()) });
      });
    });
    req.on('error', reject);
    if (payload) req.write(payload);
    req.end();
  });
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
