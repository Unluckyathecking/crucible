# Work order — `channelsig`: unified HMAC signed-channel primitive

**Lane:** `10xworker:job` (primary decomposition)
**Area:** architecture / gateway internal
**Date:** 2026-07-04

## Spec

```json
{
  "module": "channelsig",
  "scope": [
    "gateway/internal/channelsig/channelsig.go",
    "gateway/internal/channelsig/channelsig_test.go",
    "gateway/internal/channelsig/middleware.go",
    "gateway/internal/channelsig/middleware_test.go",
    "gateway/internal/webhookout/emitter.go",
    "gateway/internal/proxy/client.go"
  ],
  "input": "the two byte-identical inline HMAC signers in webhookout.Sign (emitter.go) and proxy/client.go, plus the 5-minute skew window the SDK verifiers already enforce",
  "output": "one shared channelsig package exposing Sign(secret, ts, body) and Verify(secret, header, body, now, window) built on hmac.Equal; webhookout and proxy consume Sign so wire output is unchanged; a reusable inbound-verification http middleware for future signed channels",
  "acceptance": [
    "channelsig.Sign produces byte-identical output to the pre-existing webhookout.Sign and to the current proxy/client.go X-Worker-Signature value for a table of golden (secret, ts, body) vectors (test asserts equality against literal expected hex)",
    "all pre-existing webhookout and proxy tests pass unchanged; the emitted X-Crucible-Signature and X-Worker-Signature header formats (t=<unix>,v1=<hex>) are identical to before (no wire-format change)",
    "channelsig.Verify uses subtle/hmac constant-time compare (hmac.Equal) and rejects timestamps outside a +/-5-minute window (stale and future-skew cases both covered)",
    "a cross-language parity vector test: channelsig.Verify accepts a signature string produced by the Go SDK signing path (workers/sdk-go) and rejects a tampered-body variant",
    "go test -race ./... green in gateway/"
  ],
  "forbidden": [
    "do not change the wire format of X-Crucible-Signature or X-Worker-Signature, or the \"ts.body\" (ts + \".\" + body) payload construction — the outbound-webhook signature is a frozen customer-facing contract and worker-channel parity is a cross-SDK invariant",
    "do not fold Stripe's inbound verifier (billing/webhook.go verifySignature) into this package — it implements Stripe's externally-defined scheme and stays independent",
    "do not change the opt-in / zero-config-safe default: an empty secret still emits no signature header (today's behavior)",
    "no changes to gateway/proto/tool.proto (frozen)",
    "do not touch the webhook delivery pipeline semantics (outbox/emitter delivery, egress guard, dead-letter, subscriptions) — only the signing helper it calls"
  ]
}
```

## Rationale

The scheme `HMAC-SHA256(secret, ts + "." + body)` rendered as `t=<unix>,v1=<hex>` is
hand-implemented twice inside the gateway with no shared code: `webhookout.Sign`
(`gateway/internal/webhookout/emitter.go`, header `X-Crucible-Signature`) and inline in
`gateway/internal/proxy/client.go` (header `X-Worker-Signature`, whose own comment says
it "mirrors webhookout.Sign"). The verify side is independently re-implemented in all
three worker SDKs. There is no canonical Go `Sign`/`Verify` the gateway packages share
and no reusable inbound-verification middleware, so the next signed integration copies
the scheme a fifth time.

This module consolidates the duplicated signer into one composable primitive and adds the
missing reusable `Verify`/middleware side, without altering any wire format. Because
crucible is the clone-and-adapt template, the primitive propagates to every future clone
and every future signed channel. Disjoint from the webhook delivery pipeline (outbox
#140, egress guard #141, replay #142, subscriptions #146, endpoint API #149, deliveries
#150): it touches only the signing helpers those paths call, plus two call-site swaps.
