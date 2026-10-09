package phelixerr

// Code is a stable, machine-readable error category.
type Code string

// Well-known codes. These strings are part of the external contract: they
// appear in CLI output, JSON API responses, and can drive exit codes, so they
// must stay stable. Prefer reusing a category over inventing a new one.
const (
	CodeOK               Code = "OK"
	CodeUnknown          Code = "UNKNOWN"
	CodeInvalidArgument  Code = "INVALID_ARGUMENT"
	CodeNotFound         Code = "NOT_FOUND"
	CodeAlreadyExists    Code = "ALREADY_EXISTS"
	CodePermissionDenied Code = "PERMISSION_DENIED"

	// Authentication / authorization.
	CodeUnauthenticated    Code = "UNAUTHENTICATED"
	CodeUnauthorized       Code = "UNAUTHORIZED"
	CodeInvalidCredentials Code = "INVALID_CREDENTIALS"
	CodeSessionExpired     Code = "SESSION_EXPIRED"
	// CodeRateLimited: the backend throttled the request (HTTP 429 on login
	// with a Retry-After window, or a gRPC ResourceExhausted budget). The
	// operation may succeed again once the announced window passes.
	CodeRateLimited Code = "RATE_LIMITED"

	// Configuration / validation.
	CodeConfiguration Code = "CONFIGURATION_ERROR"
	CodeValidation    Code = "VALIDATION_ERROR"

	// CodeIdempotencyConflict: a request key was reused for a materially
	// different operation. The key's original result stands; the caller must
	// supply a fresh key for the new operation. Phase 1 introduced this for
	// the CLI's request-key idempotency on deploy/rebuild/rollback.
	CodeIdempotencyConflict Code = "IDEMPOTENCY_CONFLICT"

	// Plan codes (Phase 3). Plans fail closed: a plan whose semantic content
	// changed after creation, whose preconditions no longer hold, or whose
	// required capability disappeared is never executed. PLAN_STALE is
	// deliberately not retryable — the remedy is creating a new plan, not
	// repeating the request.
	CodePlanInvalid           Code = "PLAN_INVALID"
	CodePlanCorrupt           Code = "PLAN_CORRUPT"
	CodePlanHashMismatch      Code = "PLAN_HASH_MISMATCH"
	CodePlanStale             Code = "PLAN_STALE"
	CodePlanCapabilityMissing Code = "PLAN_CAPABILITY_MISSING"

	// Execution authorization codes (Phase 4). The authorization boundary sits
	// between a validated Plan and the execution engine; every one of these
	// means the boundary refused and NOTHING mutated. They deliberately
	// distinguish the four different truths an agent must not confuse:
	// "you may not do this" (AUTHZ_DENIED), "the boundary could not decide"
	// (AUTHZ_UNAVAILABLE), "the host's authorization configuration is broken"
	// (AUTHZ_INVALID) and the approval states. Plan staleness keeps its own
	// PLAN_* codes — a valid authorization never resurrects a stale plan.
	//
	// There is deliberately no ACTOR_UNAUTHENTICATED code: an actor that
	// cannot satisfy a rule's authentication requirement simply matches no
	// allow rule, which is AUTHZ_DENIED. A separate code would describe why a
	// rule did not match, not a distinct outcome.
	CodeAuthzDenied      Code = "AUTHZ_DENIED"
	CodeAuthzUnavailable Code = "AUTHZ_UNAVAILABLE"
	CodeAuthzInvalid     Code = "AUTHZ_INVALID"
	// CodeApprovalRequired: the decision requires an approval bound to this
	// exact plan and none was found. Not a transient failure — the remedy is
	// an explicit approval, not a retry.
	CodeApprovalRequired Code = "APPROVAL_REQUIRED"
	// CodeApprovalStale: an approval exists but its execution binding
	// (plan id, plan hash, action, target) no longer matches the plan being
	// applied. Approvals are never reusable across plans.
	CodeApprovalStale Code = "APPROVAL_STALE"
	// CodeApprovalInvalid: the approval artifact itself is unusable — corrupt,
	// foreign schema, or content that no longer hashes to its stored value.
	CodeApprovalInvalid Code = "APPROVAL_INVALID"

	// Build.
	CodeBuildFailed        Code = "BUILD_FAILED"
	CodeToolchainNotFound  Code = "TOOLCHAIN_NOT_FOUND"
	CodeBuildTimeout       Code = "BUILD_TIMEOUT"
	CodeUnsupportedProject Code = "UNSUPPORTED_PROJECT"
	// CodeGitSyncFailed: the webhook's exact-commit Git source preparation
	// failed (invalid repository, missing remote, fetch failure, commit not
	// found, worktree creation). The rebuild pipeline never runs in this
	// case — the deployed source must never silently fall back to whatever
	// is on disk.
	CodeGitSyncFailed Code = "GIT_SYNC_FAILED"

	// Deploy / rollout.
	CodeDeployFailed        Code = "DEPLOY_FAILED"
	CodeInstanceStartFailed Code = "INSTANCE_START_FAILED"
	CodeHealthCheckFailed   Code = "HEALTH_CHECK_FAILED"
	CodeDeployLocked        Code = "DEPLOY_LOCKED"
	CodeVersionNotFound     Code = "VERSION_NOT_FOUND"
	// CodeCanaryRegression: a canary/progressive rollout was aborted because
	// the new version degraded traffic it served (failed health probes or a
	// metrics comparison against the stable baseline). Distinct from
	// HEALTH_CHECK_FAILED so automation can tell "candidate never became
	// healthy" from "candidate was healthy but regressed under real traffic".
	// The rollout engine restores the stable version before returning this.
	CodeCanaryRegression Code = "CANARY_REGRESSION"

	// Rollback.
	CodeRollbackFailed         Code = "ROLLBACK_FAILED"
	CodeRollbackTargetNotFound Code = "ROLLBACK_TARGET_NOT_FOUND"
	// CodeRollbackVerifyFailed: the rollback execution itself succeeded (the
	// target version is serving), but stability verification failed or was
	// cancelled. Distinct from ROLLBACK_FAILED so automation can tell "the
	// switch never happened" from "the switch happened and the target proved
	// unstable".
	CodeRollbackVerifyFailed Code = "ROLLBACK_VERIFY_FAILED"

	// CodeAutoRollbackFailed: the deployment failed and the automatic
	// rollback ALSO failed — the previous known-good version could not be
	// restored safely and the app may need manual intervention. Distinct from
	// DEPLOY_FAILED so automation can tell "deploy failed, previous version
	// serving again" from "deploy failed AND recovery failed; state degraded".
	CodeAutoRollbackFailed Code = "AUTO_ROLLBACK_FAILED"

	// Process / OS.
	CodeProcessFailed Code = "PROCESS_FAILED"
	CodeFilesystem    Code = "FILESYSTEM_ERROR"
	// CodeResourceOOM: a Phelix instance was killed because the memory limit
	// of its per-instance cgroup was exceeded — cgroup-v2 memory.events
	// oom_kill increased during the instance's lifetime. Distinct from
	// PROCESS_FAILED (generic process exit) and HEALTH_CHECK_FAILED (failed
	// probe) so automation can tell a resource-limit death from an
	// application bug or an unresponsive candidate.
	CodeResourceOOM Code = "RESOURCE_OOM"

	// Docker.
	CodeDocker                  Code = "DOCKER_ERROR"
	CodeDockerDaemonUnavailable Code = "DOCKER_DAEMON_UNAVAILABLE"

	// Network.
	CodeNetwork    Code = "NETWORK_ERROR"
	CodeConnection Code = "CONNECTION_ERROR"
	CodeTimeout    Code = "TIMEOUT"

	// Self-update (phelix update).
	CodeUpdateFailed Code = "UPDATE_FAILED"

	// Proxy.
	CodeProxy           Code = "PROXY_ERROR"
	CodePortUnavailable Code = "PORT_UNAVAILABLE"

	// Transport / server.
	CodeGRPC       Code = "GRPC_ERROR"
	CodeServer     Code = "SERVER_ERROR"
	CodeEncryption Code = "ENCRYPTION_ERROR"

	// Remote command channel (MonitorStream). CodeUnimplemented marks a
	// command type the agent cannot execute (e.g. rollback before a handler
	// is wired); CodeUnavailable a capability temporarily not usable.
	CodeUnimplemented Code = "UNIMPLEMENTED"
	CodeUnavailable   Code = "UNAVAILABLE"
)

// String returns the stable code string.
func (c Code) String() string { return string(c) }
