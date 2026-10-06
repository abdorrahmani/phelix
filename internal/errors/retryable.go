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
}

// Retryable reports whether retrying an operation that failed with this code
// could plausibly succeed without a code or configuration change. Unknown
// codes are never reported retryable — a caller that cannot classify the
// failure must not be told to blindly repeat it.
func Retryable(code Code) bool {
	return retryableCodes[code]
}
