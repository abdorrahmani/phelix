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
		ScaleUpWindows: 3, ScaleDownWindows: 3,
		ScaleUpCooldown: 60 * time.Second, ScaleDownCooldown: 60 * time.Second,
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
		ScaleUpWindows: 1, ScaleDownWindows: 1,
		ScaleUpCooldown: 60 * time.Second, ScaleDownCooldown: 60 * time.Second,
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

func TestAutoscalingMetricsBlock(t *testing.T) {
	content := `name: api
deploy:
  strategy: rolling
  replicas: 2
  autoscaling:
    enabled: true
    min_replicas: 2
    max_replicas: 10
    interval: 15s
    metrics:
      cpu:
        scale_up: 65
        scale_down: 30
      memory:
        scale_up: 80
        scale_down: 40
      requests_per_second:
        scale_up: 500
        scale_down: 50
      latency:
        scale_up_p95: 200ms
        scale_down_p95: 80ms
        scale_up_p99: 500ms
        scale_down_p99: 200ms
    scale_up:
      evaluation_windows: 3
      cooldown: 60s
    scale_down:
      evaluation_windows: 5
      cooldown: 180s
      stabilization_window: 300s
`
	cfg, err := Load(write(t, content))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s := cfg.Deploy.Autoscaling.Resolve(2)
	if !s.Enabled || s.MinReplicas != 2 || s.MaxReplicas != 10 {
		t.Errorf("enabled/min/max = %+v", s)
	}
	if s.ScaleUpCPU != 65 || s.ScaleDownCPU != 30 {
		t.Errorf("cpu = %v/%v", s.ScaleUpCPU, s.ScaleDownCPU)
	}
	if s.ScaleUpMemory != 80 || s.ScaleDownMemory != 40 {
		t.Errorf("memory = %v/%v", s.ScaleUpMemory, s.ScaleDownMemory)
	}
	if s.ScaleUpRPS != 500 || s.ScaleDownRPS != 50 {
		t.Errorf("rps = %v/%v", s.ScaleUpRPS, s.ScaleDownRPS)
	}
	if s.ScaleUpP95 != 200*time.Millisecond || s.ScaleDownP95 != 80*time.Millisecond {
		t.Errorf("p95 = %v/%v", s.ScaleUpP95, s.ScaleDownP95)
	}
	if s.ScaleUpP99 != 500*time.Millisecond || s.ScaleDownP99 != 200*time.Millisecond {
		t.Errorf("p99 = %v/%v", s.ScaleUpP99, s.ScaleDownP99)
	}
	if s.ScaleUpWindows != 3 || s.ScaleDownWindows != 5 {
		t.Errorf("windows = %d/%d", s.ScaleUpWindows, s.ScaleDownWindows)
	}
	if s.ScaleUpCooldown != 60*time.Second || s.ScaleDownCooldown != 180*time.Second {
		t.Errorf("cooldowns = %v/%v", s.ScaleUpCooldown, s.ScaleDownCooldown)
	}
	if s.StabilizationWindow != 300*time.Second {
		t.Errorf("stabilization = %v", s.StabilizationWindow)
	}
	if s.Cooldown != 60*time.Second || s.EvaluationWindows != 1 {
		t.Errorf("legacy fields should keep top-level/defaults: cooldown %v windows %d", s.Cooldown, s.EvaluationWindows)
	}
}

func TestAutoscalingMetricsOverridesLegacy(t *testing.T) {
	content := `name: api
deploy:
  strategy: rolling
  autoscaling:
    enabled: true
    cpu:
      scale_up: 90
      scale_down: 10
    metrics:
      cpu:
        scale_up: 60
        scale_down: 20
`
	cfg, err := Load(write(t, content))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s := cfg.Deploy.Autoscaling.Resolve(2)
	if s.ScaleUpCPU != 60 || s.ScaleDownCPU != 20 {
		t.Errorf("metrics.cpu should override legacy cpu: %v/%v", s.ScaleUpCPU, s.ScaleDownCPU)
	}
}

func TestAutoscalingLatencyP99Optional(t *testing.T) {
	content := `name: api
deploy:
  strategy: rolling
  autoscaling:
    enabled: true
    metrics:
      latency:
        scale_up_p95: 300ms
        scale_down_p95: 100ms
`
	cfg, err := Load(write(t, content))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s := cfg.Deploy.Autoscaling.Resolve(2)
	if s.ScaleUpP95 != 300*time.Millisecond || s.ScaleDownP95 != 100*time.Millisecond {
		t.Errorf("p95 = %v/%v", s.ScaleUpP95, s.ScaleDownP95)
	}
	if s.ScaleUpP99 != 0 || s.ScaleDownP99 != 0 {
		t.Errorf("p99 should stay unset: %v/%v", s.ScaleUpP99, s.ScaleDownP99)
	}
}

func TestLoadAutoscalingMetricsInvalid(t *testing.T) {
	roll := "name: api\ndeploy:\n  strategy: rolling\n  replicas: 2\n  autoscaling:\n    enabled: true\n"
	cases := []struct {
		name    string
		content string
		wantErr string
	}{
		{"memory down at up", roll + "    metrics:\n      memory:\n        scale_up: 80\n        scale_down: 80\n", "metrics.memory.scale_down (80) must be below"},
		{"memory up over 100", roll + "    metrics:\n      memory:\n        scale_up: 120\n", "metrics.memory.scale_up must be between 0 and 100"},
		{"rps negative", roll + "    metrics:\n      requests_per_second:\n        scale_up: -1\n", "requests_per_second thresholds must not be negative"},
		{"rps down at up", roll + "    metrics:\n      requests_per_second:\n        scale_up: 100\n        scale_down: 100\n", "requests_per_second.scale_down (100) must be below"},
		{"p99 down above up", roll + "    metrics:\n      latency:\n        scale_up_p99: 100ms\n        scale_down_p99: 200ms\n", "metrics.latency.scale_down_p99 (200ms) must be below"},
		{"metrics p95 unparseable", roll + "    metrics:\n      latency:\n        scale_up_p95: quick\n", "metrics.latency.scale_up_p95 has invalid duration"},
		{"scale_up windows negative", roll + "    scale_up:\n      evaluation_windows: -2\n", "invalid deploy.autoscaling.scale_up.evaluation_windows -2"},
		{"scale_down cooldown unparseable", roll + "    scale_down:\n      cooldown: later\n", "deploy.autoscaling.scale_down.cooldown"},
		{"stabilization unparseable", roll + "    scale_down:\n      stabilization_window: soon\n", "deploy.autoscaling.scale_down.stabilization_window"},
		{"stabilization negative", roll + "    scale_down:\n      stabilization_window: -5s\n", "deploy.autoscaling.scale_down.stabilization_window"},
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

func TestAutoscalingWorkerAllowed(t *testing.T) {
	content := `name: jobs
type: worker
deploy:
  strategy: classic
  autoscaling:
    enabled: true
    min_replicas: 1
    max_replicas: 4
`
	cfg, err := Load(write(t, content))
	if err != nil {
		t.Fatalf("Load rejected autoscaling on a worker: %v", err)
	}
	s := cfg.Deploy.Autoscaling.Resolve(1)
	if !s.Enabled || s.MinReplicas != 1 || s.MaxReplicas != 4 {
		t.Errorf("Resolve = %+v", s)
	}
}
