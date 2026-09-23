# Health Checks

`phelix health` configures HTTP health endpoints and the deploy-time health
tier.

```bash
phelix health set <App> --path /health --interval 10s --retries 3 --mode auto
phelix health add  <App> --name "API" --url https://api.example.com/health
phelix health list   <App>
phelix health remove <App> --name "API"
phelix health status <App> [--watch]   # --watch = live terminal dashboard
```

## Deploy health tiers

Used by blue-green/rolling to decide when an instance is safe to receive
traffic:

| Tier | Trigger | "Alive" means |
|------|---------|---------------|
| Tier 1 | Explicit `--path` set | HTTP 2xx on that path |
| Tier 2 | No path, app speaks HTTP | Any HTTP response (even 404/500) |
| Tier 3 (TCP) | Not HTTP, or `--mode tcp-only` | TCP port accepts connections |
| Tier 3 (PID) | Worker/daemon, or `--mode none` | Process PID still exists |

`--mode` options: `auto` (default), `http`, `tcp-only`, `none`.

> **Why Tier 2 accepts any status:** the goal at Tier 2 is to prove the process
> answers HTTP at all, not that a specific route succeeds. Apps that return 404
> at `/` (no root handler) must not be falsely marked unhealthy mid-deploy.
> Semantic 2xx success is reserved for Tier 1, where you explicitly opt in via
> `--path`. See [monitoring architecture](../architecture/monitoring.md) for the
> tier-selection internals.

## Health endpoints in `phelix.yaml`

Endpoints declared in `phelix.yaml` are applied to the app's persisted health
configuration on every `phelix build` / `phelix rebuild`, so:

- `phelix health list <app>` and `phelix health status <app>` show them.
- Zero-downtime deploys use the first endpoint's `path`/`mode` as the deploy
  health tier (same as `phelix health set`).
- The `phelix health set/add/remove` commands keep working; a `health:` block in
  the config replaces the persisted endpoints at the next build/rebuild, while
  apps without a `health:` block keep whatever was set via the commands.

See the [configuration reference](../reference/configuration.md) for the full
`health.endpoints[]` field table.

## Related

- [Zero-downtime deployments](zero-downtime-deployments.md) — how the deploy
  tier gates traffic.
- [Monitoring](monitoring.md) — how health is reported to the dashboard.
- [Command reference](../reference/commands.md#health-checks).
