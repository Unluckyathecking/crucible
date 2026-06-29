// Fixture-driven conformance tests for the Crucible TypeScript SDK.
//
// Loads workers/conformance/fixture.json (the language-neutral spec) and asserts
// each case against an in-process server built from the compiled TS SDK.
// Run after building the SDK:
//
//   cd workers/sdk-ts && npm ci && npm run build
//   node --test conformance/conformance.test.js

'use strict';

const { test } = require('node:test');
const assert = require('node:assert/strict');
const http = require('node:http');
const path = require('node:path');
const fs = require('node:fs');

// Import the compiled SDK — requires npm run build to have run first.
const { createServer, WorkerError } = require('../dist/index.js');

// ── Fixture loading ───────────────────────────────────────────────────────────

const fixturePath = path.join(__dirname, '..', '..', 'conformance', 'fixture.json');
const fixture = JSON.parse(fs.readFileSync(fixturePath, 'utf8'));

if (!fixture.cases || fixture.cases.length === 0) {
  throw new Error(`shared fixture loaded zero cases; check ${fixturePath}`);
}

// ── HTTP test helpers ─────────────────────────────────────────────────────────

/**
 * Start an http.Server, run fn(port), then close the server.
 * Uses port 0 so the OS picks a free port.
 */
function withServer(handler, fn) {
  return new Promise((resolve, reject) => {
    const srv = createServer(handler, {});
    srv.listen(0, '127.0.0.1', async () => {
      const { port } = srv.address();
      try {
        await fn(port);
        resolve();
      } catch (err) {
        reject(err);
      } finally {
        srv.close();
      }
    });
    srv.on('error', reject);
  });
}

/**
 * Send a raw HTTP request and return { status, headers, body } where body is
 * a parsed JSON value (or the raw string if JSON.parse fails).
 */
function rawRequest(port, path_, method, bodyStr, extraHeaders) {
  return new Promise((resolve, reject) => {
    const headers = { ...extraHeaders };
    if (bodyStr !== undefined) {
      headers['Content-Type'] = 'application/json';
      headers['Content-Length'] = Buffer.byteLength(bodyStr);
    }
    const req = http.request(
      { hostname: '127.0.0.1', port, path: path_, method, headers },
      (res) => {
        const chunks = [];
        res.on('data', (c) => chunks.push(c));
        res.on('end', () => {
          const raw = Buffer.concat(chunks).toString('utf8');
          let data;
          try {
            data = JSON.parse(raw);
          } catch {
            data = raw;
          }
          resolve({ status: res.statusCode, headers: res.headers, body: data });
        });
      },
    );
    req.on('error', reject);
    if (bodyStr !== undefined) req.write(bodyStr);
    req.end();
  });
}

// ── Per-case assertion helpers ────────────────────────────────────────────────

async function assertHealthzBody(port) {
  const { status, headers, body } = await rawRequest(port, '/healthz', 'GET');
  assert.equal(status, 200, 'healthz must return HTTP 200');
  const ct = headers['content-type'] || '';
  assert.ok(ct.startsWith('application/json'), `healthz Content-Type must be application/json, got ${ct}`);
  assert.deepEqual(body, { status: 'ok' }, 'healthz body must be exactly {"status":"ok"}');
}

async function assertMethodNotAllowed(port, expectedStatus) {
  for (const method of ['GET', 'HEAD', 'PUT', 'DELETE', 'PATCH', 'OPTIONS']) {
    const { status } = await rawRequest(port, '/invoke', method);
    assert.equal(
      status,
      expectedStatus,
      `${method} /invoke must return ${expectedStatus}`,
    );
  }
}

async function assertBillableUnitsFloor() {
  // Build a server with a handler that returns 0 units to exercise normalisation.
  await withServer(() => ({ payload: { floor: 'ok' }, billable_units: 0 }), async (port) => {
    const { status, body } = await rawRequest(port, '/invoke', 'POST',
      JSON.stringify({ operation: 'floor_test', payload: {} }));
    assert.equal(status, 200);
    const units = body && body.billable_units;
    assert.ok(units >= 1, `billable_units must be >= 1 after SDK normalisation, got ${units}`);
  });
}

async function assertApiErrorEnvelope() {
  const ERROR_CODE = 'FIXTURE_TEST_ERROR';
  await withServer(() => { throw new WorkerError(ERROR_CODE, 'fixture-driven error test', true); }, async (port) => {
    const { status, body } = await rawRequest(port, '/invoke', 'POST',
      JSON.stringify({ operation: 'err_test', payload: {} }));
    assert.equal(status, 200, 'error envelopes must return HTTP 200');
    assert.ok(body.error, 'error field must be present');
    assert.equal(body.error.code, ERROR_CODE, 'error.code must match');
    assert.ok(body.error.message, 'error.message must be non-empty');
    assert.ok(body.error.retryable !== undefined, 'error.retryable must be present');
    assert.ok(!body.payload, 'error envelope must not contain payload');
    assert.ok(!('billable_units' in body), 'error envelope must not contain billable_units');
  });
}

async function assertEmptyBodyBadRequest(port) {
  // Empty body is not valid JSON; the SDK must return BAD_REQUEST.
  const { status, body } = await rawRequest(port, '/invoke', 'POST', '');
  assert.equal(status, 200, 'error envelopes always return HTTP 200');
  assert.equal(body && body.error && body.error.code, 'BAD_REQUEST',
    `empty body must yield BAD_REQUEST error envelope, got ${JSON.stringify(body)}`);
}

// ── Test runner ───────────────────────────────────────────────────────────────

for (const tc of fixture.cases) {
  // Check for a known TS divergence.
  const tsDivergence = tc.known_divergences && tc.known_divergences.ts;

  test(tc.id, async () => {
    if (tsDivergence) {
      // Use the diverged expected status rather than the canonical one.
      console.log(`  known TS divergence: ${tsDivergence.note}`);
    }

    // Build a reusable echo server for cases that need a healthy worker.
    await withServer(() => ({ payload: { ok: true }, billable_units: 1 }), async (port) => {
      switch (tc.kind) {
        case 'healthz_body':
          await assertHealthzBody(port);
          break;

        case 'method_not_allowed': {
          // Use diverged status for TS if documented; canonical is 405.
          const expectedStatus = tsDivergence ? tsDivergence.status : 405;
          await assertMethodNotAllowed(port, expectedStatus);
          break;
        }

        case 'billable_units_floor':
          await assertBillableUnitsFloor();
          break;

        case 'apierror_envelope':
          await assertApiErrorEnvelope();
          break;

        case 'empty_body_bad_request':
          await assertEmptyBodyBadRequest(port);
          break;

        default:
          throw new Error(
            `unknown fixture case kind ${JSON.stringify(tc.kind)} (id=${tc.id}): update conformance.test.js`,
          );
      }
    });
  });
}
