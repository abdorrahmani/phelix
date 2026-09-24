FROM rust:1.85-slim AS build
WORKDIR /src
COPY Cargo.toml ./
COPY src ./src
RUN cargo build --release && cp target/release/worker-rust-demo /bin/app

FROM debian:bookworm-slim
RUN useradd --user-group --no-create-home --uid 10001 app
USER app
COPY --from=build /bin/app /bin/app
ENTRYPOINT ["/bin/app"]
