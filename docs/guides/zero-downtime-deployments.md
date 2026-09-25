# Zero-Downtime Deployments

Phelix supports zero-downtime **blue-green** and **rolling** deploys through the
`phelix proxy` daemon. This guide covers the proxy, both strategies, and the
safety guarantees that make them zero-downtime. For traffic-shifted rollouts see
[Canary & progressive rollouts](canary-and-progressive-rollouts.md).

## The zero-downtime reverse proxy

The `phelix proxy` daemon binds each app's public port and routes traffic to the
active internal instance. Blue-green/rolling deploys atomically switch the
target through a control socket — no dropped connections.

```text
Client → :8080 [phelix proxy]  ──atomic target──→ blue  :9001
                                               ↘ green :9002
```

```bash
phelix proxy              # start in background (detaches and returns)
phelix proxy status       # show enrolled apps + routing
phelix proxy stop         # shut the daemon down (drains up to 30s)
phelix proxy --foreground   # run attached (for process supervisors / systemd)
```

`phelix rebuild --blue-green` / `--replicas` will also auto-start the daemon if
it is not already running. Enrolled apps and their active targets are
**persisted** (`~/.phelix/proxy-state.json`) and restored automatically when the
daemon restarts, so backend instances keep serving through daemon crashes or
reboots without any redeploy.

### Prerequisites

1. Your app must listen on the port given by the `PORT` environment variable
   (Phelix sets this for each internal instance). See
   [The PORT Contract](../getting-started/the-port-contract.md).
2. Prefer a real health endpoint so deploys use Tier 1 checks:
   ```bash
   phelix health set myapp --path /health
   ```
   Without one, Phelix falls back to Tier 2 (any HTTP response) or Tier 3 (TCP
   only) and prints a warning. See [Health checks](health-checks.md).
3. One-time migration: an app currently running outside the proxy (started via a
   classic build/rebuild) binds the public port itself, so the first
   `--blue-green` / `--replicas` deploy cannot enrol it. Stop the app once
   (`phelix stop <app>`) and deploy — the proxy takes over the public port, and
   every deploy after that switches targets with zero downtime.

## Blue-green deploy

Builds a new binary, starts it on the inactive slot (blue ↔ green), waits until
healthy, then atomically switches the proxy. The previous instance is drained
and stopped. A deploy that loses the proxy mid-flight shuts down its unproven
new instance instead of leaking it, and slots left behind by crashed deploys are
reclaimed on the next run.

```bash
phelix rebuild myapp --blue-green
phelix rebuild myapp --blue-green --port 8080
```

## Rolling deploy

Replaces N replicas one at a time. Each replacement starts on a fresh internal
port **alongside** the instance it replaces and joins the proxy only after
passing its health check; the old instance is then drained from rotation and
stopped, so no request is ever routed to a dead port during the roll. Shrinking
(`--replicas N` smaller than before) drains the surplus replicas at the end of
the rollout.

```bash
phelix rebuild myapp --replicas 3
```

## What you see in `list` / `status`

- **Deploy**: `blue-green (blue|green)` or `rolling (×N)`, or `-` for classic
  stop→start rebuilds
- **Proxy**: `on :<public> → <slot>` when enrolled, otherwise `off`

## Safety guarantees

These guarantees are the point of the feature — do not weaken them:

- **A failed new deployment never replaces the live version.** If the new
  instance fails its health check, the deploy **aborts**, the new instance is
  killed, and the currently active instance is left untouched — public traffic
  keeps flowing.
- **Health verification gates traffic.** Blue-green/rolling never point the
  proxy (or the `current` pointer) at an instance that hasn't passed its health
  check.
- **Atomic proxy cut-over.** Switching backends takes effect on the next request
  with no dropped connections. (Internally the proxy reads its target from an
  `atomic.Value` per request — see
  [deployment architecture](../architecture/deployment.md).)
- **Rolling never drops below capacity.** No more than one replica is taken out
  of rotation at a time; a health failure aborts and leaves N-1 serving.
- **Active-version protection on rollback.** [Rollback](rollback.md) on a
  zero-downtime app reuses this same health-checked, atomically-switched path.

## Related

- [Canary & progressive rollouts](canary-and-progressive-rollouts.md) —
  traffic-percentage rollouts on the blue-green topology.
- [Rollback](rollback.md) — reverting a version with the same guarantees.
- [Configuration](../reference/configuration.md) — set `deploy.strategy` so you
  don't repeat `--blue-green` / `--replicas`.
- [Deployment architecture](../architecture/deployment.md) — blue-green/rolling
  internals, the deploy lock, and atomic cut-over.
- [Command reference](../reference/commands.md#zero-downtime-reverse-proxy).
