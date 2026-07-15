# Compatibility matrix

This matrix distinguishes declared minimums from the canonical versions used by the
full CI and deployment examples. "Tested" means exercised by GitHub Actions; it does
not promise compatibility outside the listed surface.

## Runtime and toolchains

| Component | Declared or supported | Tested in CI | Notes |
|---|---|---|---|
| Gateway and Go worker SDKs | Go 1.25.12 toolchain | Go 1.25.12 on Ubuntu | Go modules declare 1.25 and pin toolchain 1.25.12. |
| Go consumer client | Go 1.22+ | Go 1.22 on Ubuntu; Go 1.25.12 on Ubuntu and macOS | Go 1.22 binaries are not run on current macOS runners because that legacy toolchain cannot emit binaries accepted by the current macOS loader. |
| Dashboard | Node.js 22, pnpm 10 | Node.js 22 on Ubuntu | Next.js 15 production build is a required CI gate. |
| TypeScript SDKs/clients | Node.js 18+ | Node.js 18 and 22 on Ubuntu and macOS | Generated client declares Node 18 minimum. |
| Python SDKs/clients | Python 3.9+ | Python 3.9 and 3.12 on Ubuntu and macOS | Runtime code is standard-library only. |
| Rust worker SDK | Current stable Rust | Stable Rust on Ubuntu and macOS | No minimum supported Rust version is promised yet. |
| PostgreSQL | PostgreSQL 16 | PostgreSQL 16 on Ubuntu | Migrations are applied in lexical order. |
| Redis | Redis 7 | Redis 7 Alpine on Ubuntu | Used for hot cache, limits, quotas, and jobs. |
| Containers | Docker with Compose v2 | Docker on Ubuntu | Example images target Linux containers. |

## Platforms

Linux x86_64 is the supported production platform for the gateway, dashboard, and
example deployment assets. Ubuntu and macOS are tested for the portable client and
worker SDK surfaces. Windows is not currently tested; WSL2 with Docker may work but is
community-supported.

The repository does not promise support for other CPU architectures. Please include
architecture details in compatibility reports.

## Compatibility policy

- `main` is the only maintained line until the first tagged release.
- Generated clients are checked for drift against the gateway's served OpenAPI.
- Worker SDKs must pass the shared fixture-driven contract tests.
- A platform or version should not be advertised as supported until CI exercises it.
- Dependency support follows upstream security support where practical. A dependency
  update may raise a minimum version before 1.0; the change must be called out in the
  changelog.

See [API stability](api-stability.md) for upgrade and deprecation rules.
