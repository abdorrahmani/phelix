# docker-compose-profiles-go (pattern C)

**The app is in `docker-compose.yml` for local development, but on the server
only Phelix runs it.** The app service sits behind a compose **profile**, so a
plain `docker compose up -d` on the server starts only the backing tier (Redis)
— Phelix owns the app tier.

## When to use this

You want one `docker compose up` that runs the whole stack **on your laptop**,
but in production you want Phelix's zero-downtime deploys, versioning and proxy
for the app while compose just keeps the backing services up. A profile keeps
both in a single compose file without the two ever fighting over the app.

## Local development (compose runs everything)

```bash
cd examples/docker-compose-profiles-go
docker compose --profile local up --build   # app + redis
curl localhost:8080/redis                    # redis redis:6379 -> +PONG
```

## Server (Phelix runs the app; compose runs only Redis)

```bash
docker compose up -d            # NO --profile → starts ONLY redis (+ the network)
phelix proxy                    # zero-downtime proxy (build auto-starts it too)
phelix build profiles-go-demo   # Phelix builds the image (dockerfile strategy) and
                                # runs it as a container on docker-compose-profiles-go_appnet
curl localhost:8080/redis       # redis redis:6379 -> +PONG
phelix rebuild profiles-go-demo # subsequent zero-downtime deploys
```

`deploy.network` in [`phelix.yaml`](phelix.yaml) is `docker-compose-profiles-go_appnet`
— the compose default `<project>_appnet` (confirm with `docker network ls`; pin
`name:` on the network to make it prefix-free).

## The ownership rule

The app tier has exactly **one** owner. On the server the app service is gated
behind `profiles: ["local"]`, so `docker compose up -d` never starts it and
Phelix is the sole owner — Phelix's ownership-conflict warning stays silent.
(If you *did* `docker compose --profile local up -d` on the server and then
`phelix build`, the two would collide on the public port and Phelix would warn.)

## Image build strategy

Default (`dockerfile`): Phelix builds the committed [`Dockerfile`](Dockerfile).
The same Dockerfile is what compose builds locally, so local and server images
match. If instead you want Phelix to build *through* the compose service's build
config, see [`../docker-compose-aware-go`](../docker-compose-aware-go/)
(`deploy.docker.build: compose`).

> Rust is analogous: a `Cargo.toml` + a Rust `Dockerfile`, same
> `docker-compose.yml` shape and `phelix.yaml`.
