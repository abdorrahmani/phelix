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

## Backing services (databases, caches): split ownership

Phelix builds and zero-downtime-deploys your **app tier**. It does **not** run
your databases or caches — you own those, with `docker compose`. The split is
deliberate, not a limitation:

- **Phelix owns the app tier** — the stateless Go/Rust services it builds and
  cuts over. Blue-green and rolling both assume instances are **interchangeable
  and disposable**: start a second copy, shift traffic, discard the old one.
  That is exactly right for a stateless app.
- **You own the backing tier** — `mysql`, `redis`, `postgres`, … — via
  `docker compose`. A single-writer stateful service is the opposite of
  disposable: you cannot start a second `mysql` on the same volume and cut over
  between them, so **Phelix must never blue-green a database.** A backing
  service just needs to be *up*, on a *stable network*, with its *volume* —
  which is precisely what compose is for.

A **shared Docker network** connects the two tiers. The app container Phelix
launches joins the network your backing services already sit on and reaches
them by their **compose service name** (`mysql:3306`, `redis:6379`) — the DNS
compose gives every service on that network. One new key turns it on:

```yaml
deploy:
  runtime: docker
  network: myproj_appnet   # ← attach app containers to this existing network
```

An empty or omitted `network` keeps today's behavior: the container lands on the
default bridge and cannot resolve compose service names.

### Step by step: `billing` (Phelix) + `mysql` and `redis` (compose)

**1. A compose file with ONLY the backing services**, on a named network, with
volumes. Phelix's app is *not* in here.

```yaml
# compose.yml
services:
  mysql:
    image: mysql:8
    networks: [appnet]
    volumes: [db:/var/lib/mysql]
    environment:
      MYSQL_DATABASE: billing
      MYSQL_ROOT_PASSWORD: change-me
  redis:
    image: redis:7
    networks: [appnet]

networks:
  appnet: {}
volumes:
  db: {}
```

```bash
docker compose up -d      # creates the "appnet" network AND starts the services
```

**2. Find the network's real name.** Compose **prefixes** the network with the
project name (the compose directory by default), so `appnet` becomes
`myproj_appnet`:

```bash
docker network ls        # look for <project>_appnet, e.g. myproj_appnet
```

To pin a stable, prefix-free name instead, set `name:` on the network:

```yaml
networks:
  appnet:
    name: appnet         # the network is now literally "appnet"
```

**3. Point `billing`'s `phelix.yaml` at that network:**

```yaml
name: billing
port: 8080
deploy:
  runtime: docker
  strategy: blue-green
  network: myproj_appnet   # the name from `docker network ls`
health:
  endpoints:
    - name: default
      path: /health
      mode: auto
```

**4. Give the app its connection strings by DNS name**, through encrypted env —
never baked into the image:

```bash
phelix env set billing DATABASE_URL "mysql://user:pass@mysql:3306/billing"
phelix env set billing REDIS_URL   "redis://redis:6379"
```

`mysql` and `redis` resolve because the container is on `myproj_appnet`; the
values are injected into the container via the private `0600` env-file (never
`-e` on the command line).

**5. Deploy the app tier as usual:**

```bash
phelix proxy                 # once (build/rebuild auto-start it too)
phelix build billing         # first deploy: build image, run on appnet, health-check, enrol
phelix rebuild billing       # every subsequent zero-downtime deploy
```

Phelix runs `billing:vN` as a container on `myproj_appnet` **and** publishes it
to a loopback port for the proxy — both at once: the app calls `mysql`/`redis`
outbound over the shared network, while inbound traffic still arrives only
through the proxy's public port. If the network does not exist yet, the deploy
stops before building anything and tells you to `docker network create` it or
`docker compose up -d` the backing services first.

### Many services share one network

Each Go/Rust service is its **own** `phelix.yaml` / its own Phelix app — *N
services = N apps*. They can all set the same `network:`; compose still holds
only the backing tier. So `billing`, `auth`, and `users` deploy independently
and all reach `mysql`/`redis` by name.

**App→app calls go through the callee's proxy public port, not its container
name.** Call `auth` at `http://<host>:<auth-public-port>`, never `http://auth:…`.
Phelix deliberately gives managed containers **no network alias**, because
blue-green/rolling briefly runs *two* containers of the same app — a shared
name would let the network's DNS round-robin route requests to the unproven
candidate. The proxy port is the stable, zero-downtime address; container DNS
names are for reaching the *backing* tier, which Phelix never blue-greens.

