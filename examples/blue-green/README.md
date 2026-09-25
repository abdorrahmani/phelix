# blue-green

A minimal Go service that demonstrates the actual Phelix **zero-downtime
blue-green** workflow. It is the [simple-go](../simple-go/) app plus a `version`
constant you bump between deploys so you can watch traffic cut over.

> This example demonstrates the mechanism; it is **not** production-ready.

## What it demonstrates

- `deploy.strategy: blue-green` in `phelix.yaml` — a zero-downtime deploy where
  the new version starts on the inactive slot, passes its health check, and the
  proxy switches to it atomically. See
  [Zero-downtime deployments](../../docs/guides/zero-downtime-deployments.md).
- A `/health` endpoint that gates promotion (the candidate only receives traffic
  after it answers 200 — see [Health checks](../../docs/guides/health-checks.md)).
- The [PORT contract](../../docs/getting-started/the-port-contract.md): each
  slot gets its own internal `PORT`; the proxy owns the public port.

## The deployment lifecycle

```text
phelix build             → v1 starts, proxy routes the public port to it
edit main.go (bump version), phelix rebuild
                         → v2 starts on the inactive slot (its own internal PORT)
                         → v2 must pass /health before any traffic moves
                         → proxy switches atomically to v2 (no dropped requests)
                         → v1 is drained and stopped
phelix rollback          → same health-gated switch, back to v1
```

If the new version fails its health check, the deploy aborts, the candidate is
killed, and **v1 keeps serving** — a failed deploy never replaces the live
version.

## Commands

```bash
cd examples/blue-green

# 1. Optional: confirm the project is Phelix-compatible
phelix doctor

# 2. First build — creates the app and starts it (rebuild auto-starts the proxy;
#    you can also start it explicitly first).
phelix proxy
phelix build

# 3. Deploy a change with zero downtime:
#    edit main.go, change `const version = "v1"` to "v2", then:
phelix rebuild bluegreen-demo --blue-green

# 4. Watch the switch (run in a loop against the public port while rebuilding):
#    curl localhost:8080/     → flips from v1 to v2 at cut-over

# 5. Inspect deployment state (Deploy + Proxy columns):
phelix status bluegreen-demo
phelix list

# 6. Roll back to the previous version, zero downtime:
phelix rollback bluegreen-demo
```

`deploy.strategy: blue-green` in `phelix.yaml` means a plain `phelix rebuild
bluegreen-demo` also deploys blue-green; the explicit `--blue-green` flag above
just makes it obvious.

## Further reading

- [Zero-downtime deployments](../../docs/guides/zero-downtime-deployments.md)
- [Rollback](../../docs/guides/rollback.md)
- [Configuration](../../docs/reference/configuration.md)
- [Deployment engine (architecture)](../../docs/architecture/deployment.md)
- [Troubleshooting](../../docs/guides/troubleshooting.md)
