# docker-compose-profiles-rust (pattern C)

The Rust counterpart of
[`../docker-compose-profiles-go`](../docker-compose-profiles-go/): **the app is
in `docker-compose.yml` for local development, but on the server only Phelix runs
it.** The app service sits behind a compose **profile**, so a plain
`docker compose up -d` on the server starts only the backing tier (Redis).

## When to use this

You want one `docker compose up` that runs the whole stack **on your laptop**,
but in production you want Phelix's zero-downtime deploys, versioning and proxy
for the app while compose just keeps the backing services up.

## Local development (compose runs everything)

```bash
cd examples/docker-compose-profiles-rust
docker compose --profile local up --build   # app + redis
curl localhost:8080/redis                    # redis redis:6379 -> +PONG
```

## Server (Phelix runs the app; compose runs only Redis)

```bash
docker compose up -d              # NO --profile → starts ONLY redis (+ the network)
phelix proxy
phelix build profiles-rust-demo   # dockerfile strategy → container on docker-compose-profiles-rust_appnet
curl localhost:8080/redis         # redis redis:6379 -> +PONG
phelix rebuild profiles-rust-demo # subsequent zero-downtime deploys
```

## The ownership rule

The app tier has exactly **one** owner. On the server the app service is gated
behind `profiles: ["local"]`, so `docker compose up -d` never starts it and
Phelix is the sole owner — the ownership-conflict warning stays silent.

## Image build strategy

Default (`dockerfile`): Phelix builds the committed [`Dockerfile`](Dockerfile)
(a `rust:slim` → `debian:bookworm-slim` multi-stage). To build *through* the
compose service's build config instead, see
[`../docker-compose-aware-rust`](../docker-compose-aware-rust/)
(`deploy.docker.build: compose`).

> This mirrors the Go example one-for-one; only the language toolchain differs.
