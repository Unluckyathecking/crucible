# Release process

Crucible currently has no tagged releases. The project owner named in
[GOVERNANCE.md](../GOVERNANCE.md) is the release manager until more maintainers are
appointed.

For a release:

1. Select a commit on `main` with all required checks passing.
2. Move relevant `CHANGELOG.md` entries from `Unreleased` to a dated version heading.
3. Confirm OpenAPI/client drift, worker conformance, full Go race tests, dashboard
   tests/build, container builds, vulnerability checks, and clone acceptance.
4. Confirm package version metadata and the compatibility matrix agree with the tag.
5. Create an annotated `vMAJOR.MINOR.PATCH` tag and GitHub release using the changelog
   entry as release notes.
6. Publish language packages only when registry ownership, provenance, and release
   automation have been explicitly established. A Git tag does not imply that every
   SDK is available from a public package registry.
7. Start the next `Unreleased` changelog section.

Do not tag directly from an unreviewed feature branch. If a release is withdrawn or
superseded, preserve the tag and document the reason rather than silently replacing it.
