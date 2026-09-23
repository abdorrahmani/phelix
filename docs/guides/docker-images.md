# Docker Image Building

Phelix can build optimized multi-stage Docker images for Go and Rust projects.
Language is auto-detected from `go.mod` (Go) or `Cargo.toml` (Rust).

> This is `phelix dockerize` — it *builds and optionally pushes an image*. To
> run your app *instances as containers* under the zero-downtime proxy, see the
> [Docker runtime](docker-runtime.md) instead.

## `phelix dockerize <AppName>`

```bash
phelix dockerize myapp --tag v1.0.0
phelix dockerize myapp --tag v1.0.0 --push --registry ghcr.io/myuser
phelix dockerize myapp --tag v1.0.0 --build-arg VERSION=1.0.0
phelix dockerize myapp --tag v1.0.0 --with-compose --depends-on redis,postgres
```

| Flag | Description |
|------|-------------|
| `--tag` | Version tag for the Docker image (e.g. `v1.2.3`, default `latest`) |
| `--push` | Push the image to the registry after building |
| `--registry` | Registry prefix (e.g. `ghcr.io/user`, `docker.io/myorg`, `harbor.example.com/project`) |
| `-a, --build-arg` | Extra build argument `KEY=VALUE` (repeatable; malformed entries are rejected with `INVALID_ARGUMENT`) |
| `--with-compose` | Generate a `docker-compose.yml` with the app service |
| `--depends-on` | Sidecar services for compose (`redis`, `postgres`, `mysql`, `mongodb`, `rabbitmq`) |
| `--matrix` + family | Matrix Docker builds — see [Matrix builds](matrix-builds.md) |

## How it works

**Language detection:** checks for `go.mod` (Go) or `Cargo.toml` (Rust); fails
with a clear error if neither or both are found.

**Dockerfile generation (if none exists):** multi-stage builds optimized for
Docker layer caching — dependency-heavy layers are cached separately from source
code. The toolchain images are parameterized (`ARG GO_VERSION=1.23` / `ARG
RUST_VERSION=1.80`), so you can pin a different toolchain per build without
editing the file:

```bash
phelix dockerize myapp --build-arg GO_VERSION=1.27
```

*Go:*
1. **Builder stage** — `FROM golang:${GO_VERSION}-alpine` → `COPY go.mod go.sum`
   → `go mod download` → `COPY . .` → `go build`. Dependencies are cached before
   source is copied; `CGO_ENABLED=0` for a fully static binary.
2. **Runtime stage** — `FROM scratch` with just the binary. Smallest possible
   image.

*Rust:*
1. **Dependency cache stage** — `FROM rust:${RUST_VERSION}-slim`, copies
   `Cargo.toml`/`Cargo.lock`, builds a dummy `main.rs` to compile and cache all
   dependencies.
2. **Real build stage** — copies real source; only the app's own code
   recompiles.
3. **Runtime stage** — `debian:bookworm-slim` with `ca-certificates`.

> **If a Dockerfile already exists in the project directory, it is used as-is**
> — Phelix never overwrites a user-provided Dockerfile.

**`.dockerignore` generation (if none exists):** excludes `.git`, `.env` files,
build artifacts, logs, IDE configs, and Phelix internals.

**Build output:** Docker's build progress streams live to the terminal.

**OCI metadata labels:** `org.opencontainers.image.version`, `.revision` (git
commit), `.created` (build timestamp).

**Version integration:** each `dockerize` call records the image reference in
`versions.json` — the same versioning system used by `phelix rollback` — so
future rollback logic can support `docker run <image>` as a deploy source
without redesigning the schema.

**Push to registry:** `--push` pushes to a configurable registry. Credentials
are stored using the same AES-256-GCM encrypted mechanism as environment
variables. If not logged in, a clear error tells you to run `docker login`
first.

## Docker-compose generation

```bash
phelix dockerize myapp --tag v1.0.0 --with-compose --depends-on redis,postgres
```

Produces a ready-to-use `docker-compose.yml` with the app's service plus
requested sidecars (e.g. Redis on port 6379 using `redis:7-alpine`; PostgreSQL
on port 5432 using `postgres:16-alpine`). Supported sidecars: `redis`,
`postgres`, `mysql`, `mongodb`, `rabbitmq`.

> **Important:** Phelix only *generates* the compose file. It does **not** manage
> the lifecycle (start/stop/health) of compose-defined services — use `docker
> compose up -d` directly.

## Related

- [Docker runtime](docker-runtime.md) — run app instances as containers under
  the proxy.
- [Matrix builds](matrix-builds.md) — build many toolchain × platform images at
  once.
- [Environment variables](environment-variables.md) — how registry credentials
  are encrypted.
- [Command reference](../reference/commands.md#docker-image-building).
