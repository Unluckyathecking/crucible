# 10xworker:job — route-response-schema (type the /v1 invoke success body end-to-end into generated clients)

```json
{
  "module": "route-response-schema",
  "scope": [
    "gateway/internal/openapi/openapi.go",
    "gateway/internal/openapi/openapi_test.go",
    "gateway/internal/openapi/codegen_test.go",
    "gateway/internal/server/routes_table.go",
    "gateway/test/textkit/routes.go",
    "gateway/test/textkit/textkit_test.go"
  ],
  "input": "The frozen /v1 invoke success envelope (proxy.InvokeResponse = {payload, billable_units, units_label}, gateway/internal/proxy/client.go) plus the existing RouteDescriptor.RequestSchema/SampleRequest primitives from #170.",
  "output": "A new optional ResponseSchema *Schema field on openapi.RouteDescriptor (symmetric complement to #170's request-side field) that invokeOperation wraps into the documented 200 body, replacing today's untyped {\"type\":\"object\"}, so scripts/gen-clients.sh emits typed *Response structs for both the Go and TypeScript clients.",
  "acceptance": [
    "openapi.RouteDescriptor gains an optional ResponseSchema *Schema field; its nil zero-value is backward-compatible — a route that declares none (e.g. /echo in routes_table.go) still emits the current generic {\"type\":\"object\"} 200 body, asserted by an existing/added openapi_test case.",
    "When set, invokeOperation emits the 200 application/json schema as an object whose properties are {payload: <ResponseSchema>, billable_units: {type:\"integer\"}, units_label: {type:\"string\"}}, matching the proxy.InvokeResponse json tags exactly.",
    "All three gateway/test/textkit/routes.go routes declare a ResponseSchema (count-words->{words:int}, transform->{text:string}, slugify->{slug:string}) mirroring the handler's response structs (workers/stubs/textkit/handler/handler.go), and textkit_test.go asserts the emitted 200 properties for each.",
    "A codegen_test.go assertion exercises the go_response_type path in scripts/gen-clients.sh: a route declaring ResponseSchema yields a named *Response struct rather than the `any` fallback that empty properties produces today.",
    "go test -race ./... is green in gateway/; pre-existing openapi 200-response tests for undeclared routes pass unchanged, and error responses still use $ref (errorSchemaRef) with no 4xx/5xx doc regression."
  ],
  "forbidden": [
    "Do NOT add response validation at the invoke trust boundary (server/routes.go::invoke) — this is OpenAPI documentation + codegen only; billable_units < 1 enforcement and the 502 WORKER_BAD_RESPONSE path (invariant 2) stay byte-for-byte unchanged.",
    "Do NOT touch gateway/proto/tool.proto or add per-product proto fields (invariant 1); ResponseSchema lives only in the openapi descriptor.",
    "Do NOT alter existing RequestSchema/SampleRequest semantics from #170 — this is a purely additive sibling field.",
    "Do NOT register textkit's routes into the shipped V1Routes (routes_table.go) — textkit stays a harness-swapped test table."
  ]
}
```

#170 gave routes a declared *request* schema, but the *response* side is still a generic
`{"type":"object"}` (`openapi.go`), so every `/v1` invoke route degrades to `any` return types in
both `clients/go` and `clients/typescript` — the client generator already reads the `200` schema's
`properties` to build typed structs (`scripts/gen-clients.sh`). Declaring `ResponseSchema` closes that
loop for every clone with one composable, additive field, and the just-merged textkit worker (#172)
already returns three concrete, differently-shaped response structs that give the primitive real call
sites to prove against (satisfying the repo's "three call sites earn the helper" bar rather than a
speculative one). The module is entirely inside `gateway/internal/openapi` + `gateway/test/textkit`,
byte-disjoint from open PRs #167 (license/EE/dashboard) and #168 (CI), and touches none of
invariants 1–9.

---
_Seeded by the cross-repo sprint planner._
