# docker-rust

A minimal Rust app that Phelix runs **as a container** using the
[Docker runtime](../../docs/guides/docker-runtime.md). Like
[docker-go](../docker-go/), Phelix builds an image and runs each instance as a
container under the zero-downtime proxy — one Phelix agent on the host manages
many app containers as **one** server.

Standard library only (serves HTTP over a `TcpListener`); a real service would
use an HTTP framework. Demonstrates the mechanism; not production-ready.

## What it demonstrates

- `deploy.runtime: docker` with a required zero-downtime strategy
  (`blue-green`) — see [Docker runtime](../../docs/guides/docker-runtime.md).
- The [PORT contract](../../docs/getting-started/the-port-contract.md) inside a
  container: Phelix injects `PORT` and publishes the container port to a private
  `127.0.0.1` host port; the Dockerfile needs no `EXPOSE`.
- A `/health` endpoint that gates the blue-green switch.
- The `Dockerfile` and `.dockerignore` here are exactly what **Phelix
  generates** for a Rust project (captured with `phelix dockerize`): a
  cache-mounted `cargo build --release` in `rust:${RUST_VERSION}-slim`, with the
  binary copied into a `debian:bookworm-slim` runtime carrying
  `ca-certificates`/`libssl3` and running as a non-root user. Phelix uses a
  project's own Dockerfile as-is — delete it to let Phelix regenerate on the
  first deploy.

## Requirements

- Docker installed and reachable on the host.
- The `phelix proxy` running (rebuild auto-starts it).

## Deploy with Phelix

Phelix auto-detects Rust from `Cargo.toml`.

```bash
cd examples/docker-rust

phelix proxy                       # zero-downtime proxy (build auto-starts it too)
phelix build docker-rust-demo      # builds docker-rust-demo:v1, runs it as a container
phelix status docker-rust-demo     # STATUS running; Deploy blue-green; Proxy on
```

Ship a change with zero downtime, then roll back:

```bash
phelix rebuild docker-rust-demo    # builds :v2, health-checks it, switches the proxy
phelix rollback docker-rust-demo   # back to the previous image, zero downtime
```

Only Docker is required on the host — the image is built with the toolchain
inside the container, not a host Rust toolchain.

## Run the container yourself (optional sanity check)

```bash
docker build -t docker-rust-demo:local .
docker run --rm -e PORT=8080 -p 127.0.0.1:8080:8080 docker-rust-demo:local
curl localhost:8080/health         # ok
```

## Build an image without running it

```bash
phelix dockerize docker-rust-demo --tag v1.0.0
```

See [Docker image building](../../docs/guides/docker-images.md).

## Further reading

- [Docker runtime](../../docs/guides/docker-runtime.md)
- [Docker image building](../../docs/guides/docker-images.md)
- [Zero-downtime deployments](../../docs/guides/zero-downtime-deployments.md)
- [Configuration](../../docs/reference/configuration.md)
- [Troubleshooting](../../docs/guides/troubleshooting.md)
