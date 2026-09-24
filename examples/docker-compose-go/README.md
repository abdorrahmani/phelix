# docker-compose-go

A minimal Go app that Phelix runs **as a container on a shared Docker network**,
beside a backing service (Redis) owned by **docker-compose** — the
**split-ownership** model of the
[Docker runtime](../../docs/guides/docker-runtime.md).

- **Phelix owns the app tier:** it builds `compose-go-demo:vN` and
  zero-downtime-deploys each instance as a container (`phelix build` /
  `phelix rebuild`).
- **You own the backing tier:** Redis runs under `docker compose`, on a stable
  named network with (for a real datastore) a volume.

They are joined by one key — `deploy.network` in [`phelix.yaml`](phelix.yaml) —
which attaches the app container to the compose network. The app then reaches
Redis by its **service name**, `redis:6379`.

> For the plain Docker runtime with no backing services, see
> [`../docker-go`](../docker-go/). Demonstrates the mechanism; not
> production-ready.

## What it demonstrates

- `deploy.runtime: docker` + `deploy.network: phelix-compose-go` — the app
  container joins the compose network and resolves `redis` by DNS.
- The [PORT contract](../../docs/getting-started/the-port-contract.md) in a
  container: Phelix injects `PORT` and publishes it to a private `127.0.0.1`
  host port; the Dockerfile needs no `EXPOSE`.
- `/health` is **liveness only** (independent of Redis) so a backing blip never
  fails the blue-green health gate; `/redis` dials Redis by name and returns its
  `+PONG` — the proof that the shared network is wired.
- `Dockerfile` / `.dockerignore` are exactly what **`phelix dockerize`
  generates** for a Go project.

## Run it

```bash
cd examples/docker-compose-go

# 1. Backing tier: creates the "phelix-compose-go" network AND starts Redis.
docker compose up -d

# 2. App tier: Phelix builds the image and runs it as a container on that network.
phelix proxy                  # zero-downtime proxy (build auto-starts it too)
phelix build compose-go-demo  # first deploy: image → container on the network → health → proxy

# 3. Verify — the app reached Redis across the shared network.
curl localhost:8080/          # hello from compose-go-demo on port 8080
curl localhost:8080/health    # ok
curl localhost:8080/redis     # redis redis:6379 -> +PONG
```

Ship a change with zero downtime, then roll back — the container reattaches to
the same network each time:

```bash
phelix rebuild compose-go-demo    # builds :v2, health-checks it, switches the proxy
phelix rollback compose-go-demo   # back to the previous image, zero downtime
```

## Why the app is NOT a service in `docker-compose.yml`

Phelix owns the app so it can blue-green it — start a second container,
health-check it, cut the proxy over, drain the old one. Compose can't do that,
and the two owners would fight over the public port. So `docker-compose.yml`
holds **only** the backing tier; `docker compose down` never touches the
Phelix-managed app container.

**Can the app run as a compose service?** Yes — but only in the *other* model,
where compose owns everything and Phelix is not used to run it:

```yaml
# compose-only (NOT this example): docker compose up runs the app too.
services:
  app:
    build: .
    environment: { PORT: "8080", REDIS_ADDR: "redis:6379" }
    ports: ["127.0.0.1:8080:8080"]
    networks: [appnet]
    depends_on: [redis]
  redis:
    image: redis:7-alpine
    networks: [appnet]
networks: { appnet: {} }
```

That works (`docker compose up` serves the app and it reaches `redis`), but it
gives up Phelix's zero-downtime deploys, versioning, health-gated cut-over and
proxy. The two models are **mutually exclusive**: you can't both `phelix build`
the app and publish it from compose — they collide on the public port. Pick one.
Use `phelix dockerize <app> --with-compose --depends-on redis` to scaffold the
compose-only variant.

## Generate the Dockerfile yourself

```bash
phelix dockerize compose-go-demo --tag latest
```

## Further reading

- [Docker runtime](../../docs/guides/docker-runtime.md) — the split-ownership
  model and the `deploy.network` knob in full.
- [Docker image building](../../docs/guides/docker-images.md)
- [Zero-downtime deployments](../../docs/guides/zero-downtime-deployments.md)
