# Running App Instances as Containers (Docker Runtime)

**Run one Phelix on the host and let it manage your app containers.** This is
the supported way to use Phelix with Docker: a single Phelix agent on the
machine builds an image per app and runs each instance as a container, so **one
host stays one server** on the dashboard no matter how many app containers it
runs.

> This is distinct from [`phelix dockerize`](docker-images.md), which just
> builds/pushes an image. Here, Phelix *runs your app instances as containers*
> under the zero-downtime proxy.

Do **not** install a separate Phelix inside each app container. Each Phelix
runtime is its own agent with its own identity, so three such containers would
register as three servers even though they share one physical host — the exact
problem this model exists to avoid. (See [identity](../architecture/identity.md).)

Phelix keeps running exactly as it does on a bare VPS — one agent, one
`agent_id`, one `apps.json` — but instead of executing each app as a host
process, it **builds an image and runs each instance as a container**.
Blue-green, rolling, canary, health checks, versioning and rollback all work
unchanged: the zero-downtime proxy still owns the single public port and routes
to the container's published loopback port, and a version rollback restarts the
recorded image (`myapp:v11`) instead of a binary.

```text
┌─ VPS — ONE Phelix agent = ONE server on the dashboard ─────────────┐
│  phelix monitor        (host process, systemd — as today)          │
│  phelix proxy  :8080 ─┐                                            │
│                       ├─→ 127.0.0.1:49215  ← docker: billing:v3    │
│                       ├─→ 127.0.0.1:49216  ← docker: auth:v7       │
│                       └─→ 127.0.0.1:49217  ← docker: user:v2       │
└────────────────────────────────────────────────────────────────────┘
```

## Requirements

- Phelix runs **on the host** (the normal `systemd` install), with a reachable
  Docker daemon. Do **not** run Phelix itself inside a container for this model
  unless you deliberately mount the Docker socket — that grants the container
  root-equivalent access to the host (see the security note below).
- Each app must read `PORT` (the same [PORT contract](../getting-started/the-port-contract.md)
  as native apps). Phelix injects `PORT` into the container and publishes it to
  an ephemeral host port bound to `127.0.0.1`.
- The docker runtime rides the zero-downtime topology, so the app needs a
  zero-downtime strategy — `blue-green`, `rolling`, `canary`, or `progressive`.
  The `classic` stop→start path is native-only.

## Turn it on in `phelix.yaml`

```yaml
name: billing
port: 8080
deploy:
  runtime: docker        # ← run instances as containers instead of host processes
  strategy: blue-green   # required: classic is native-only
health:
  endpoints:
    - name: default
      path: /health
      interval: 10s
      retries: 3
      mode: auto
```

`phelix init` writes this for you when you pick the docker runtime (it asks, or
pass `--runtime docker`; it defaults `strategy` to `blue-green` so the file is
valid). You can also flip a single command to docker without editing the file:

```bash
PHELIX_RUNTIME=docker phelix rebuild billing
```

Precedence is `PHELIX_RUNTIME` env → `phelix.yaml` `deploy.runtime` → `native`.

## Deploy exactly as before

```bash
# One-time on the host
phelix proxy                     # zero-downtime proxy (auto-started by rebuild too)

# Build an image (myapp:vN) and roll it out with zero downtime
phelix rebuild billing --blue-green

# Roll back to the previous image, zero downtime
phelix rollback billing
```

Phelix generates a multi-stage `Dockerfile` if the project has none (the same
generator `phelix dockerize` uses; a user-provided `Dockerfile` is always
respected), builds `billing:v<N>`, records it in `versions.json`, then runs it
with the `phelix.managed`, `phelix.app`, and `phelix.slot` labels.

## Step by step: an app you deploy through Docker

This is the whole flow, from an ordinary Go/Rust project directory to a running,
zero-downtime, containerized deployment.

**1. Initialise the project and choose the docker runtime.**

```bash
cd ~/src/billing
phelix init            # pick "docker" at the runtime prompt
# or non-interactively:
phelix init --name billing --port 8080 --runtime docker --yes
```

This writes a `phelix.yaml` with `deploy.runtime: docker` and
`deploy.strategy: blue-green` (classic is native-only, so init upgrades it for
you). `port: 8080` here is the **public** port the proxy will bind — what your
users connect to.

**2. Make sure the app reads `PORT`.** This is the one hard requirement. Phelix
runs the container with `PORT` set and maps that container port to a private host
port the proxy dials; an app that hardcodes its port can't be routed to. Run
`phelix doctor` to check.

