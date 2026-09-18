package project

import (
	"strings"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

const validAutoscalingYAML = `
name: api
port: 3000
deploy:
  strategy: rolling
  replicas: 2
  autoscaling:
    enabled: true
    min_replicas: 2
    max_replicas: 8
    interval: 15s
    cooldown: 60s
    cpu:
      scale_up: 70
      scale_down: 30
    latency:
      scale_up_p95: 500ms
      scale_down_p95: 150ms
    evaluation_windows: 3
`

func TestLoadAutoscalingValid(t *testing.T) {
	cfg, err := Load(write(t, validAutoscalingYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a := cfg.Deploy.Autoscaling
	if a == nil {
		t.Fatal("autoscaling block not parsed")
	}
	if !a.Enabled || a.MinReplicas != 2 || a.MaxReplicas != 8 {
		t.Errorf("enabled/min/max = %+v", a)
	}
	if a.Interval != "15s" || a.Cooldown != "60s" {
		t.Errorf("interval/cooldown = %q/%q", a.Interval, a.Cooldown)
	}
	if a.CPU == nil || a.CPU.ScaleUp != 70 || a.CPU.ScaleDown != 30 {
		t.Errorf("cpu thresholds = %+v", a.CPU)
	}
	if a.Latency == nil || a.Latency.ScaleUpP95 != "500ms" || a.Latency.ScaleDownP95 != "150ms" {
		t.Errorf("latency thresholds = %+v", a.Latency)
	}
	if a.EvaluationWindows != 3 {
		t.Errorf("evaluation_windows = %d", a.EvaluationWindows)
	}

	s := a.Resolve(cfg.Deploy.Replicas)
	want := AutoscaleSettings{
		Enabled: true, MinReplicas: 2, MaxReplicas: 8,
		Interval: 15 * time.Second, Cooldown: 60 * time.Second,
		ScaleUpCPU: 70, ScaleDownCPU: 30,
		ScaleUpP95: 500 * time.Millisecond, ScaleDownP95: 150 * time.Millisecond,
		EvaluationWindows: 3,
	}
	if s != want {
		t.Errorf("Resolve = %+v, want %+v", s, want)
	}
}

func TestAutoscalingResolveDefaults(t *testing.T) {
	cfg, err := Load(write(t, "name: api\ndeploy:\n  strategy: rolling\n  replicas: 4\n  autoscaling:\n    enabled: true\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s := cfg.Deploy.Autoscaling.Resolve(cfg.Deploy.Replicas)
	want := AutoscaleSettings{
		Enabled: true, MinReplicas: 1, MaxReplicas: 4,
		Interval: 15 * time.Second, Cooldown: 60 * time.Second,
		ScaleUpCPU: 70, ScaleDownCPU: 30,
		ScaleUpP95: 500 * time.Millisecond, ScaleDownP95: 150 * time.Millisecond,
		EvaluationWindows: 1,
	}
	if s != want {
		t.Errorf("Resolve defaults = %+v, want %+v", s, want)
	}

	// min_replicas set above replicas: the range clamps to min, never below it.
	cfg, err = Load(write(t, "name: api\ndeploy:\n  strategy: rolling\n  autoscaling:\n    enabled: true\n    min_replicas: 3\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s := cfg.Deploy.Autoscaling.Resolve(2); s.MinReplicas != 3 || s.MaxReplicas != 3 {
		t.Errorf("Resolve min-clamp = min %d max %d, want 3/3", s.MinReplicas, s.MaxReplicas)
	}

	// No replicas declared: the range collapses to [1,1] instead of widening.
	cfg, err = Load(write(t, "name: api\ndeploy:\n  strategy: rolling\n  autoscaling:\n    enabled: true\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s := cfg.Deploy.Autoscaling.Resolve(0); s.MinReplicas != 1 || s.MaxReplicas != 1 {
		t.Errorf("Resolve no-replicas = min %d max %d, want 1/1", s.MinReplicas, s.MaxReplicas)
	}

	// A nil block resolves to zero settings: the engine reads that as disabled.
	var missing *AutoscalingConfig
	if s := missing.Resolve(2); s.Enabled || s.MinReplicas != 0 {
		t.Errorf("nil Resolve = %+v, want zero settings", s)
	}
}

func TestLoadAutoscalingInvalid(t *testing.T) {
	roll := "name: api\ndeploy:\n  strategy: rolling\n  replicas: 2\n"
	cases := []struct {
		name    string
		content string
		wantErr string
	}{
		{"min above max", roll + "  autoscaling:\n    enabled: true\n    min_replicas: 5\n    max_replicas: 2\n", "deploy.autoscaling.min_replicas 5 exceeds max_replicas 2"},
		{"negative min", roll + "  autoscaling:\n    enabled: true\n    min_replicas: -1\n", "invalid deploy.autoscaling.min_replicas -1"},
		{"negative max", roll + "  autoscaling:\n    enabled: true\n    max_replicas: -2\n", "invalid deploy.autoscaling.max_replicas -2"},
		{"negative evaluation windows", roll + "  autoscaling:\n    enabled: true\n    evaluation_windows: -1\n", "invalid deploy.autoscaling.evaluation_windows -1"},
		{"unparseable interval", roll + "  autoscaling:\n    enabled: true\n    interval: abc\n", "deploy.autoscaling.interval"},
		{"negative interval", roll + "  autoscaling:\n    enabled: true\n    interval: -5s\n", "deploy.autoscaling.interval"},
		{"unparseable cooldown", roll + "  autoscaling:\n    enabled: true\n    cooldown: soon\n", "deploy.autoscaling.cooldown"},
		{"cpu scale_up over 100", roll + "  autoscaling:\n    enabled: true\n    cpu:\n      scale_up: 150\n", "cpu.scale_up must be between 0 and 100"},
		{"cpu scale_down negative", roll + "  autoscaling:\n    enabled: true\n    cpu:\n      scale_down: -5\n", "cpu.scale_down must be between 0 and 100"},
		{"cpu scale_down at scale_up", roll + "  autoscaling:\n    enabled: true\n    cpu:\n      scale_up: 70\n      scale_down: 70\n", "cpu.scale_down (70) must be below cpu.scale_up (70)"},
		{"latency up unparseable", roll + "  autoscaling:\n    enabled: true\n    latency:\n      scale_up_p95: fast\n", "latency.scale_up_p95 has invalid duration"},
		{"latency down negative", roll + "  autoscaling:\n    enabled: true\n    latency:\n      scale_down_p95: -150ms\n", "latency.scale_down_p95 must not be negative"},
		{"latency down above up", roll + "  autoscaling:\n    enabled: true\n    latency:\n      scale_up_p95: 100ms\n      scale_down_p95: 200ms\n", "latency.scale_down_p95 (200ms) must be below latency.scale_up_p95 (100ms)"},
		{"enabled on blue-green", "name: api\ndeploy:\n  strategy: blue-green\n  autoscaling:\n    enabled: true\n", "deploy.autoscaling requires deploy.strategy: rolling"},
		{"enabled without strategy", "name: api\ndeploy:\n  autoscaling:\n    enabled: true\n", "deploy.autoscaling requires deploy.strategy: rolling"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(write(t, tc.content))
			if err == nil {
				t.Fatalf("Load accepted invalid autoscaling config")
			}
			if got := phelixerr.CodeOf(err); got != phelixerr.CodeConfiguration {
				t.Errorf("error code = %v, want %v", got, phelixerr.CodeConfiguration)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestAutoscalingDisabledBlock(t *testing.T) {
	// A disabled block on a non-rolling deploy is inert, not an error; only
	// structural nonsense (min>max) still fails regardless of enabled.
	cfg, err := Load(write(t, "name: api\ndeploy:\n  strategy: classic\n  autoscaling:\n    enabled: false\n    min_replicas: 2\n    max_replicas: 4\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s := cfg.Deploy.Autoscaling.Resolve(2); s.Enabled {
		t.Errorf("Resolve = %+v, want disabled", s)
	}
	if _, err := Load(write(t, "name: api\ndeploy:\n  strategy: classic\n  autoscaling:\n    enabled: false\n    min_replicas: 5\n    max_replicas: 2\n")); err == nil {
		t.Error("Load accepted min_replicas > max_replicas on a disabled block")
	}
}

func TestAutoscalingAbsentUnchanged(t *testing.T) {
	// Apps without the block keep behaving exactly as before.
	cfg, err := Load(write(t, "name: api\ndeploy:\n  strategy: rolling\n  replicas: 3\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Deploy.Autoscaling != nil {
		t.Errorf("autoscaling = %+v, want nil", cfg.Deploy.Autoscaling)
	}
	if cfg.Deploy.Replicas != 3 || cfg.Deploy.Strategy != StrategyRolling {
		t.Errorf("deploy = %+v", cfg.Deploy)
	}
}
