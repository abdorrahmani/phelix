# simple-go

The smallest Go application that Phelix can build, run, and deploy. It exists to
demonstrate the one hard requirement Phelix places on a managed app: it must
listen on the port given by the `PORT` environment variable.

Standard library only — no external dependencies.

## What it demonstrates

- The [PORT contract](../../docs/getting-started/the-port-contract.md): the app
  reads `PORT` and binds it, rather than hardcoding a port.
- A `/health` endpoint suitable for the Phelix deploy health tier (see
  [Health checks](../../docs/guides/health-checks.md)).
- A minimal `phelix.yaml` (see the
  [configuration reference](../../docs/reference/configuration.md)).

## Run it directly (no Phelix)

```bash
cd examples/simple-go
PORT=8080 go run .
# in another terminal:
curl localhost:8080/         # hello from simple-go on port 8080
curl localhost:8080/health   # ok
```

If `PORT` is missing it falls back to `8080`; if `PORT` is not an integer in
`1-65535` the app exits with a clear error instead of a cryptic bind failure.

## How it uses `PORT`

```go
port := os.Getenv("PORT")
if port == "" {
    port = "8080"
}
```

Phelix sets `PORT` for each instance it launches (a different internal port per
blue-green slot / rolling replica), and the public port is owned by the proxy.
The app only ever needs to bind whatever `PORT` says.

## Build and run with Phelix

From this directory (`phelix.yaml` supplies the name and port):

```bash
phelix build           # compiles and starts the app as "simple-go"
phelix status simple-go
phelix log simple-go
```

Check compatibility first if you like — this reports whether the app honors the
PORT contract:

```bash
phelix doctor
```

## Deploy a change

```bash
# edit main.go, then:
phelix rebuild simple-go          # classic rebuild (this example's strategy)
phelix rollback simple-go         # revert to the previous version if needed
```

This example uses `deploy.strategy: classic` for simplicity. For a zero-downtime
workflow see the [blue-green example](../blue-green/) and
[Zero-downtime deployments](../../docs/guides/zero-downtime-deployments.md).

## Further reading

- [Quick Start](../../docs/getting-started/quick-start.md)
- [The PORT Contract](../../docs/getting-started/the-port-contract.md)
- [CLI Commands](../../docs/reference/commands.md)
- [Configuration](../../docs/reference/configuration.md)
- [Troubleshooting](../../docs/guides/troubleshooting.md)
