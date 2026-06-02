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
 * Creates a Node.js HTTP server pre-wired to the worker contract:
 *   POST /invoke  — decodes, calls handler, encodes response.
 *   GET  /healthz — returns 200 OK.
 *
 * Use serve() for the standard lifecycle (signal handling, graceful drain).
 * Use createServer() when you need to manage the server lifecycle yourself.
 */
export function createServer(handler: WorkerHandler): http.Server {
  return http.createServer((req, res) => {
    void dispatch(req, res, handler);
  });
}

/**
 * Runs the worker on the given port and blocks until SIGINT/SIGTERM,
 * then drains in-flight requests for up to 10 s.
 */
export function serve(port: number, handler: WorkerHandler): Promise<void> {
  const server = createServer(handler);

  return new Promise<void>((resolve, reject) => {
    server.listen(port, () => {
      log('info', { port, msg: 'worker listening' });
    });

    server.on('error', reject);

    const shutdown = (): void => {
      log('info', { msg: 'worker shutting down' });
      const timer = setTimeout(resolve, 10_000);
      if (typeof (timer as NodeJS.Timeout).unref === 'function') {
        (timer as NodeJS.Timeout).unref();
      }
      server.close((err) => {
        clearTimeout(timer);
        if (err) reject(err);
        else resolve();
      });
    };

    process.once('SIGINT', shutdown);
    process.once('SIGTERM', shutdown);
  });
}

async function dispatch(
  req: http.IncomingMessage,
  res: http.ServerResponse,
  handler: WorkerHandler,
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

  let raw: string;
  try {
    raw = (await readBody(req, 10 * 1024 * 1024)).toString('utf8');
  } catch {
    writeError(res, { code: 'BAD_REQUEST', message: 'request body too large', retryable: false });
    return;
  }

  let workerReq: Request;
  try {
    workerReq = JSON.parse(raw) as Request;
  } catch {
    writeError(res, { code: 'BAD_REQUEST', message: 'invalid request body', retryable: false });
    return;
  }

  let result: Response;
  try {
    result = await Promise.resolve(handler(workerReq));
  } catch (err) {
    if (err instanceof WorkerError) {
      log('info', {
        request_id: workerReq.request_id,
        operation: workerReq.operation,
        code: err.code,
        msg: 'handler returned structured error',
      });
      writeError(res, { code: err.code, message: err.message, retryable: err.retryable });
      return;
    }
    log('error', {
      request_id: workerReq.request_id,
      operation: workerReq.operation,
      msg: 'handler failed',
    });
    writeError(res, { code: 'INTERNAL', message: 'internal error', retryable: true });
    return;
  }

  if (!result.billable_units || result.billable_units < 1) {
    result = { ...result, billable_units: 1 };
  }

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
