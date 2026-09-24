# docker-compose-aware-go (pattern B)

**docker-compose is the source of truth for the app's build; Phelix builds the
image *from* that compose service.** Set `deploy.docker.build: compose` and name
the service — Phelix runs `docker compose build <service>` (applying its
`context` / `args` / `target`), then tags the result `aware-go-demo:vN` and runs
it exactly like any docker-runtime app.

## When to use this

Your compose file already defines a real `build:` block (custom args, a `target`
stage, a non-default dockerfile) and you don't want to duplicate that into a
separate Phelix-owned Dockerfile. Phelix reuses the compose build so there is one
build definition.

## What Phelix does and does NOT take from compose

- **Build config (context, dockerfile, args, target):** used — that's the point.
- **`environment` / `env_file`:** NOT imported. Runtime env is `phelix env`
  (encrypted, single source of truth) — no accidental secret duplication.
- **`depends_on`:** not honored — Phelix has no cross-app ordering. The app must
  tolerate a briefly-unavailable backing service.

## Server steps

```bash
cd examples/docker-compose-aware-go

docker compose up -d mysql redis   # backing tier ONLY — never `docker compose up app`
phelix env set aware-go-demo DATABASE_URL "mysql://user:pass@mysql:3306/app"
phelix env set aware-go-demo REDIS_ADDR   "redis:6379"

phelix proxy
phelix build aware-go-demo         # `docker compose build app` → tag aware-go-demo:v1 → run on the net
curl localhost:8080/backends       # mysql mysql:3306 -> reachable / redis redis:6379 -> +PONG
phelix rebuild aware-go-demo       # subsequent zero-downtime deploys, same compose build
```

`deploy.network` is `docker-compose-aware-go_appnet` (the compose default
`<project>_appnet`; confirm with `docker network ls`).

## The ownership rule

Run only the backing services on the server (`docker compose up -d mysql redis`),
never `docker compose up app` — Phelix owns the app tier. If a compose-run `app`
container is present when you deploy, Phelix warns that the app has two owners.

> Rust is analogous: a `Cargo.toml`, a Rust `Dockerfile` with a `prod` target,
> the same `build: { target: prod }` compose service, and `build: compose` in
> `phelix.yaml`.
