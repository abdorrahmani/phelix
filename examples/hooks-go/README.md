# hooks-go

A small Go service with every deployment lifecycle hook configured. Use it to
try hooks end to end without wiring a real database or notification system.

The app exposes `/health` and `/ready` on the port Phelix provides in `PORT`.

## Run it directly

```bash
cd examples/hooks-go
PORT=8080 go run .
curl localhost:8080/health
```

## Run it with Phelix

```bash
cd examples/hooks-go
phelix hooks list
phelix rebuild
phelix hooks history
```

The example uses `blue-green` so `pre_traffic` runs as well. A `classic`
deployment skips that hook because it has no separate traffic activation step.

## What each hook does

| Hook | Script | Purpose |
|------|--------|---------|
| `pre_build` | `scripts/pre-build.sh` | Runs `go vet ./...`. |
| `post_build` | `scripts/post-build.sh` | Confirms the artifact is ready. |
| `pre_deploy` | `scripts/pre-deploy.sh` | Writes a migration marker. |
| `post_start` | `scripts/post-start.sh` | Confirms the new instance and marker exist. |
| `pre_traffic` | `scripts/pre-traffic.sh` | Confirms readiness before traffic moves. |
| `post_deploy` | `scripts/post-deploy.sh` | Reports a committed deployment. |
| `pre_stop` | `scripts/pre-stop.sh` | Reports the drain step. |
| `post_rollback` | `scripts/post-rollback.sh` | Reports a restored version. |

`post_deploy`, `pre_stop` and `post_rollback` use `continue_on_error: true`
because they run after the outcome is already committed.

See the [lifecycle hooks guide](../../docs/guides/lifecycle-hooks.md) for the
full configuration and failure behavior.
