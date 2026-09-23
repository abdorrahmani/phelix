# The PORT Contract

Phelix sets the `PORT` environment variable. Applications managed by Phelix
should listen on the port provided by `PORT` rather than hardcoding a port.

```bash
phelix build api --port 4000
```

starts your application with `PORT=4000`, so it should listen on `:4000`. If the
process runs but nothing listens on the requested port, Phelix reports a
port-validation failure and suggests `phelix doctor`.

Go example:

```go
port := os.Getenv("PORT")
if port == "" {
    port = "3000"
}

log.Fatal(http.ListenAndServe(":"+port, router))
```

## Three distinct port concepts

| Concept | Example | Who owns it |
|---------|---------|-------------|
| Public proxy port | `:8080` | The Phelix proxy (clients connect here) |
| Internal application port | `:49152` / `:49153` | Phelix assigns per instance (blue/green get different ports) |
| `PORT` environment variable | `PORT=49152` | How Phelix tells each instance which internal port to use |

With blue-green deployment, each instance receives its own `PORT`; the proxy
owns the single public port. A failed new deployment never touches the currently
active instance.

## Why it matters

The PORT contract is what makes [zero-downtime
deployments](../guides/zero-downtime-deployments.md) and the [Docker
runtime](../guides/docker-runtime.md) possible: Phelix runs old and new
instances side by side on different internal ports and switches the public proxy
port between them. An app that hardcodes its listen port cannot be routed to.

`phelix doctor` exists to catch hardcoded ports — run it if a build reports a
port-validation failure. `phelix init` also prints a warning (and points to
`phelix doctor`) if it detects a hardcoded listen port, but it never modifies
your source code.

## Examples

Both starter examples read `PORT` exactly as the contract requires:

- [`examples/simple-go`](../../examples/simple-go/) — `os.Getenv("PORT")`.
- [`examples/simple-rust`](../../examples/simple-rust/) — `env::var("PORT")`.

(Note: `phelix doctor`'s static PORT check recognizes the Go idiom and passes it,
but reports the Rust idiom as an inconclusive ⚠ warning rather than a pass — the
Rust example does honor the contract regardless.)