### Who owns what

- `docker compose down` stops **only** the backing services. Phelix-managed app
  containers are not part of the compose project, so compose never touches them.
- Phelix never restarts, versions, or blue-greens the backing services. It
  builds and cuts over the app tier; compose keeps `mysql`/`redis` up with their
  volumes.

## Choosing how the image is built

Under `deploy.runtime: docker`, one key — `deploy.docker.build` — chooses HOW the
app image is produced. It changes only *where the image comes from*; the
launcher, proxy, network attach, blue-green and rollback are identical.

| Your setup | Set |
|---|---|
| No compose, or the app is not a compose service | `dockerfile` (default — nothing to set) |
| App is in compose for **local dev** (a profile), but on the server only Phelix runs it | `dockerfile` (default) + the profiles pattern |
| Compose is the **source of truth for the build** (context/args/target) and you don't want to duplicate it into a standalone Dockerfile | `compose` |

**`dockerfile` (default):** Phelix generates a Dockerfile if the project has
none (a user's is always respected) and builds it — today's behavior, unchanged.

**`compose`:** Phelix runs `docker compose build <service>` (compose applies the
service's own `build:` config — context, args, target, dockerfile), then tags the
result `<app>:vN`. Requires `deploy.docker.service`; `compose_file` defaults to
`docker-compose.yml`:

```yaml
deploy:
  runtime: docker
  strategy: blue-green
  network: myproj_appnet
  docker:
    build: compose
    service: app          # the compose service whose build config makes this image
```

`build: compose` is **build only**. The compose service's `environment` /
`env_file` are NOT imported into the running container — runtime env stays with
`phelix env` (encrypted, single source of truth, no accidental secret
duplication). `depends_on` is not honored either (Phelix has no cross-app
ordering), so the app must tolerate a briefly-unavailable backing service.

**The ownership rule (every strategy):** the app tier has exactly one owner. If
the same app is *also* run as a docker-compose service, Phelix and compose both
manage it — duplicate instances and a fight over the public port. Phelix prints a
warning before deploying when it detects this (it never blocks). Keep the app
service behind a compose `profile` so `docker compose up -d` on the server never
starts it (the warning then stays silent — the desired signal), or
`docker compose stop <svc>`.

Runnable examples of each choice:

- [`examples/docker-compose-profiles-go`](../../examples/docker-compose-profiles-go/)
  — app in compose for local dev (profile), Phelix builds it (`dockerfile`) on the server.
- [`examples/docker-compose-aware-go`](../../examples/docker-compose-aware-go/)
  — Phelix builds *from* the compose service (`build: compose`).
- [`examples/docker-compose-multiservice-go`](../../examples/docker-compose-multiservice-go/)
  — several app services, one shared backing tier; each is its own Phelix app.

Each has a Rust counterpart (`docker-compose-profiles-rust`,
`docker-compose-aware-rust`, `docker-compose-multiservice-rust`) — identical
`phelix.yaml` and `docker-compose.yml` shape, only the toolchain and Dockerfile
differ.

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

- **Runnable examples:** [`examples/docker-go`](../../examples/docker-go/) /
  [`examples/docker-rust`](../../examples/docker-rust/) run the app as a
  container with no backing services;
  [`examples/docker-compose-go`](../../examples/docker-compose-go/) /
  [`examples/docker-compose-rust`](../../examples/docker-compose-rust/) add this
  split-ownership model — Phelix runs the app on a shared network beside a
  compose-owned Redis backing tier (`deploy.network`), each with a
  `docker-compose.yml` and a Dockerfile generated by `phelix dockerize`.
- [Docker image building](docker-images.md) — the `dockerize` command and the
  Dockerfile generator this runtime reuses.
- [Identity](../architecture/identity.md) — the `agent_id` / `app_id` /
  `instance_id` / `machine_id` model and why one host is one server.
- [Zero-downtime deployments](zero-downtime-deployments.md) — the topology the
  docker runtime rides on.
- [Docker runtime telemetry contract](../architecture/backend-contracts/docker-runtime-telemetry-backend-contract.md)
  — how runtime state reaches the backend.
- [Security](security.md) — Docker socket and network-exposure notes.
