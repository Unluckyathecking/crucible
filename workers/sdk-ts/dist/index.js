"use strict";
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
var __createBinding = (this && this.__createBinding) || (Object.create ? (function(o, m, k, k2) {
    if (k2 === undefined) k2 = k;
    var desc = Object.getOwnPropertyDescriptor(m, k);
    if (!desc || ("get" in desc ? !m.__esModule : desc.writable || desc.configurable)) {
      desc = { enumerable: true, get: function() { return m[k]; } };
    }
    Object.defineProperty(o, k2, desc);
}) : (function(o, m, k, k2) {
    if (k2 === undefined) k2 = k;
    o[k2] = m[k];
}));
var __setModuleDefault = (this && this.__setModuleDefault) || (Object.create ? (function(o, v) {
    Object.defineProperty(o, "default", { enumerable: true, value: v });
}) : function(o, v) {
    o["default"] = v;
});
var __importStar = (this && this.__importStar) || (function () {
    var ownKeys = function(o) {
        ownKeys = Object.getOwnPropertyNames || function (o) {
            var ar = [];
            for (var k in o) if (Object.prototype.hasOwnProperty.call(o, k)) ar[ar.length] = k;
            return ar;
        };
        return ownKeys(o);
    };
    return function (mod) {
        if (mod && mod.__esModule) return mod;
        var result = {};
        if (mod != null) for (var k = ownKeys(mod), i = 0; i < k.length; i++) if (k[i] !== "default") __createBinding(result, mod, k[i]);
        __setModuleDefault(result, mod);
        return result;
    };
})();
Object.defineProperty(exports, "__esModule", { value: true });
exports.WorkerError = void 0;
exports.createServer = createServer;
exports.serve = serve;
const http = __importStar(require("http"));
/**
 * Structured error a handler can throw to surface a stable code to the caller.
 * A plain Error is also accepted — the SDK wraps it as a generic INTERNAL error
 * (the real cause is logged but never surfaced in the response).
 */
class WorkerError extends Error {
    code;
    retryable;
    constructor(code, message, retryable = false) {
        super(message);
        this.name = 'WorkerError';
        this.code = code;
        this.retryable = retryable;
    }
}
exports.WorkerError = WorkerError;
/**
 * Creates a Node.js HTTP server pre-wired to the worker contract:
 *   POST /invoke  — decodes, calls handler, encodes response.
 *   GET  /healthz — returns 200 OK.
 *
 * Use serve() for the standard lifecycle (signal handling, graceful drain).
 * Use createServer() when you need to manage the server lifecycle yourself.
 */
function createServer(handler) {
    return http.createServer((req, res) => {
        void dispatch(req, res, handler);
    });
}
/**
 * Runs the worker on the given port and blocks until SIGINT/SIGTERM,
 * then drains in-flight requests for up to 10 s.
 */
function serve(port, handler) {
    const server = createServer(handler);
    return new Promise((resolve, reject) => {
        server.listen(port, () => {
            log('info', { port, msg: 'worker listening' });
        });
        server.on('error', reject);
        const shutdown = () => {
            log('info', { msg: 'worker shutting down' });
            const timer = setTimeout(resolve, 10_000);
            if (typeof timer.unref === 'function') {
                timer.unref();
            }
            server.close((err) => {
                clearTimeout(timer);
                if (err)
                    reject(err);
                else
                    resolve();
            });
        };
        process.once('SIGINT', shutdown);
        process.once('SIGTERM', shutdown);
    });
}
async function dispatch(req, res, handler) {
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
    let raw;
    try {
        raw = (await readBody(req, 10 * 1024 * 1024)).toString('utf8');
    }
    catch {
        writeError(res, { code: 'BAD_REQUEST', message: 'request body too large', retryable: false });
        return;
    }
    let workerReq;
    try {
        workerReq = JSON.parse(raw);
    }
    catch {
        writeError(res, { code: 'BAD_REQUEST', message: 'invalid request body', retryable: false });
        return;
    }
    let result;
    try {
        result = await Promise.resolve(handler(workerReq));
    }
    catch (err) {
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
function readBody(req, maxBytes) {
    return new Promise((resolve, reject) => {
        const chunks = [];
        let size = 0;
        req.on('data', (chunk) => {
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
function writeError(res, err) {
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ error: err }));
}
function log(level, data) {
    process.stdout.write(JSON.stringify({ level, time: Date.now(), ...data }) + '\n');
}
