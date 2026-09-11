package project

import (
	"fmt"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// TestLoadRolloutConfig covers the canary/progressive configuration surface:
// valid plans parse (percent suffix or bare number), and every unsafe or
// malformed plan is rejected at load time with an error naming the yaml path.
func TestLoadRolloutConfig(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		want    func(*Config) error // nil for valid configs
		wantErr string
	}{
		{
			name: "progressive plan with percent-suffixed traffic",
			yaml: `
deploy:
  strategy: progressive
  rollout:
    steps:
      - traffic: 5%
        duration: 2m
      - traffic: 25%
        duration: 5m
      - traffic: 50%
        duration: 5m
      - traffic: 100%
    verification:
      interval: 5s
      max_error_rate: 5
      max_error_delta: 2
      max_p95_factor: 3
`,
			want: func(c *Config) error {
				r := c.Deploy.Rollout
				if len(r.Steps) != 4 {
					return fmt.Errorf("steps = %d, want 4", len(r.Steps))
				}
				wantPct := []int{5, 25, 50, 100}
				for i, s := range r.Steps {
					if int(s.Traffic) != wantPct[i] {
						return fmt.Errorf("step %d traffic = %d, want %d", i, s.Traffic, wantPct[i])
					}
				}
				if r.Steps[0].Duration != "2m" {
					return fmt.Errorf("step 0 duration = %q", r.Steps[0].Duration)
				}
				if r.Verification == nil || r.Verification.MaxP95Factor != 3 {
					return fmt.Errorf("verification = %+v", r.Verification)
				}
				return nil
			},
		},
		{
			name: "progressive plan with bare-number traffic",
			yaml: `
deploy:
  strategy: progressive
  rollout:
    steps:
      - traffic: 10
        duration: 30s
      - traffic: 100
`,
			want: func(c *Config) error {
				if c.Deploy.Rollout.Steps[0].Traffic != 10 {
					return fmt.Errorf("traffic = %d, want 10", c.Deploy.Rollout.Steps[0].Traffic)
				}
				return nil
			},
		},
		{
			name: "canary config with share and duration",
			yaml: `
deploy:
  strategy: canary
  rollout:
    canary: 5%
    duration: 2m
`,
			want: func(c *Config) error {
				r := c.Deploy.Rollout
				if r.Canary == nil || *r.Canary != 5 {
					return fmt.Errorf("canary = %v, want 5", r.Canary)
				}
				if r.Duration != "2m" {
					return fmt.Errorf("duration = %q", r.Duration)
				}
				return nil
			},
		},
		{
			name:    "progressive without steps is rejected",
			yaml:    "deploy:\n  strategy: progressive\n",
			wantErr: "requires a deploy.rollout.steps",
		},
		{
			name: "steps without a rollout strategy are rejected",
			yaml: `
deploy:
  strategy: blue-green
  rollout:
    steps:
      - traffic: 5%
      - traffic: 100%
`,
			wantErr: "deploy.rollout requires deploy.strategy: canary or progressive",
		},
		{
			name: "canary share on progressive is rejected",
			yaml: `
deploy:
  strategy: progressive
  rollout:
    canary: 5%
    steps:
      - traffic: 5%
      - traffic: 100%
`,
			wantErr: "deploy.rollout.canary requires deploy.strategy: canary",
		},
		{
			name: "rollout without a rollout strategy is rejected",
			yaml: `
deploy:
  strategy: rolling
  rollout:
    duration: 1m
`,
			wantErr: "requires deploy.strategy: canary or progressive",
		},
		{
			name: "traffic above 100 is rejected",
			yaml: `
deploy:
  strategy: progressive
  rollout:
    steps:
      - traffic: 150%
      - traffic: 100%
`,
			wantErr: "invalid traffic 150%",
		},
		{
			name: "traffic of zero is rejected",
			yaml: `
deploy:
  strategy: progressive
  rollout:
    steps:
      - traffic: 0%
      - traffic: 100%
`,
			wantErr: "invalid traffic 0%",
		},
		{
			name: "non-increasing steps are rejected",
			yaml: `
deploy:
  strategy: progressive
  rollout:
    steps:
      - traffic: 25%
      - traffic: 25%
      - traffic: 100%
`,
			wantErr: "traffic shares must strictly increase",
		},
		{
			name: "decreasing steps are rejected",
			yaml: `
deploy:
  strategy: progressive
  rollout:
    steps:
      - traffic: 50%
      - traffic: 25%
      - traffic: 100%
`,
			wantErr: "traffic shares must strictly increase",
		},
		{
			name: "missing final 100 percent step is rejected",
			yaml: `
deploy:
  strategy: progressive
  rollout:
    steps:
      - traffic: 5%
        duration: 2m
      - traffic: 50%
        duration: 2m
`,
			wantErr: "must end with the 100%",
		},
		{
			name: "single 100 percent step is rejected as not a rollout",
			yaml: `
deploy:
  strategy: progressive
  rollout:
    steps:
      - traffic: 100%
`,
			wantErr: "at least one canary step",
		},
		{
			name: "fractional traffic is rejected",
			yaml: `
deploy:
  strategy: progressive
  rollout:
    steps:
      - traffic: 5.5%
      - traffic: 100%
`,
			wantErr: "unparseable traffic value",
		},
		{
			name: "invalid step duration is rejected",
			yaml: `
deploy:
  strategy: progressive
  rollout:
    steps:
      - traffic: 5%
        duration: two minutes
      - traffic: 100%
`,
			wantErr: "invalid duration",
		},
		{
			name: "negative step duration is rejected",
			yaml: `
deploy:
  strategy: progressive
  rollout:
    steps:
      - traffic: 5%
        duration: -1m
      - traffic: 100%
`,
			wantErr: "must not be negative",
		},
		{
			name: "canary share above 99 is rejected",
			yaml: `
deploy:
  strategy: canary
  rollout:
    canary: 100%
`,
			wantErr: "between 1% and 99%",
		},
		{
			name: "invalid verification interval is rejected",
			yaml: `
deploy:
  strategy: canary
  rollout:
    canary: 5%
    verification:
      interval: soon
`,
			wantErr: "verification.interval",
		},
		{
			name: "p95 factor below 1 is rejected",
			yaml: `
deploy:
  strategy: canary
  rollout:
    canary: 5%
    verification:
      max_p95_factor: 0.5
`,
			wantErr: "max_p95_factor must be >= 1",
		},
		{
			name: "error rate above 100 is rejected",
			yaml: `
deploy:
  strategy: canary
  rollout:
    canary: 5%
    verification:
      max_error_rate: 150
`,
			wantErr: "max_error_rate must be between 0 and 100",
		},
		{
			name: "replicas cannot be combined with canary",
			yaml: `
deploy:
  strategy: canary
  replicas: 2
  rollout:
    canary: 5%
`,
			wantErr: "deploy.replicas requires deploy.strategy: rolling",
		},
		{
			name:    "unknown strategy still rejected",
			yaml:    "deploy:\n  strategy: surge\n",
			wantErr: "invalid deploy.strategy",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := write(t, tc.yaml)
			cfg, err := Load(dir)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got cfg %+v", tc.wantErr, cfg)
				}
				if phelixerr.CodeOf(err) != phelixerr.CodeConfiguration {
					t.Errorf("code = %s, want CONFIGURATION_ERROR", phelixerr.CodeOf(err))
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.want != nil {
				if err := tc.want(cfg); err != nil {
					t.Error(err)
				}
			}
		})
	}
}

// TestRolloutConfigSaveRoundTrip guards Save/Load symmetry for the new types:
// a marshalled Percent must reload as the same value.
func TestRolloutConfigSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	pct := Percent(25)
	cfg := &Config{
		Name: "api",
		Deploy: &DeployConfig{
			Strategy: StrategyProgressive,
			Rollout: &RolloutConfig{
				Steps: []RolloutStepConfig{
					{Traffic: pct, Duration: "2m"},
					{Traffic: 100},
				},
				Verification: &RolloutVerificationConfig{Interval: "5s", MaxErrorDelta: 3},
			},
		},
	}
	if err := Save(dir, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Deploy.Strategy != StrategyProgressive || got.Deploy.Rollout == nil {
		t.Fatalf("deploy = %+v", got.Deploy)
	}
	if got.Deploy.Rollout.Steps[0].Traffic != 25 || got.Deploy.Rollout.Steps[1].Traffic != 100 {
		t.Errorf("steps = %+v", got.Deploy.Rollout.Steps)
	}
}
