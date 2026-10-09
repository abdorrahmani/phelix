package phelixerr

// retryableCodes are the failure categories where repeating the same
// operation could plausibly succeed without a code or configuration change.
// This is the single classification for retryability: deploy telemetry
// (internal/deploy/telemetry.go failureFromError) and the machine-readable
// error contract (internal/machine) both derive from it, so a code never
// disagrees with itself about retryability across output surfaces.
var retryableCodes = map[Code]bool{
	CodeHealthCheckFailed:   true,
	CodeInstanceStartFailed: true,
	CodeProxy:               true,
	CodeConnection:          true,
	CodeTimeout:             true,
	CodePortUnavailable:     true,
	CodeDeployLocked:        true,
	CodeNetwork:             true,
	CodeProcessFailed:       true,
	CodeRateLimited:         true,
	CodeUnavailable:         true,
	// The authorization backend could not be consulted (I/O failure reading
	// the host policy). The decision itself may succeed once it is readable
	// again. AUTHZ_DENIED / AUTHZ_INVALID / APPROVAL_* are deliberately NOT
	// retryable: an agent must never be told to repeat a denied action, and a
	// broken policy or a missing approval needs an operator, not a retry.
	CodeAuthzUnavailable: true,
}

// Retryable reports whether retrying an operation that failed with this code
// could plausibly succeed without a code or configuration change. Unknown
// codes are never reported retryable — a caller that cannot classify the
// failure must not be told to blindly repeat it.
func Retryable(code Code) bool {
	return retryableCodes[code]
}
