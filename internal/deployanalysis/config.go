package deployanalysis

import (
	"time"

	"github.com/abdorrahmani/phelix/internal/project"
)

// Config holds the observation windows and regression thresholds used by the
// analysis engine. Defaults are deliberately conservative: an analysis never
// declares a regression from a fluctuation smaller than the thresholds below.
type Config struct {
	// BaselineWindow is how far back from the deployment the stable baseline
	// is measured.
	BaselineWindow time.Duration
	// ObserveWindow is how long after a successful deployment the new version
	// is observed before the report is finalized.
	ObserveWindow time.Duration
	// MinSamples is the minimum number of resource samples (and, separately,
	// traffic samples) a window needs before a metric can be compared.
	MinSamples int

	ErrorRateMaxDelta        float64
	LatencyMaxPercent        float64
	CPUMaxPercent            float64
	MemoryMaxPercent         float64
	RestartMaxExtra          int
	ThroughputMaxDegradation float64
	HealthFailuresMaxExtra   int
}

// DefaultConfig returns the built-in analysis settings.
func DefaultConfig() Config {
	return Config{
		BaselineWindow:           5 * time.Minute,
		ObserveWindow:            2 * time.Minute,
		MinSamples:               5,
		ErrorRateMaxDelta:        1.0,
		LatencyMaxPercent:        20,
		CPUMaxPercent:            25,
		MemoryMaxPercent:         15,
		RestartMaxExtra:          0,
		ThroughputMaxDegradation: 20,
		HealthFailuresMaxExtra:   0,
	}
}

// FromProject overlays a phelix.yaml deploy.analysis block on the defaults.
// A nil config, nil block or invalid duration leaves the default in place so
// analysis can never be broken by project configuration.
func FromProject(cfg *project.Config) Config {
	out := DefaultConfig()
	if cfg == nil || cfg.Deploy == nil || cfg.Deploy.Analysis == nil {
		return out
	}
	a := cfg.Deploy.Analysis
	if d, err := time.ParseDuration(a.BaselineWindow); err == nil && d > 0 {
		out.BaselineWindow = d
	}
	if d, err := time.ParseDuration(a.ObserveWindow); err == nil && d > 0 {
		out.ObserveWindow = d
	}
	if a.MinSamples > 0 {
		out.MinSamples = a.MinSamples
	}
	if a.ErrorRateMaxDelta > 0 {
		out.ErrorRateMaxDelta = a.ErrorRateMaxDelta
	}
	if a.LatencyMaxPercent > 0 {
		out.LatencyMaxPercent = a.LatencyMaxPercent
	}
	if a.CPUMaxPercent > 0 {
		out.CPUMaxPercent = a.CPUMaxPercent
	}
	if a.MemoryMaxPercent > 0 {
		out.MemoryMaxPercent = a.MemoryMaxPercent
	}
	if a.RestartMaxExtra > 0 {
		out.RestartMaxExtra = a.RestartMaxExtra
	}
	if a.ThroughputMaxDegradation > 0 {
		out.ThroughputMaxDegradation = a.ThroughputMaxDegradation
	}
	if a.HealthFailuresMaxExtra > 0 {
		out.HealthFailuresMaxExtra = a.HealthFailuresMaxExtra
	}
	return out
}

// Disabled reports whether analysis is explicitly turned off.
func Disabled(cfg *project.Config) bool {
	return cfg != nil && cfg.Deploy != nil && cfg.Deploy.Analysis != nil &&
		cfg.Deploy.Analysis.Enabled != nil && !*cfg.Deploy.Analysis.Enabled
}
