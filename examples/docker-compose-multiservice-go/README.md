# docker-compose-multiservice-go

**Several Go services, one shared backing tier.** `api/` and `worker/` are two
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
api/     phelix.yaml (api-demo,    port 8080) + main.go + Dockerfile
worker/  phelix.yaml (worker-demo, port 8090) + main.go + Dockerfile
```

## Local development

```bash
cd examples/docker-compose-multiservice-go
docker compose --profile local up --build   # redis + api (:8080) + worker (:8090)
```

## Server (Phelix deploys each app; compose runs only Redis)

```bash
cd examples/docker-compose-multiservice-go
docker compose up -d          # ONLY redis (+ the shared network)
phelix proxy

(cd api    && phelix build api-demo)      # public :8080
(cd worker && phelix build worker-demo)   # public :8090

curl localhost:8080/redis     # redis redis:6379 -> +PONG
curl localhost:8090/redis     # redis redis:6379 -> +PONG
```

Redeploy either independently: `cd api && phelix rebuild api-demo`. Both attach
to `docker-compose-multiservice-go_appnet` and reach `redis` by name.

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

> Rust is analogous: each service is a `Cargo.toml` + Rust `Dockerfile` +
> `phelix.yaml`; the compose shape is unchanged.
