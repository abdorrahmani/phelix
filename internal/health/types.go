package health

import "time"

// HealthCheckConfig represents the configuration for a single health check endpoint
type HealthCheckConfig struct {
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
}

// HealthCheckHistory represents stored health check history
type HealthCheckHistory struct {
	EndpointName string              `json:"endpoint_name"`
	Results      []HealthCheckResult `json:"results"` // Ring buffer, max 100
	LastChecked  time.Time           `json:"last_checked"`
}

// AutoRestartRecord tracks restart events
type AutoRestartRecord struct {
	AppID              string    `json:"app_id"`
	AppName            string    `json:"app_name"`
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