```go
port := os.Getenv("PORT")
if port == "" {
    port = "8080"
}
log.Fatal(http.ListenAndServe(":"+port, mux))
```

**3. Provide a Dockerfile — or let Phelix generate one.**

If your project has **no** `Dockerfile`, Phelix generates a good multi-stage one
on the first deploy and never overwrites it later. If you already have one (a
"dockerized project"), Phelix uses **yours as-is**. Either way it only has to
satisfy two rules:

- the image starts your app as its `ENTRYPOINT`/`CMD`, and
- the app listens on the port from the `PORT` environment variable.

You do **not** need to hardcode, `EXPOSE`, or publish a port in the Dockerfile —
Phelix injects `PORT` and publishes the mapping itself. A minimal hand-written
Dockerfile for a Go app:

```dockerfile
# Build
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /bin/app .

# Run
FROM alpine:3.20
RUN adduser -D -u 10001 app
USER app
COPY --from=build /bin/app /bin/app
# The app must bind $PORT (Phelix sets it); no EXPOSE or hardcoded port needed.
ENTRYPOINT ["/bin/app"]
```

For Rust, build with `cargo build --release` and copy the binary into a
`debian:bookworm-slim` (add `ca-certificates` if you make HTTPS calls); the same
`ENTRYPOINT` + `$PORT` rules apply. If the app needs configuration, set it with
`phelix env` (encrypted, injected into the container as a private env-file) — not
with `ENV` lines that bake secrets into the image.

**4. Create and deploy the app for the first time with `phelix build`.**

```bash
phelix proxy                      # zero-downtime proxy (build auto-starts it too)
phelix build billing              # name + port come from phelix.yaml
```

`phelix build` is the command that **creates** the app; `phelix rebuild` only
works on an app that already exists. For a docker-runtime app, `build` skips the
native classic start and goes straight through the container path: Phelix builds
`billing:v1`, runs it as a container on a private loopback port, waits for it to
pass its health check, then points the proxy at it. `phelix list` /
`phelix status` show it serving on the public port. Only Docker is required on
the host — the image is built with the toolchain inside the container, not a host
Go/Rust toolchain.

**5. Ship a change — `phelix rebuild`, zero downtime.**

```bash
phelix rebuild billing            # builds billing:v2, health-checks it, switches over
```

The old container keeps serving until the new one is healthy; only then does the
proxy switch and the old container drain and stop.

**6. Roll back if needed.**

```bash
phelix rollback billing           # back to the previous image, zero downtime
phelix rollback billing --to v1   # or a specific version
```

Everything else — `phelix env`, `phelix health`, canary/progressive rollouts,
build reports — works exactly as in the native runtime; only the launch
mechanism (container instead of host process) changed.

## How instances are identified and cleaned up

- Every container Phelix starts carries a `phelix.managed=true` label plus the
  app and slot. Liveness and shutdown verify the **container id + label** (never
  a recycled host PID), so a live container is never mistaken for dead, and
  Phelix never signals a container it did not create.
- Secrets reach the container through a `0600` `--env-file`, never as `-e` on
  the command line, so decrypted values never appear in `docker inspect` or the
  host process table.
- If a deploy crashes after `docker run` but before recording the instance, the
  orphaned container is reclaimed automatically the next time `phelix monitor`
  starts (only `phelix.managed` containers that no deploy state references are
  removed).

> **Security — Docker socket.** Running Phelix on the host talks to the local
> Docker daemon directly. If you instead run Phelix inside a container, mounting
> `/var/run/docker.sock` into it is equivalent to giving that container root on
> the host. Do this only deliberately, and never expose that container.

> **Security — network exposure.** Managed containers publish only to
> `127.0.0.1`; the `phelix proxy` is the sole public entry point. Keep it that
> way — do not publish app containers on `0.0.0.0` in parallel with the proxy.

## Related

- [Docker image building](docker-images.md) — the `dockerize` command and the
  Dockerfile generator this runtime reuses.
- [Identity](../architecture/identity.md) — the `agent_id` / `app_id` /
  `instance_id` / `machine_id` model and why one host is one server.
- [Zero-downtime deployments](zero-downtime-deployments.md) — the topology the
  docker runtime rides on.
- [Docker runtime telemetry contract](../architecture/backend-contracts/docker-runtime-telemetry-backend-contract.md)
  — how runtime state reaches the backend.
- [Security](security.md) — Docker socket and network-exposure notes.
