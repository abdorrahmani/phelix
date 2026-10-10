# Exit Codes

Exit codes are part of the CLI's script-facing contract: **same category ⇒ same
exit code**, so automation can rely on them. `--json` never changes the exit
code for a given outcome — the JSON error envelope carries the same code in
`error.exit_code`.

| Exit | Category | Typical codes |
|-----:|----------|---------------|
| 0 | success | — |
| 1 | generic failure | `UNKNOWN`, `SERVER_ERROR`, `UPDATE_FAILED`, `IDEMPOTENCY_CONFLICT`, `SESSION_CONFLICT`, plain errors |
| 2 | invalid usage / arguments (bad invocation) | `INVALID_ARGUMENT` |
| 3 | validation failure (execution result, e.g. `doctor` checks) | `VALIDATION_ERROR`, `PLAN_*`, `SESSION_INVALID`, `SESSION_CORRUPT`, `SESSION_INVALID_TRANSITION` |
| 10 | authentication | `UNAUTHENTICATED`, `INVALID_CREDENTIALS`, `SESSION_EXPIRED` |
| 11 | permission | `PERMISSION_DENIED`, `UNAUTHORIZED`, `AUTHZ_DENIED`, `AUTHZ_UNAVAILABLE`, `AUTHZ_INVALID`, `APPROVAL_REQUIRED`, `APPROVAL_STALE`, `APPROVAL_INVALID` |
| 12 | not found | `NOT_FOUND`, `VERSION_NOT_FOUND`, `ROLLBACK_TARGET_NOT_FOUND` |
| 20 | build | `BUILD_FAILED`, `BUILD_TIMEOUT`, `TOOLCHAIN_NOT_FOUND`, `UNSUPPORTED_PROJECT`, `GIT_SYNC_FAILED` |
| 21 | deploy | `DEPLOY_FAILED`, `INSTANCE_START_FAILED`, `HEALTH_CHECK_FAILED`, `DEPLOY_LOCKED`, `CANARY_REGRESSION`, `RESOURCE_OOM` |
| 22 | rollback | `ROLLBACK_FAILED` |
| 23 | rollback verification failed/cancelled | `ROLLBACK_VERIFY_FAILED` |
| 24 | deployment failed AND automatic rollback failed (state degraded — manual intervention required) | `AUTO_ROLLBACK_FAILED` |
| 30 | network | `CONNECTION_ERROR`, `NETWORK_ERROR`, `GRPC_ERROR`, `PORT_UNAVAILABLE` |
| 40 | configuration | `CONFIGURATION_ERROR` |
| 50 | docker | `DOCKER_ERROR`, `DOCKER_DAEMON_UNAVAILABLE` |
| 60 | timeout | `TIMEOUT` |
| 70 | encryption | `ENCRYPTION_ERROR` |

The 2/3 split keeps invocation failures distinguishable from execution
results: `phelix status` (missing required argument) exits **2**, while
`phelix doctor` (failing checks) exits **3**.

For the Phase 4 authorization codes, exit **11** uniformly means *the
execution authorization boundary refused or could not decide, and nothing
mutated*. The JSON `error.code` distinguishes denied / unavailable / invalid
/ approval states, and `error.retryable` says whether repeating could help
(only `AUTHZ_UNAVAILABLE` is retryable). Plan staleness keeps exit **3** and
its own `PLAN_*` codes, so "not authorized" and "the world moved on" are
never the same signal. See the
[authorization guide](../guides/authorization.md#error-codes).

Agent-session (Phase 6) record and transition failures are validation results
(exit **3**: `SESSION_INVALID`, `SESSION_CORRUPT`, `SESSION_INVALID_TRANSITION`)
— the tracking layer refused and nothing changed. A `SESSION_CONFLICT`
(optimistic-concurrency) is a generic retryable failure (exit **1**). Sessions
never gate execution, so these never collide with the `PLAN_*`/`AUTHZ_*` codes.
See the [agent sessions guide](../guides/agent-sessions.md).

Examples: `phelix status no-such-app` exits **12** (`NOT_FOUND`).

See [error codes](error-codes.md) for the error-reporter registry and known-error
guidance, the [machine contract](machine-contract.md) for the `--json` envelope,
and
[`docs/error-architecture.md`](../error-architecture.md) for the full error
architecture, code inventory, and developer guidelines.
