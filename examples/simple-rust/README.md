# simple-rust

The smallest Rust application that Phelix can build, run, and deploy. Like
[simple-go](../simple-go/), it exists to demonstrate the Phelix
[PORT contract](../../docs/getting-started/the-port-contract.md): the app binds
the port given in the `PORT` environment variable.

Standard library only — no external crates. It serves plain HTTP directly over a
`TcpListener`, which keeps the example dependency-free; a real service would use
an HTTP framework.

## What it demonstrates

- Reading and validating `PORT`, exiting with a clear message on a bad value.
- A `/health` endpoint for the Phelix deploy health tier (see
  [Health checks](../../docs/guides/health-checks.md)).
- A minimal `phelix.yaml` (see the
  [configuration reference](../../docs/reference/configuration.md)).

## Run it directly (no Phelix)

```bash
cd examples/simple-rust
PORT=8080 cargo run
# in another terminal:
curl localhost:8080/         # hello from simple-rust on port 8080
curl localhost:8080/health   # ok
```

If `PORT` is missing it falls back to `8080`; a `PORT` that is not an integer in
`1-65535` makes the app exit with a clear error.

## How it uses `PORT`

```rust
let port = env::var("PORT").unwrap_or_else(|_| "8080".to_string());
let port_num: u16 = port.parse().unwrap_or_else(|_| { /* exit with an error */ });
```

Phelix sets `PORT` for each instance it launches; the public port is owned by
the proxy. The app only binds whatever `PORT` says.

## Build and run with Phelix

Phelix auto-detects Rust from `Cargo.toml`. From this directory:

```bash
phelix doctor          # optional: confirms Rust detection + build tools
phelix build           # compiles (cargo) and starts the app as "simple-rust"
phelix status simple-rust
phelix log simple-rust
```

> `phelix doctor` reports the PORT check as a ⚠ warning
> (`could not be verified automatically`) for this example — its static check
> recognizes Go's `os.Getenv("PORT")` but not Rust's `env::var("PORT")`. This is
> an inconclusive result, not a failure: the app does read `PORT` (see
> `src/main.rs`), and `doctor` still exits successfully.

## Deploy a change

```bash
# edit src/main.rs, then:
phelix rebuild simple-rust
phelix rollback simple-rust      # revert if needed
```

Uses `deploy.strategy: classic`. For zero downtime see the
[blue-green example](../blue-green/).

## Further reading

- [Quick Start](../../docs/getting-started/quick-start.md)
- [The PORT Contract](../../docs/getting-started/the-port-contract.md)
- [CLI Commands](../../docs/reference/commands.md)
- [Troubleshooting](../../docs/guides/troubleshooting.md)
