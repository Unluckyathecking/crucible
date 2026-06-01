"use strict";
// Hello-world Crucible worker stub (TypeScript/Node.js).
//
// Every TS worker in a Crucible clone starts from this shape: import the SDK,
// implement one function, call serve. Per-product logic lives entirely in the
// handler body.
//
// Run locally (TS stub):
//   cd workers/stubs/ts && npm install && npm run build && npm start
//
// Or from repo root:
//   cd workers/sdk-ts && npm install && npm run build
//   cd workers/stubs/ts && npm install && npm run build && node dist/index.js
//
// Smoke test:
//   curl -X POST localhost:8081/invoke \
//     -H 'content-type: application/json' \
//     -d '{"operation":"echo","payload":{"x":"hi"},"metadata":{"units":"3"}}'
Object.defineProperty(exports, "__esModule", { value: true });
const worker_sdk_ts_1 = require("@crucible/worker-sdk-ts");
// handle echoes the request payload back. If metadata["units"] is set to a positive
// integer, it returns that as billable_units — useful for testing per-unit billing end-to-end.
const handler = (req) => {
    let units = 1;
    const raw = req.metadata['units'];
    if (raw !== undefined) {
        const n = parseInt(raw, 10);
        if (Number.isFinite(n) && n >= 1)
            units = n;
    }
    return {
        payload: { echo: req.payload, operation: req.operation },
        billable_units: units,
    };
};
const port = (() => {
    const raw = process.env['PORT'];
    if (raw === undefined)
        return 8081;
    const n = parseInt(raw, 10);
    return Number.isFinite(n) && n > 0 ? n : 8081;
})();
(0, worker_sdk_ts_1.serve)(port, handler).catch((err) => {
    process.stderr.write(String(err) + '\n');
    process.exit(1);
});
