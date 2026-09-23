# docker-go

A minimal Go app that Phelix runs **as a container** using the
[Docker runtime](../../docs/guides/docker-runtime.md). Phelix builds an image for
the app and runs each instance as a container under the zero-downtime proxy —
one Phelix agent on the host manages many app containers as **one** server.

> Demonstrates the mechanism; not production-ready.

## What it demonstrates

- `deploy.runtime: docker` with a required zero-downtime strategy
  (`blue-green`) — see [Docker runtime](../../docs/guides/docker-runtime.md).
- The [PORT contract](../../docs/getting-started/the-port-contract.md) inside a
  container: Phelix injects `PORT` and publishes the container port to a private
  `127.0.0.1` host port; the Dockerfile needs no `EXPOSE` or hardcoded port.
- A `/health` endpoint that gates the blue-green switch.
- The `Dockerfile` and `.dockerignore` here are exactly what **Phelix
  generates** for a Go project (captured with `phelix dockerize`): a multi-stage
  build producing a static binary in a `scratch` runtime, with the CA bundle
  copied in and a non-root numeric UID. Phelix uses a project's own Dockerfile
  as-is, so this committed copy is what builds — delete it to let Phelix
  regenerate on the first deploy.

## Requirements

- Docker installed and reachable on the host (Phelix talks to the local Docker
  daemon).
- The `phelix proxy` running (rebuild auto-starts it).

## Deploy with Phelix

```bash
cd examples/docker-go

phelix proxy                     # zero-downtime proxy (build auto-starts it too)
phelix build docker-go-demo      # builds docker-go-demo:v1, runs it as a container
phelix status docker-go-demo     # STATUS running; Deploy blue-green; Proxy on
```

Ship a change with zero downtime, then roll back:

```bash
phelix rebuild docker-go-demo    # builds :v2, health-checks it, switches the proxy
phelix rollback docker-go-demo   # back to the previous image, zero downtime
```

Only Docker is required on the host — the image is built with the toolchain
inside the container, not a host Go toolchain.

## Run the container yourself (optional sanity check)

```bash
docker build -t docker-go-demo:local .
docker run --rm -e PORT=8080 -p 127.0.0.1:8080:8080 docker-go-demo:local
curl localhost:8080/health       # ok
```

## Build an image without running it

If you only want an image (not the runtime), use `phelix dockerize` instead:

```bash
phelix dockerize docker-go-demo --tag v1.0.0
```

See [Docker image building](../../docs/guides/docker-images.md).

## Further reading

- [Docker runtime](../../docs/guides/docker-runtime.md)
- [Docker image building](../../docs/guides/docker-images.md)
- [Zero-downtime deployments](../../docs/guides/zero-downtime-deployments.md)
- [Configuration](../../docs/reference/configuration.md)
- [Troubleshooting](../../docs/guides/troubleshooting.md)
