# Multi-stage build → binary in a minimal Debian runtime (Rust links glibc, so
# debian-slim, not scratch/alpine). Used by BOTH `docker compose --profile local
# up` (local dev) and Phelix's default "dockerfile" build strategy on the server.
# No EXPOSE / hardcoded port — the app binds $PORT, injected by Phelix (or compose).
FROM rust:1.85-slim AS build
WORKDIR /src
COPY Cargo.toml ./
COPY src ./src
RUN cargo build --release && cp target/release/profiles-rust-demo /bin/app

FROM debian:bookworm-slim
RUN useradd --user-group --no-create-home --uid 10001 app
USER app
COPY --from=build /bin/app /bin/app
ENTRYPOINT ["/bin/app"]
