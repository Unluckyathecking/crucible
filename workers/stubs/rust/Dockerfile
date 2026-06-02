# Crucible Rust worker stub.
# Build context MUST be the repo root so the sdk-rust path dependency resolves.
#   docker build -f workers/stubs/rust/Dockerfile .
FROM rust:1.87-slim AS build
WORKDIR /src
COPY workers/sdk-rust ./workers/sdk-rust
COPY workers/stubs/rust ./workers/stubs/rust
WORKDIR /src/workers/stubs/rust
RUN cargo build --release

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd -r -g 10001 cru \
    && useradd --no-log-init -u 10001 -g cru -r cru
COPY --from=build /src/workers/stubs/rust/target/release/crucible-stub-rust /usr/local/bin/worker
USER cru
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/worker"]
