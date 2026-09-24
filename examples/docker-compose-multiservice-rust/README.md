# docker-compose-multiservice-rust

The Rust counterpart of
[`../docker-compose-multiservice-go`](../docker-compose-multiservice-go/):
**several Rust services, one shared backing tier.** `api/` and `worker/` are two
**independent Phelix apps** (two `phelix.yaml` files, two public ports) that
share one Docker network and one Redis. Compose owns only the backing tier; each
app is built and zero-downtime-deployed by Phelix on its own.

## When to use this

You run more than one small service on a box and want each deployed
independently (its own version history, its own zero-downtime cut-over) while
they share backing infrastructure. **N services = N Phelix apps.**

## Layout

```
docker-compose.yml        # redis (always) + api/worker under the "local" profile
api/     phelix.yaml (api-rust-demo,    port 8080) + Cargo.toml + src/ + Dockerfile
worker/  phelix.yaml (worker-rust-demo, port 8090) + Cargo.toml + src/ + Dockerfile
```

## Local development

```bash
cd examples/docker-compose-multiservice-rust
docker compose --profile local up --build   # redis + api (:8080) + worker (:8090)
```

## Server (Phelix deploys each app; compose runs only Redis)

```bash
cd examples/docker-compose-multiservice-rust
docker compose up -d          # ONLY redis (+ the shared network)
phelix proxy

(cd api    && phelix build api-rust-demo)      # public :8080
(cd worker && phelix build worker-rust-demo)   # public :8090

curl localhost:8080/redis     # redis redis:6379 -> +PONG
curl localhost:8090/redis     # redis redis:6379 -> +PONG
```

Both attach to `docker-compose-multiservice-rust_appnet` and reach `redis` by
name. Redeploy either independently: `cd api && phelix rebuild api-rust-demo`.

## api → worker calls go through the proxy, not the container name

When `api` needs `worker`, call **worker's public proxy port**
(`http://<host>:8090`) — the stable, zero-downtime address. Do **not** call a
container DNS name like `http://worker:8080`: Phelix gives managed app containers
**no network alias**, because blue-green/rolling briefly runs two containers of
the same app and a shared alias would let DNS round-robin hit the unproven one.
Container DNS names are for the **backing** tier (`redis`), which Phelix never
blue-greens.

## The ownership rule

On the server the app services stay behind `profiles: ["local"]`, so
`docker compose up -d` starts only Redis and each app has exactly one owner
(Phelix). The ownership-conflict warning stays silent.

> This mirrors the Go example one-for-one; only the language toolchain differs.
