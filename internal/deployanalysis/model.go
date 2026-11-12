// Package deployanalysis evaluates the impact of a successful deployment by
// comparing a baseline observation window against the traffic and resource
// measurements collected after the new version took over.
//
// Analysis is pure observability. It never changes a deployment decision, never
// triggers a rollback, and works fully offline: reports are written to the
// app's data directory and synchronized later when a session is available.
package deployanalysis

import "time"

// SchemaVersion is the on-disk and wire report schema. Readers must accept
// older versions and treat unknown fields as absent.
const SchemaVersion = 1

// Metric keys. They are stable identifiers used by the CLI, the backend and
// the dashboard; labels and units may change.
const (
	MetricErrorRate      = "error_rate"
	MetricP95Latency     = "p95_latency"
	MetricCPUUsage       = "cpu_usage"
	MetricMemoryUsage    = "memory_usage"
	MetricRestartCount   = "restart_count"
	MetricThroughput     = "throughput"
	MetricHealthFailures = "health_failures"
)

// Metric status values.
const (
	StatusNormal           = "normal"
	StatusRegression       = "regression"
	StatusImprovement      = "improvement"
	StatusInsufficientData = "insufficient_data"
	StatusNotAvailable     = "not_available"
)

// Deployment outcomes. A failed rollout is never recorded as promoted.
const (
	OutcomePromoted                = "promoted"
	OutcomePromotedWithRegressions = "promoted_with_regressions"
	OutcomeRejected                = "rejected"
	OutcomeRolledBack              = "rolled_back"
	OutcomeInsufficientData        = "insufficient_data"
)

// MetricComparison is one metric's before/after observation. Nil pointers mean
// the value could not be measured; they are never rendered as zero.
type MetricComparison struct {
	Key             string   `json:"key"`
	Label           string   `json:"label"`
	Unit            string   `json:"unit"`
	Status          string   `json:"status"`
	Before          *float64 `json:"before,omitempty"`
	After           *float64 `json:"after,omitempty"`
	Delta           *float64 `json:"delta,omitempty"`
	Percent         *float64 `json:"percent_delta,omitempty"`
	BeforePeak      *float64 `json:"before_peak,omitempty"`
	AfterPeak       *float64 `json:"after_peak,omitempty"`
	BaselineSamples int      `json:"baseline_samples,omitempty"`
	ObservedSamples int      `json:"observed_samples,omitempty"`
	SkipReason      string   `json:"skip_reason,omitempty"`
}

// Window describes one observation window in the report.
type Window struct {
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	Samples   int       `json:"samples"`
}

// RolloutVerification mirrors the canary/progressive step verdict the deploy
// engine already computed. Values are copied, never recomputed.
type RolloutVerification struct {
	CanaryErrorRate   float64 `json:"canary_error_rate"`
	BaselineErrorRate float64 `json:"baseline_error_rate"`
	CanaryP95MS       float64 `json:"canary_p95_ms"`
	BaselineP95MS     float64 `json:"baseline_p95_ms"`
	CanaryRequests    int64   `json:"canary_requests"`
	CanaryErrors      int64   `json:"canary_errors"`
	BaselineRequests  int64   `json:"baseline_requests"`
	Passed            bool    `json:"passed"`
	SkipReason        string  `json:"skip_reason,omitempty"`
}

// Rollout describes the canary/progressive context of a deployment.
type Rollout struct {
	Strategy     string               `json:"strategy"`
	Promoted     bool                 `json:"promoted"`
	TotalSteps   int                  `json:"total_steps"`
	FailedStep   int                  `json:"failed_step,omitempty"`
	FailureCode  string               `json:"failure_code,omitempty"`
	Reason       string               `json:"reason,omitempty"`
	Verification *RolloutVerification `json:"verification,omitempty"`
}

// Report is the complete deployment-impact assessment stored locally and
// synchronized to the backend.
type Report struct {
	SchemaVersion int    `json:"schema_version"`
	AnalysisID    string `json:"analysis_id"`
	DeploymentID  string `json:"deployment_id"`

	AppName  string `json:"app_name"`
	AppID    string `json:"app_id,omitempty"`
	ServerID string `json:"server_id,omitempty"`

	Strategy        string `json:"strategy"`
	PreviousVersion string `json:"previous_version,omitempty"`
	NewVersion      string `json:"new_version,omitempty"`

	Outcome        string `json:"outcome"`
	Status         string `json:"status"`
	Recommendation string `json:"recommendation"`

	ImpactScore float64 `json:"impact_score"`
	ImpactGrade string  `json:"impact_grade"`

	Baseline Window             `json:"baseline"`
	Observed Window             `json:"observed"`
	Metrics  []MetricComparison `json:"metrics"`
	Rollout  *Rollout           `json:"rollout,omitempty"`

	FailureCode string    `json:"failure_code,omitempty"`
	DeployedAt  time.Time `json:"deployed_at"`
	CompletedAt time.Time `json:"completed_at"`
	AnalyzedAt  time.Time `json:"analyzed_at"`
}

// Metric returns the metric with key, or nil.
func (r *Report) Metric(key string) *MetricComparison {
	if r == nil {
		return nil
	}
	for i := range r.Metrics {
		if r.Metrics[i].Key == key {
			return &r.Metrics[i]
		}
	}
	return nil
}
