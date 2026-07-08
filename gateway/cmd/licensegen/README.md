# licensegen

Mints and inspects Crucible **deployment** license keys — the open-core edition
gate that decides whether a gateway runs community, Pro, Business, or Enterprise.
This is unrelated to the billing `plans` table (which tiers the end-customers of
a cloned product). License keys verify offline against a compiled-in Ed25519
public key; no database, network, or Stripe involvement.

## Before you sell licenses — rotate the keypair

The repo ships with a development public key baked into
`gateway/internal/license/DefaultPublicKeyHex`. Its private half was generated
once and **discarded**, so nobody (including the framework author) can mint keys
that a fresh clone will trust — until you generate your own:

```
go run ./cmd/licensegen keygen
```

This prints a private key (32-byte hex seed) and a public key (32-byte hex).

1. Keep the **private** key OFFLINE. It is the only thing that can sign valid
   licenses. Never commit it.
2. Publish the **public** key one of two ways:
   - replace the `DefaultPublicKeyHex` constant in
     `gateway/internal/license/license.go` and rebuild, or
   - set `CRUCIBLE_LICENSE_PUBKEY=<hex>` in the gateway's environment (overrides
     the constant at runtime).

## Sign a license

```
go run ./cmd/licensegen sign \
  --priv <hex-private-key> \
  --licensee "Acme Corp" \
  --email ops@acme.com \
  --edition pro \
  --seats 10 \
  --expires 2027-01-01
```

`--features a,b` overrides the edition defaults; omit it to let the gateway
derive them (pro → operator_tokens, audit_export; business/enterprise → sso,
operator_tokens, audit_export). `--expires` defaults to one year out. Prints a
`cru1.…` key.

## Verify a license

```
go run ./cmd/licensegen verify --pub <hex-public-key> --key cru1.…
```

Pretty-prints the parsed license and exits non-zero if it fails signature,
format, or expiry-past-grace checks. A key that is expired but still inside the
14-day grace window verifies and reports `in_grace: true`.
