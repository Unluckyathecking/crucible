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
export declare class WorkerError extends Error {
    readonly code: string;
    readonly retryable: boolean;
    constructor(code: string, message: string, retryable?: boolean);
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
export declare function createServer(handler: WorkerHandler): http.Server;
/**
 * Runs the worker on the given port and blocks until SIGINT/SIGTERM,
 * then drains in-flight requests for up to 10 s.
 */
export declare function serve(port: number, handler: WorkerHandler): Promise<void>;
