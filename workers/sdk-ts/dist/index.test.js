"use strict";
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
var __importDefault = (this && this.__importDefault) || function (mod) {
    return (mod && mod.__esModule) ? mod : { "default": mod };
};
Object.defineProperty(exports, "__esModule", { value: true });
const node_test_1 = require("node:test");
const strict_1 = __importDefault(require("node:assert/strict"));
const http = __importStar(require("node:http"));
const index_1 = require("./index");
function request(port, path, method, body) {
    return new Promise((resolve, reject) => {
        const payload = body !== undefined ? JSON.stringify(body) : undefined;
        const opts = {
            hostname: '127.0.0.1',
            port,
            path,
            method,
            headers: payload
                ? { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(payload) }
                : {},
        };
        const req = http.request(opts, (res) => {
            const chunks = [];
            res.on('data', (c) => chunks.push(c));
            res.on('end', () => {
                resolve({ status: res.statusCode ?? 0, data: JSON.parse(Buffer.concat(chunks).toString()) });
            });
        });
        req.on('error', reject);
        if (payload)
            req.write(payload);
        req.end();
    });
}
async function withServer(handler, fn) {
    const server = (0, index_1.createServer)(handler);
    await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
    const port = server.address().port;
    try {
        await fn(port);
    }
    finally {
        await new Promise((resolve, reject) => server.close((err) => (err ? reject(err) : resolve())));
    }
}
(0, node_test_1.test)('GET /healthz returns 200 with status ok', async () => {
    await withServer(() => ({ payload: {} }), async (port) => {
        const { status, data } = await request(port, '/healthz', 'GET');
        strict_1.default.equal(status, 200);
        strict_1.default.deepEqual(data, { status: 'ok' });
    });
});
(0, node_test_1.test)('POST /invoke returns payload and billable_units', async () => {
    const handler = (req) => ({
        payload: { echo: req.payload },
        billable_units: 2,
    });
    await withServer(handler, async (port) => {
        const { status, data } = await request(port, '/invoke', 'POST', {
            operation: 'echo',
            payload: { x: 1 },
        });
        strict_1.default.equal(status, 200);
        strict_1.default.deepEqual(data, { payload: { echo: { x: 1 } }, billable_units: 2 });
    });
});
(0, node_test_1.test)('billable_units defaults to 1 when absent', async () => {
    await withServer(() => ({ payload: 'ok' }), async (port) => {
        const { data } = await request(port, '/invoke', 'POST', { operation: 'test' });
        strict_1.default.equal(data.billable_units, 1);
    });
});
(0, node_test_1.test)('WorkerError surfaces as structured error envelope', async () => {
    const handler = () => {
        throw new index_1.WorkerError('NOT_FOUND', 'thing not found', false);
    };
    await withServer(handler, async (port) => {
        const { status, data } = await request(port, '/invoke', 'POST', { operation: 'test' });
        strict_1.default.equal(status, 200);
        strict_1.default.deepEqual(data, {
            error: { code: 'NOT_FOUND', message: 'thing not found', retryable: false },
        });
    });
});
(0, node_test_1.test)('plain Error becomes generic INTERNAL error', async () => {
    const handler = () => {
        throw new Error('secret internal detail');
    };
    await withServer(handler, async (port) => {
        const { status, data } = await request(port, '/invoke', 'POST', { operation: 'test' });
        strict_1.default.equal(status, 200);
        strict_1.default.deepEqual(data, {
            error: { code: 'INTERNAL', message: 'internal error', retryable: true },
        });
    });
});
