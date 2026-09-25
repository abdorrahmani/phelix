# Monitoring & Health Internals

Covers the gRPC monitor stream, the health daemon, snapshot semantics, and
connection state. For the user-facing view see [Monitoring](../guides/monitoring.md)
and [Health checks](../guides/health-checks.md).

## The monitor stream

`internal/grpc` holds a single, long-lived, TLS-secured gRPC stream to the backend
(`MonitorStream`), opened by the `phelix monitor` daemon. It requires an
authenticated session (full-scope or agent-scoped — see
[Authentication](../guides/authentication.md)) and reconnects with exponential
backoff.

- Health reaches the backend as one `AppHealthSnapshot` per app — a complete,
  idempotent statement of that app's configuration + runtime state, pushed on every
  (re)connect and every 10s tick.
- The daemon sends app info, resource metrics, and logs of every **watched** app
  roughly every 2s.
- Backend rate limits are honored: `ResourceExhausted` on the stream backs the loop
  off (≥30s, escalating) instead of the normal 2s cadence; auth-throttle windows
  park the metadata sync; `DeadlineExceeded` idle timeouts are treated as an
  ordinary disconnect. See the
  [CLI changes required contract](backend-contracts/CLI_CHANGES_REQUIRED.md).

## Watching gate

Server-level data (identity, host metrics, self logs, agent metadata) is
transmitted only while at least one app is watched (`app.AnyWatched`). With nothing
watched, the daemon does not even open the stream — an unbound stream would be
reaped by the backend's pre-bind window and produce reconnect churn — it parks and
re-checks every 5s, and the first `phelix watch` opt-in re-opens it within seconds.
Consequently, dashboard-issued remote commands cannot reach a server while none of
its apps are watched. See [Monitoring guide](../guides/monitoring.md) for the
user-facing behavior.

## Snapshot semantics

`health.SnapshotForApp` returns the current health of one app, or **`nil` when
health is not configured for it**. `internal/grpc` skips `nil` apps entirely —
absence on the wire means "not configured", never "unhealthy". A config with zero
endpoints *is* reported (empty endpoint list) so deletions propagate. This is the
authoritative CLI↔backend contract; there is no second health sync path.

## Health daemon reconciliation

The monitoring daemon's endpoint checkers are **reconciled, not enumerated once**.
`Daemon.Sync` re-reads the configs from disk every 15s and starts/stops/restarts
checkers to match, because `health.json` is written by *different* processes
(`phelix health set/add/remove`, `phelix build`, a backend `HealthCommand`). A
snapshot taken only at startup would miss every app configured afterward.

### Tier selection

`internal/health/tiered.go:SelectTier` picks Tier 1 (user-configured path, **2xx
required**), Tier 2 (HTTP detected — *any* status counts as alive), Tier 3 TCP, or
Tier 3 None (PID only). Tier 2's any-status rule is intentional: apps with no root
handler return 404 and must not be marked unhealthy mid-deploy; semantic 2xx is
reserved for the opt-in Tier 1. `WaitForHealthy` polls until N consecutive
successes or timeout. `internal/deploy/health.go:selectDeployHealth` is the single
tier-resolution point both deploy strategies share.

## Connection state

`internal/connstate` models the monitor connection's state so the daemon and the
stream loop agree on whether the stream is open, parked (nothing watched), backing
off, or disconnected — the basis for the reconnect/backoff and watching-gate logic
above.

## Command execution

`internal/monitor` collects metrics and executes dashboard-issued commands
(transport-agnostic), while `internal/grpc` is the transport. Remote commands
(rebuild with one-off strategy override, rollback, matrix, webhook management)
arrive over the same stream; see the
[remote webhook contract](backend-contracts/remote-webhook-backend-contract.md) and
[agent capabilities contract](backend-contracts/agent-capabilities-backend-contract.md).

## Transport security

`mode: dev` is the only mode using plaintext gRPC (local backends); every other
mode forces TLS with no insecure fallback. Certificate policy and SPKI-pinning
implications are in the
[TLS & certificate policy contract](backend-contracts/tls-cert-policy.md).

## Related

- [Monitoring guide](../guides/monitoring.md), [Health checks](../guides/health-checks.md).
- [Authentication](../guides/authentication.md) — the session the stream requires.
- Backend contracts:
  [agent capabilities](backend-contracts/agent-capabilities-backend-contract.md),
  [docker-runtime telemetry](backend-contracts/docker-runtime-telemetry-backend-contract.md),
  [remote webhook](backend-contracts/remote-webhook-backend-contract.md),
  [TLS policy](backend-contracts/tls-cert-policy.md),
  [CLI changes required](backend-contracts/CLI_CHANGES_REQUIRED.md).

> The historical `DEVELOPMENT.md` referenced a
> `docs/backend-app-health-transport.md` file for the `AppHealthSnapshot`
> transport; that file does not exist in the repository. The snapshot contract is
> summarized above and in the backend contracts; the dangling reference is recorded
> as an issue for Phase 3.
