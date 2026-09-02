package buildreport

import "time"

// Config holds the regression thresholds and history window used by Analyze.
// Values live here as named constants/fields rather than scattered magic
// numbers; no user-facing CLI flags exist for them by design.
type Config struct {
	// HistorySize is the regression window: how many previous comparable
	// builds (newest first) are considered.
	HistorySize int

	// BinarySizeAbsoluteThreshold is the minimum absolute binary growth in
	// bytes for a size regression alert.
	BinarySizeAbsoluteThreshold int64
	// BinarySizePercentThreshold is the minimum relative binary growth in
	// percent for a size regression alert.
	BinarySizePercentThreshold float64

	// DurationPercentThreshold is the minimum relative duration increase in
	// percent for a time regression alert.
	DurationPercentThreshold float64
	// DurationAbsoluteThreshold floors duration alerts so noisy micro-changes
	// (e.g. +0.4s) never trigger warnings even at high percentages.
	DurationAbsoluteThreshold time.Duration
}

// DefaultConfig returns the built-in regression configuration:
//
//   - last 5 comparable builds form the history window,
//   - binary alerts require ≥1 MB absolute OR ≥5% growth,
//   - duration alerts require ≥25% growth AND ≥2s absolute slowdown.
func DefaultConfig() Config {
	return Config{
		HistorySize:                 DefaultHistorySize,
		BinarySizeAbsoluteThreshold: DefaultBinarySizeAbsoluteThreshold,
		BinarySizePercentThreshold:  DefaultBinarySizePercentThreshold,
		DurationPercentThreshold:    DefaultDurationPercentThreshold,
		DurationAbsoluteThreshold:   DefaultDurationAbsoluteThreshold,
	}
}

const (
	DefaultHistorySize = 5

	DefaultBinarySizeAbsoluteThreshold = int64(1024 * 1024) // 1 MB
	DefaultBinarySizePercentThreshold  = 5.0                // +5%

	DefaultDurationPercentThreshold  = 25.0            // +25%
	DefaultDurationAbsoluteThreshold = 2 * time.Second // 2s floor
)
