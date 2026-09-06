package health

import (
	"net/url"
	"sort"
	"time"
)

// HealthCheckConfig represents the configuration for a single health check endpoint
type HealthCheckConfig struct {
	// ID is the stable identity of this endpoint, minted once by
	// ConfigManager.SaveConfig and preserved across every later edit, agent
	// restart and backend reconnect. It is what the backend keys its endpoint
	// records on — Name is a display label that a future rename may change, and
	// URL changes whenever the app's port does.
	ID            string `json:"id"`
	Name          string `json:"name"`
	URL           string `json:"url"`
	Interval      string `json:"interval"` // e.g., "10s", "1m"
	Retries       int    `json:"retries"`
	ExpectedCodes string `json:"expectedCodes"` // e.g., "200-299"
	Timeout       string `json:"timeout"`       // e.g., "5s"
}

// HealthCheckResult represents a single health check result
type HealthCheckResult struct {
	AppID            string    `json:"app_id"`
	AppName          string    `json:"app_name"`
	EndpointName     string    `json:"endpoint_name"`
	URL              string    `json:"url"`
	Status           string    `json:"status"` // UP, DOWN, TIMEOUT
	StatusCode       *int      `json:"status_code"`
	LatencyMs        *int64    `json:"latency_ms"`
	CheckedAt        time.Time `json:"checked_at"`
	Error            *string   `json:"error"`
	ConsecutiveFails int       `json:"-"`
}

// AppHealthConfig represents all health check endpoints for an app
type AppHealthConfig struct {
	AppID     string                        `json:"app_id"`
	AppName   string                        `json:"app_name"`
	Endpoints map[string]*HealthCheckConfig `json:"endpoints"` // key is endpoint name
	Enabled   bool                          `json:"enabled"`
	UpdatedAt time.Time                     `json:"updated_at"`

	// DeployTier configures the tiered health check used during zero-downtime
	// deploys (blue-green / rolling). It is read by internal/deploy via
	// internal/health/tiered.go. nil means "auto-detect at deploy time".
	DeployTier *DeployTierConfig `json:"deploy_tier,omitempty"`

	// HTTPMetrics opts into read-only reverse-proxy metrics. nil means the
	// feature is disabled for this application.
	HTTPMetrics *HTTPMetricsConfig `json:"http_metrics,omitempty"`
}

// EffectiveDeployTier returns the deploy-tier health config for this app.
//
// It is the single resolution point shared by blue-green and rolling deploys:
// an explicitly persisted DeployTier wins; otherwise one is derived from the
// persisted endpoints — the same data `phelix health list/status` display — so
// an app with a configured localhost endpoint is never treated as
// "no health endpoint configured" just because the deploy_tier block is
// missing (older configs, or endpoints added via `health add`). Only
// loopback endpoints can be bridged: the deploy probe targets the new
// instance's own port, so a remote URL says nothing about that instance.
// Returns nil when nothing usable is configured (genuine Tier 2/3 fallback).
func (c *AppHealthConfig) EffectiveDeployTier() *DeployTierConfig {
	if c == nil {
		return nil
	}
	if c.DeployTier != nil {
		return c.DeployTier
	}

	derive := func(ep *HealthCheckConfig) *DeployTierConfig {
		u, err := url.Parse(ep.URL)
		if err != nil || u.Path == "" || u.Path == "/" {
			return nil
		}
		host := u.Hostname()
		if host != "localhost" && host != "127.0.0.1" && host != "::1" {
			return nil
		}
		// Only Mode/Path: endpoint Interval/Retries/Timeout describe the
		// monitoring daemon's cadence (10s checks), which would make the
		// deploy deadline impossible (see cmd/health.go). Deploy defaults
		// (1s interval, 5 successes, 30s deadline) apply instead.
		return &DeployTierConfig{
			Mode: TierModeAuto,
			Path: u.Path,
		}
	}

	if ep, ok := c.Endpoints["default"]; ok {
		if cfg := derive(ep); cfg != nil {
			return cfg
		}
	}
	for _, name := range sortedEndpointNames(c.Endpoints) {
		if cfg := derive(c.Endpoints[name]); cfg != nil {
			return cfg
		}
	}
	return nil
}

// sortedEndpointNames gives deterministic iteration order for the endpoint
// map, so bridged configs are stable across runs.
func sortedEndpointNames(m map[string]*HealthCheckConfig) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// DeployTierMode selects which health-check tier the deploy flow uses.
//
// The deploy flow cannot assume every app exposes a /health endpoint, so the
// tier system falls back from "explicit HTTP endpoint" down to "process is
// alive". See SelectTier in tiered.go for the selection rules.
type DeployTierMode string

const (
	// TierModeAuto auto-detects: explicit path -> Tier1, else HTTP probe ->
	// Tier2, else TCP -> Tier3. This is the default.
	TierModeAuto DeployTierMode = "auto"
	// TierModeHTTP forces Tier 1: require 2xx on the configured path.
	TierModeHTTP DeployTierMode = "http"
	// TierModeTCPOnly forces Tier 3 TCP: only net.Dial the port.
	TierModeTCPOnly DeployTierMode = "tcp-only"
	// TierModeNone forces Tier 3 None: only check the PID is alive (no network).
	TierModeNone DeployTierMode = "none"
)

// DeployTierConfig configures the tiered deploy-time health check for an app.
type DeployTierConfig struct {
	// Mode selects the tier or "auto" for selection logic.
	Mode DeployTierMode `json:"mode"`
	// Path is the explicit health endpoint path (e.g. /health). Selects Tier 1
	// when non-empty (and Mode is auto/http).
	Path string `json:"path,omitempty"`
	// Interval between consecutive probes during the deploy health-check window.
	// Defaults to 1s.
	Interval string `json:"interval,omitempty"`
	// Retries is the number of consecutive successful probes required before
	// an instance is considered healthy. Defaults to 5.
	Retries int `json:"retries,omitempty"`
	// Timeout is the overall deadline for the instance to become healthy.
	// Defaults to 30s.
	Timeout string `json:"timeout,omitempty"`
}

// HealthCheckHistory represents stored health check history
type HealthCheckHistory struct {
	EndpointName string              `json:"endpoint_name"`
	Results      []HealthCheckResult `json:"results"` // Ring buffer, max 100
	LastChecked  time.Time           `json:"last_checked"`
}

// AutoRestartRecord tracks restart events
type AutoRestartRecord struct {
	AppID   string `json:"app_id"`
	AppName string `json:"app_name"`
	// EndpointID / EndpointName attribute the restart to the endpoint whose
	// failures triggered it, so the backend can join it against the endpoint it
	// already holds instead of parsing Reason.
	EndpointID         string    `json:"endpoint_id,omitempty"`
	EndpointName       string    `json:"endpoint_name,omitempty"`
	Reason             string    `json:"reason"`
	ExitCode           int       `json:"exit_code"`
	BackoffNextSeconds int       `json:"backoff_next_seconds"`
	RestartedAt        time.Time `json:"restarted_at"`
	CrashCount24h      int       `json:"crash_count_24h"`
}

// EndpointState tracks runtime state of an endpoint
type EndpointState struct {
	LastResult          *HealthCheckResult
	ConsecutiveFailures int
	LastFailTime        time.Time
	LastSuccessTime     time.Time
	BackoffLevel        int // 0 = no backoff, 1 = 5s, 2 = 15s, etc.
	NextRestartTime     time.Time
	CrashHistory        []time.Time // timestamps of crashes in last 24h
}
