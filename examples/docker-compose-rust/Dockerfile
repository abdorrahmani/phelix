# syntax=docker/dockerfile:1
# Stage 1: Build
ARG RUST_VERSION=1.85
FROM rust:${RUST_VERSION}-slim AS builder

ARG TARGETPLATFORM

WORKDIR /app

# Copy the whole project (Cargo.toml, Cargo.lock, src, ...)
COPY . .

# Build under cache mounts; copy the binary out (mounts aren't layers)
RUN --mount=type=cache,target=/usr/local/cargo/registry,id=phelix-cargo-registry \
    --mount=type=cache,target=/app/target,id=phelix-rust-target-${TARGETPLATFORM},sharing=locked \
    cargo build --release && \
    cp target/release/compose-rust-demo /app/server

# Stage 2: Runtime (minimal image)
FROM debian:bookworm-slim AS runtime

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates libssl3 && rm -rf /var/lib/apt/lists/*

# Run as a non-root user
RUN useradd --user-group --no-create-home --uid 65532 app
COPY --from=builder /app/server /server
USER app

ENTRYPOINT ["/server"]
