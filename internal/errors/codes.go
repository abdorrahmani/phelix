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

	// Build.
	CodeBuildFailed        Code = "BUILD_FAILED"
	CodeToolchainNotFound  Code = "TOOLCHAIN_NOT_FOUND"
	CodeBuildTimeout       Code = "BUILD_TIMEOUT"
	CodeUnsupportedProject Code = "UNSUPPORTED_PROJECT"

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
