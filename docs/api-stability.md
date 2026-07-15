# API stability and deprecation

Crucible has not yet published a stable tagged release. The following rules make the
current compatibility boundaries explicit without claiming 1.0 stability.

## Compatibility surfaces

The public surfaces are the gateway's served OpenAPI document, generated consumer
client APIs, worker HTTP/JSON request and response shapes, exported worker SDK APIs,
environment variables documented in `.env.example`, and forward database migrations.
Internal Go packages, dashboard internals, scripts not documented as public, and
`docs-internal/` are not compatibility commitments.

## Before 1.0

Breaking changes may occur on `main`, but they must be intentional, tested, documented
in `CHANGELOG.md`, and accompanied by migration guidance. Consumers should pin an
exact commit. Worker-contract changes require coordinated updates to every SDK, stub,
fixture, and conformance job in the same pull request.

## Tagged releases

Tagged releases will use Semantic Versioning. Once a 1.0 release exists:

- incompatible public-surface changes require a major release;
- backwards-compatible features require a minor release;
- backwards-compatible fixes require a patch release; and
- security fixes follow the smallest safe release increment.

## Deprecation

When practical, a public feature will be marked deprecated in documentation and code,
an alternative will be provided, and the old behavior will remain through at least
one subsequent minor release before removal in a major release. Urgent security or
data-integrity fixes may require faster removal and will be called out prominently.

Database migrations are forward-only. Published migrations must not be rewritten;
compatibility fixes use a new idempotent migration.
