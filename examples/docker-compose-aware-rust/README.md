# docker-compose-aware-rust (pattern B)

The Rust counterpart of
[`../docker-compose-aware-go`](../docker-compose-aware-go/): **docker-compose is
the source of truth for the app's build; Phelix builds the image *from* that
compose service.** Set `deploy.docker.build: compose` and name the service —
Phelix runs `docker compose build <service>` (applying its `context` / `args` /
`target`), then tags the result `aware-rust-demo:vN`.

## When to use this

Your compose file already defines a real `build:` block (a `RUST_VERSION` arg, a
`prod` target stage) and you don't want to duplicate that into a separate
Phelix-owned Dockerfile.

## What Phelix does and does NOT take from compose

- **Build config (context, dockerfile, args, target):** used — that's the point.
- **`environment` / `env_file`:** NOT imported. Runtime env is `phelix env`.
- **`depends_on`:** not honored — the app tolerates a briefly-unavailable backing
  service.

## Server steps

```bash
cd examples/docker-compose-aware-rust

docker compose up -d mysql redis   # backing tier ONLY — never `docker compose up app`
phelix env set aware-rust-demo DATABASE_URL "mysql://user:pass@mysql:3306/app"
phelix env set aware-rust-demo REDIS_ADDR   "redis:6379"

phelix proxy
phelix build aware-rust-demo       # `docker compose build app` → tag aware-rust-demo:v1 → run on the net
curl localhost:8080/backends       # mysql mysql:3306 -> reachable / redis redis:6379 -> +PONG
phelix rebuild aware-rust-demo     # subsequent zero-downtime deploys, same compose build
```

`deploy.network` is `docker-compose-aware-rust_appnet` (the compose default
`<project>_appnet`; confirm with `docker network ls`).

## The ownership rule

Run only the backing services on the server (`docker compose up -d mysql redis`),
never `docker compose up app` — Phelix owns the app tier. If a compose-run `app`
container is present when you deploy, Phelix warns that the app has two owners.

> This mirrors the Go example one-for-one; only the language toolchain and the
> Dockerfile's `prod` stage differ.
