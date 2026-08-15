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

	// Rollback.
	CodeRollbackFailed         Code = "ROLLBACK_FAILED"
	CodeRollbackTargetNotFound Code = "ROLLBACK_TARGET_NOT_FOUND"

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

	// Proxy.
	CodeProxy           Code = "PROXY_ERROR"
	CodePortUnavailable Code = "PORT_UNAVAILABLE"

	// Transport / server.
	CodeGRPC       Code = "GRPC_ERROR"
	CodeServer     Code = "SERVER_ERROR"
	CodeEncryption Code = "ENCRYPTION_ERROR"
)

// String returns the stable code string.
func (c Code) String() string { return string(c) }
