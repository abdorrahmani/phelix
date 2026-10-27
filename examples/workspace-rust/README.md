# workspace-rust

Three Rust services managed as one unit by a `phelix.workspace.yaml` manifest.
The Rust twin of [workspace-go](../workspace-go/): same shape, same commands,
same dependency order.

Standard library only. No external crates.

## Services

| Service | Port | Strategy | Depends on |
|---|---|---|---|
| `gateway` | 8180 | classic | auth, orders |
| `auth` | 8181 | blue-green | none |
| `orders` | 8182 | classic | auth |

The ports sit in the 8180 range so this example can run next to its Go twin
without a conflict.

`gateway` forwards `/auth/*` to `auth` and `/orders/*` to `orders`, stripping
the prefix. `orders` calls `auth` for a token when it serves the order list.

## Layout

```
Cargo.toml                [workspace] with three members
phelix.workspace.yaml     the mesh: paths, ports, strategies, depends_on
crates/gateway/           Cargo.toml + src/main.rs + phelix.yaml
crates/auth/              Cargo.toml + src/main.rs + phelix.yaml
crates/orders/            Cargo.toml + src/main.rs + phelix.yaml
```

Each crate keeps its own `phelix.yaml`, which here only declares the health
endpoint. The manifest owns the name, port, strategy, and dependency order.

## The manifest

```yaml
name: workspace-rust

services:
  gateway:
    path: ./crates/gateway
    port: 8180
    depends_on: [auth, orders]
  auth:
    path: ./crates/auth
    port: 8181
    strategy: blue-green
  orders:
    path: ./crates/orders
    port: 8182
    depends_on: [auth]
```

Phelix reads the manifest from the current directory only. Run every workspace
command from this directory.

## Run it without Phelix

The root `Cargo.toml` declares the workspace, so one command builds all three:

```bash
cd examples/workspace-rust
cargo build
```

The binaries land in the workspace target directory, `target/debug/`, not in
each crate. Phelix resolves that path, so a workspace member builds correctly.

Starting them by hand still means three terminals:

```bash
PORT=8181 cargo run -p auth
PORT=8182 cargo run -p orders
PORT=8180 cargo run -p gateway
```

## Run it with Phelix

```bash
cd examples/workspace-rust
phelix build --all
phelix deploy --all
phelix status --workspace
```

`build --all` groups the services into dependency levels and builds each level
in parallel:

```
level 0: auth
level 1: orders
level 2: gateway
```

`deploy --all` goes one service at a time in that same order, through the
regular rebuild path: per-service deploy lock, health gate, version promotion.
The first failure halts the mesh.

`status --workspace` prints one table:

```
NAME     STATUS   VERSION  HEALTH   PORT  STRATEGY    DEPENDS ON
auth     running  v2       healthy  8181  blue-green  []
gateway  running  v2       healthy  8180  classic     [auth orders]
orders   running  v2       healthy  8182  classic     [auth]
```

## Check it

With the mesh deployed:

```bash
curl localhost:8180/               # the routes the gateway exposes
curl localhost:8180/auth/token     # forwarded to auth
curl localhost:8180/orders/orders  # forwarded to orders, which calls auth
curl localhost:8180/health         # gateway health, never forwarded
```

The `/orders/orders` response carries an `auth_token` field, which is the mesh
dependency working end to end.

## Roll the whole mesh back

```bash
phelix rollback --all --reason "testing the rollback"
```

Services revert in reverse dependency order: `gateway` first, then `orders`,
then `auth`.

## A failing deploy

Stop one service and try to deploy the mesh:

```bash
phelix stop auth
phelix deploy --all
```

The run halts at `auth` with the underlying error, and the workspace state
file records which services succeeded and which did not.

## Native discovery

Delete the manifest and Phelix falls back to the root `Cargo.toml`:

```bash
mv phelix.workspace.yaml /tmp/
phelix build --all --port-base 9100
```

Now the members come from `[workspace] members`, and each gets a port walking
up from 9100. A Cargo workspace carries no dependency information, so
everything builds in one level. Put the manifest back to restore the order and
ports:

```bash
mv /tmp/phelix.workspace.yaml .
```

## Where state lives

```
~/.phelix/workspaces/workspace-rust/workspace.json   membership + last outcome
~/.phelix/workspaces/workspace-rust/workspace.lock   orchestration lock
~/.phelix/apps/<service>/deploy.json                 per-service deployment
~/.phelix/apps/<service>/versions.json               per-service version history
```

The workspace file records membership and the last orchestrated outcome. Each
service's runtime truth stays in its own stores, exactly as for a single app.

## Further reading

- [Workspaces guide](../../docs/guides/workspaces.md)
- [The PORT contract](../../docs/getting-started/the-port-contract.md)
- [Health checks](../../docs/guides/health-checks.md)
- [Go twin](../workspace-go/)
