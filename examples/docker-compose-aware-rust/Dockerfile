# Multi-stage build with a named `prod` target and a RUST_VERSION arg.
# docker-compose's `app` service points its build.target/args here, and Phelix
# (deploy.docker.build: compose) builds THROUGH that compose service — so the
# build config lives in ONE place (compose) and is never duplicated.
ARG RUST_VERSION=1.85
FROM rust:${RUST_VERSION}-slim AS build
WORKDIR /src
COPY Cargo.toml ./
COPY src ./src
RUN cargo build --release && cp target/release/aware-rust-demo /bin/app

# The runtime target compose selects with build.target: prod. No EXPOSE /
# hardcoded port — the app binds $PORT.
FROM debian:bookworm-slim AS prod
RUN useradd --user-group --no-create-home --uid 10001 app
USER app
COPY --from=build /bin/app /bin/app
ENTRYPOINT ["/bin/app"]
