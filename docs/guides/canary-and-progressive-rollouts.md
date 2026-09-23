# Canary & Progressive Rollouts

A rollout deploys the new version *alongside* the stable one instead of
replacing it: the stable version keeps serving while the new version (the
canary) receives a share of live traffic through the proxy's weighted routing.
Every step of the plan is verified before the next share is applied, and the
final `100%` step is the promotion.

Rollouts run on the [zero-downtime](zero-downtime-deployments.md) blue-green
topology and require `phelix proxy`.

```bash
# One-shot canary: 5% of traffic for the verification window, then promote
phelix rebuild myapp --canary 5

# Same thing, resolved from phelix.yaml (deploy.strategy: canary)
phelix rebuild myapp

# Multi-step progressive rollout from phelix.yaml
phelix rebuild myapp --strategy progressive
```

`--canary` requires an existing deployment to serve as the stable baseline —
deploy with `--blue-green` (or `--replicas N`) first. A rollout over a rolling
fleet consolidates it to a single stable instance (its first replica) for the
comparison and promotion.

## What a rollout looks like

```text
→ step 1/4: routing 5% of traffic to the canary for 2m
    v12  ███████████████████  95%
    v13  █                     5%
  → monitoring canary for 2m (health and metrics every 5s)
    v13: 1,284 requests, 0.23% errors, p95 81ms
    v12: 24,391 requests, 0.31% errors, p95 74ms
  ✓ step 1/4 verified at 5% traffic
...
✓ traffic switched fully to slot green (zero downtime)
✓ progressive rollout of myapp complete: v13 serves 100% of traffic on slot green
```

## Safety behavior

* **Never route to an unhealthy canary.** The canary must pass the same
  deploy-tier health gate blue-green uses before it receives any traffic, and
  every step keeps probing it for the whole window.
* **Metric comparison.** The proxy counts requests, 5xx/failed responses and
  latency per backend, so each window compares the canary's error rate and p95
  latency against the stable baseline using the `verification` thresholds. With
  no per-backend metrics available the rollout degrades to health-only
  verification instead of guessing.
* **Automatic rollback to stable.** Any regression (health failure, error rate,
  latency) aborts the rollout: traffic switches back to the stable version at
  100% atomically, the canary instance is stopped, and the failure is reported
  (`CANARY_REGRESSION`). This restore is built into the rollout — it does not
  need `--auto-rollback`, which still covers the cases where the restore itself
  fails.
* **Cancellation.** Ctrl-C aborts the rollout through the same restore path (the
  stable version keeps 100% of traffic); the cleanup runs even though the deploy
  was interrupted.
* **Crash recovery.** A rollout interrupted by a crash leaves the stable version
  serving; the next deploy restores stable routing and reclaims the leftover
  canary before doing anything else.
* **Success is durable.** The rollout is only reported successful after the final
  promotion is complete: state persisted, version promoted in `versions.json`,
  old instance drained.

## Configuring rollouts in `phelix.yaml`

```yaml
deploy:
  strategy: canary           # one-shot canary share, then promotion
  rollout:
    canary: 5%               # traffic share for the canary (default 10%)
    duration: 2m             # verification window before promotion (default 30s)
```

```yaml
deploy:
  strategy: progressive      # step-by-step traffic increase
  rollout:
    steps:
      - traffic: 5%
        duration: 2m
      - traffic: 25%
        duration: 5m
      - traffic: 50%
        duration: 5m
      - traffic: 100%        # the final promotion step (required)
    verification:
      interval: 5s           # health/metrics poll cadence (default 5s)
      max_error_rate: 5      # absolute canary error-rate cap, percent (default 5)
      max_error_delta: 2     # canary minus baseline, percentage points (default 2)
      max_p95_factor: 3      # canary p95 at most N × baseline p95 (default 3)
```

`--canary` cannot be combined with `--strategy`, `--blue-green` or `--replicas`
— it already names a strategy. See the full field table and validation rules in
the [configuration reference](../reference/configuration.md).

`canary`/`progressive` strategy overrides sent by the dashboard for a remote
rebuild resolve their plan from the app's `phelix.yaml` `deploy.rollout`.

## Related

- [Zero-downtime deployments](zero-downtime-deployments.md) — the underlying
  blue-green topology.
- [Rollback](rollback.md) — how `--auto-rollback` interacts with a rollout's own
  restore.
- [Configuration](../reference/configuration.md) — `deploy.rollout` fields and
  validation.
- [Command reference](../reference/commands.md#building--rebuilding).
