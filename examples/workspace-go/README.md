# workspace-go

Three Go services managed as one unit by a `phelix.workspace.yaml` manifest.
Without a workspace you would register, build, and deploy each service
separately, with no shared order and no single status view. Here one command
does the whole mesh.

Standard library only. No external dependencies.

## Services

| Service | Port | Strategy | Depends on |
|---|---|---|---|
| `gateway` | 8080 | classic | auth, orders |
| `auth` | 8081 | blue-green | none |
| `orders` | 8082 | classic | auth |

`gateway` proxies `/auth/*` to `auth` and `/orders/*` to `orders`, stripping
the prefix. `orders` calls `auth` when it serves its order list, so the
dependency the manifest declares is a real call, not a label.

## Layout

```
phelix.workspace.yaml     the mesh: paths, ports, strategies, depends_on
go.work                   the native Go workspace, for `go build` and tooling
gateway/                  go.mod + main.go + phelix.yaml
services/auth/            go.mod + main.go + phelix.yaml
services/orders/          go.mod + main.go + phelix.yaml
```

Each service keeps its own `phelix.yaml`, which here only declares the health
endpoint. The manifest owns the name, port, strategy, and dependency order;
the services stay runnable on their own.

## The manifest

```yaml
name: workspace-go

services:
  gateway:
    path: ./gateway
    port: 8080
    depends_on: [auth, orders]
  auth:
    path: ./services/auth
    port: 8081
    strategy: blue-green
  orders:
    path: ./services/orders
    port: 8082
    depends_on: [auth]
```

Phelix reads the manifest from the current directory only. Run every workspace
command from this directory.

## Run it without Phelix

`go.work` ties the three modules together, so the whole mesh builds from the
root:

```bash
cd examples/workspace-go
go build ./...
```

Starting them by hand still means three terminals, because nothing assigns the
ports:

```bash
(cd services/auth   && PORT=8081 go run .)
(cd services/orders && PORT=8082 go run .)
(cd gateway         && PORT=8080 go run .)
```

That is the problem the workspace removes.

## Run it with Phelix

```bash
cd examples/workspace-go
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
The first failure halts the mesh, so `orders` never comes up against a broken
`auth`.

`status --workspace` prints one table:

```
NAME     STATUS   VERSION  HEALTH   PORT  STRATEGY    DEPENDS ON
auth     running  v2       healthy  8081  blue-green  []
gateway  running  v2       healthy  8080  classic     [auth orders]
orders   running  v2       healthy  8082  classic     [auth]
```

## Check it

With the mesh deployed:

```bash
curl localhost:8080/            # the routes the gateway exposes
curl localhost:8080/auth/token  # proxied to auth
curl localhost:8080/orders/orders  # proxied to orders, which calls auth
curl localhost:8080/health      # gateway health, never proxied
```

The `/orders/orders` response carries an `auth_token` field, which is the mesh
dependency working end to end.

## Roll the whole mesh back

```bash
phelix rollback --all --reason "testing the rollback"
```

Services revert in reverse dependency order: `gateway` first, then `orders`,
then `auth`. A consumer is never left running on a reverted provider.

## A failing deploy

Stop one service and try to deploy the mesh:

```bash
phelix stop auth
phelix deploy --all
```

The run halts at `auth` with the underlying error, and the workspace state
file records which services succeeded and which did not. Fix the service, run
`deploy --all` again, and the run resumes from a clean state.

## Native discovery

Delete the manifest and Phelix falls back to `go.work`:

```bash
mv phelix.workspace.yaml /tmp/
phelix build --all --port-base 9000
```

Now the services come from the `use` entries, and each gets a port walking up
from 9000. A `go.work` file carries no dependency information, so everything
builds in one level. Put the manifest back to restore the order and ports:

```bash
mv /tmp/phelix.workspace.yaml .
```

## Where state lives

```
~/.phelix/workspaces/workspace-go/workspace.json   membership + last outcome
~/.phelix/workspaces/workspace-go/workspace.lock   orchestration lock
~/.phelix/apps/<service>/deploy.json               per-service deployment
~/.phelix/apps/<service>/versions.json             per-service version history
```

The workspace file records membership and the last orchestrated outcome. Each
service's runtime truth stays in its own stores, exactly as for a single app.

## Further reading

- [Workspaces guide](../../docs/guides/workspaces.md)
- [The PORT contract](../../docs/getting-started/the-port-contract.md)
- [Health checks](../../docs/guides/health-checks.md)
- [Rust twin](../workspace-rust/)
