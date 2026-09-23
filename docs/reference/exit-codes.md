# Exit Codes

Exit codes are part of the CLI's script-facing contract: **same category ⇒ same
exit code**, so automation can rely on them.

| Exit | Category | Typical codes |
|-----:|----------|---------------|
| 0 | success | — |
| 1 | generic failure | `UNKNOWN`, `SERVER_ERROR`, `ENCRYPTION_ERROR`, plain errors |
| 2 | invalid usage / arguments | `INVALID_ARGUMENT`, `VALIDATION` |
| 10 | authentication | `UNAUTHENTICATED`, `INVALID_CREDENTIALS`, `SESSION_EXPIRED` |
| 11 | permission | `PERMISSION_DENIED` |
| 12 | not found | `NOT_FOUND`, `VERSION_NOT_FOUND`, `ROLLBACK_TARGET_NOT_FOUND` |
| 20 | build | `BUILD_FAILED`, `BUILD_TIMEOUT`, `TOOLCHAIN_NOT_FOUND`, `UNSUPPORTED_PROJECT` |
| 21 | deploy | `DEPLOY_FAILED`, `INSTANCE_START_FAILED`, `HEALTH_CHECK_FAILED`, `DEPLOY_LOCKED` |
| 22 | rollback | `ROLLBACK_FAILED` |
| 23 | rollback verification failed/cancelled | `ROLLBACK_VERIFY_FAILED` |
| 24 | deployment failed AND automatic rollback failed (state degraded — manual intervention required) | `AUTO_ROLLBACK_FAILED` |
| 30 | network | `CONNECTION_ERROR`, `PORT_UNAVAILABLE` |
| 40 | configuration | `CONFIGURATION_ERROR` |
| 50 | docker | `DOCKER_ERROR` |
| 60 | timeout | `TIMEOUT` |
| 70 | encryption | `ENCRYPTION_ERROR` |

Examples: `phelix status no-such-app` exits **12** (`NOT_FOUND`); `phelix status`
(missing required argument) exits **2**.

See [error codes](error-codes.md) for the error-reporter registry and known-error
guidance, and
[`docs/error-architecture.md`](../error-architecture.md) for the full error
architecture, code inventory, and developer guidelines.
