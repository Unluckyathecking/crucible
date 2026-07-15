# Changelog

All notable changes to Crucible will be documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
tagged releases will follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
as described in [docs/releases.md](docs/releases.md).

## [Unreleased]

### Added

- Open-source contribution, security, support, conduct, governance, compatibility,
  API stability, and release policies.
- Structured issue forms, pull request guidance, dependency update configuration,
  and SDK compatibility CI.

### Changed

- Updated the canonical Go toolchain to 1.25.12 for the standard-library fix for
  GO-2026-5856.
- Made clone acceptance hydrate the active Go worker's standalone module graph and
  serialized integration-test packages while they share a migration database.

[Unreleased]: https://github.com/Unluckyathecking/crucible/compare/main...HEAD
