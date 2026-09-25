package deploy

import (
	"strings"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/project"
)

// fakeClock is the injectable time source: no test sleeps on wall-clock time.
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// specSettings mirrors the documented example block (window 3, thresholds
// 70/30 and 500ms/150ms, min 2, max 8, cooldown 60s).
func specSettings() project.AutoscaleSettings {
	return project.AutoscaleSettings{
		Enabled: true, MinReplicas: 2, MaxReplicas: 8,
		Cooldown:          60 * time.Second,
		ScaleUpCPU:        70,
		ScaleDownCPU:      30,
		ScaleUpP95:        500 * time.Millisecond,
		ScaleDownP95:      150 * time.Millisecond,
		EvaluationWindows: 3,
	}
}

func newTestAutoscaler(s project.AutoscaleSettings) (*Autoscaler, *fakeClock) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	a := NewAutoscaler(s)
	a.now = clk.Now
	return a, clk
}

func mustDecide(t *testing.T, a *Autoscaler, m AutoscaleMetrics, current int) ScaleDecision {
	t.Helper()
	d := a.Evaluate(m, current)
	if d.CurrentReplicas != current {
		t.Fatalf("CurrentReplicas = %d, want %d", d.CurrentReplicas, current)
	}
	return d
}

func assertNone(t *testing.T, d ScaleDecision, wantReasonPart string) {
	t.Helper()
	if d.Direction != ScaleNone {
		t.Fatalf("direction = %v, want ScaleNone (decision: %+v)", d.Direction, d)
	}
	if d.DesiredReplicas != d.CurrentReplicas {
		t.Errorf("ScaleNone desired = %d, want %d", d.DesiredReplicas, d.CurrentReplicas)
	}
	if wantReasonPart != "" && !strings.Contains(d.Reason, wantReasonPart) {
		t.Errorf("reason = %q, want it to contain %q", d.Reason, wantReasonPart)
	}
}

func assertDecision(t *testing.T, d ScaleDecision, dir ScaleDirection, current, desired int) {
	t.Helper()
	if d.Direction != dir {
		t.Fatalf("direction = %v, want %v (decision: %+v)", d.Direction, dir, d)
	}
	if d.DesiredReplicas != desired {
		t.Errorf("desired = %d, want %d", d.DesiredReplicas, desired)
	}
	if d.Reason == "" {
		t.Error("decision carries no reason")
	}
}

// 1. CPU scale-up threshold (spec example: 75/78/74 with window 3 => up).
func TestAutoscaleCPUScaleUp(t *testing.T) {
	a, _ := newTestAutoscaler(specSettings())
	assertNone(t, mustDecide(t, a, AutoscaleMetrics{CPUPercent: 75}, 4), "1 of 3")
	assertNone(t, mustDecide(t, a, AutoscaleMetrics{CPUPercent: 78}, 4), "2 of 3")
	assertDecision(t, mustDecide(t, a, AutoscaleMetrics{CPUPercent: 74}, 4), ScaleUp, 4, 5)
}

// 2. CPU scale-down threshold.
func TestAutoscaleCPUScaleDown(t *testing.T) {
	a, _ := newTestAutoscaler(specSettings())
	for range 2 {
		assertNone(t, mustDecide(t, a, AutoscaleMetrics{CPUPercent: 20, P95Latency: 100 * time.Millisecond}, 4), "")
	}
	assertDecision(t, mustDecide(t, a, AutoscaleMetrics{CPUPercent: 22, P95Latency: 100 * time.Millisecond}, 4), ScaleDown, 4, 3)
}

// 3. Latency scale-up threshold.
func TestAutoscaleLatencyScaleUp(t *testing.T) {
	a, _ := newTestAutoscaler(specSettings())
	for range 2 {
		assertNone(t, mustDecide(t, a, AutoscaleMetrics{CPUPercent: 50, P95Latency: 600 * time.Millisecond}, 3), "")
	}
	assertDecision(t, mustDecide(t, a, AutoscaleMetrics{CPUPercent: 50, P95Latency: 600 * time.Millisecond}, 3), ScaleUp, 3, 4)
}

// 4. Latency scale-down threshold: p95 must be low enough to let an idle CPU
// count; a p95 above scale_down_p95 blocks the AND even when CPU is idle.
func TestAutoscaleLatencyScaleDown(t *testing.T) {
	a, _ := newTestAutoscaler(specSettings())
	for range 3 {
		assertNone(t, mustDecide(t, a, AutoscaleMetrics{CPUPercent: 25, P95Latency: 200 * time.Millisecond}, 4), "within thresholds")
	}

	a2, _ := newTestAutoscaler(specSettings())
	for range 2 {
		assertNone(t, mustDecide(t, a2, AutoscaleMetrics{CPUPercent: 25, P95Latency: 100 * time.Millisecond}, 4), "")
	}
	assertDecision(t, mustDecide(t, a2, AutoscaleMetrics{CPUPercent: 25, P95Latency: 100 * time.Millisecond}, 4), ScaleDown, 4, 3)
}

// 5. CPU OR latency for scale-up: either metric alone triggers the up rule.
func TestAutoscaleCPUOrLatencyScaleUp(t *testing.T) {
	latOnly, _ := newTestAutoscaler(specSettings())
	for range 2 {
		assertNone(t, mustDecide(t, latOnly, AutoscaleMetrics{CPUPercent: 50, P95Latency: 600 * time.Millisecond}, 4), "")
	}
	d := mustDecide(t, latOnly, AutoscaleMetrics{CPUPercent: 50, P95Latency: 600 * time.Millisecond}, 4)
	assertDecision(t, d, ScaleUp, 4, 5)
	if !strings.Contains(d.Reason, "p95") {
		t.Errorf("reason = %q, want the latency side of the OR named", d.Reason)
	}

	cpuOnly, _ := newTestAutoscaler(specSettings())
	for range 2 {
		assertNone(t, mustDecide(t, cpuOnly, AutoscaleMetrics{CPUPercent: 75, P95Latency: 100 * time.Millisecond}, 4), "")
	}
	d = mustDecide(t, cpuOnly, AutoscaleMetrics{CPUPercent: 75, P95Latency: 100 * time.Millisecond}, 4)
	assertDecision(t, d, ScaleUp, 4, 5)
	if !strings.Contains(d.Reason, "cpu") {
		t.Errorf("reason = %q, want the CPU side of the OR named", d.Reason)
	}
}

// 6. CPU AND latency for scale-down: both must hold at once.
func TestAutoscaleCPUAndLatencyScaleDown(t *testing.T) {
	highLat, _ := newTestAutoscaler(specSettings())
	for range 3 {
		assertNone(t, mustDecide(t, highLat, AutoscaleMetrics{CPUPercent: 20, P95Latency: 400 * time.Millisecond}, 4), "within thresholds")
	}

	highCPU, _ := newTestAutoscaler(specSettings())
	for range 3 {
		assertNone(t, mustDecide(t, highCPU, AutoscaleMetrics{CPUPercent: 50, P95Latency: 100 * time.Millisecond}, 4), "within thresholds")
	}

	both, _ := newTestAutoscaler(specSettings())
	for range 2 {
		assertNone(t, mustDecide(t, both, AutoscaleMetrics{CPUPercent: 20, P95Latency: 100 * time.Millisecond}, 4), "")
	}
	assertDecision(t, mustDecide(t, both, AutoscaleMetrics{CPUPercent: 20, P95Latency: 100 * time.Millisecond}, 4), ScaleDown, 4, 3)
}

// 7. min_replicas floor.
func TestAutoscaleMinReplicas(t *testing.T) {
	a, _ := newTestAutoscaler(specSettings())
	assertNone(t, mustDecide(t, a, AutoscaleMetrics{CPUPercent: 0}, 2), "1 of 3")
	assertNone(t, mustDecide(t, a, AutoscaleMetrics{CPUPercent: 0}, 2), "2 of 3")
	assertNone(t, mustDecide(t, a, AutoscaleMetrics{CPUPercent: 0}, 2), "min_replicas 2")
}

// 8. max_replicas ceiling.
func TestAutoscaleMaxReplicas(t *testing.T) {
	a, _ := newTestAutoscaler(specSettings())
	assertNone(t, mustDecide(t, a, AutoscaleMetrics{CPUPercent: 99}, 8), "1 of 3")
	assertNone(t, mustDecide(t, a, AutoscaleMetrics{CPUPercent: 99}, 8), "2 of 3")
	assertNone(t, mustDecide(t, a, AutoscaleMetrics{CPUPercent: 99}, 8), "max_replicas 8")
}

// 9. Exactly ±1 replica per decision, never a jump.
func TestAutoscaleExactlyOneReplicaPerDecision(t *testing.T) {
	s := specSettings()
	s.EvaluationWindows = 1
	s.Cooldown = 0

	a, _ := newTestAutoscaler(s)
	d := mustDecide(t, a, AutoscaleMetrics{CPUPercent: 90}, 4)
	assertDecision(t, d, ScaleUp, 4, 5)
	if d.DesiredReplicas-d.CurrentReplicas != 1 {
		t.Errorf("up delta = %d, want exactly 1", d.DesiredReplicas-d.CurrentReplicas)
	}

	a2, _ := newTestAutoscaler(s)
	d = mustDecide(t, a2, AutoscaleMetrics{CPUPercent: 10, P95Latency: 10 * time.Millisecond}, 4)
	assertDecision(t, d, ScaleDown, 4, 3)
	if d.CurrentReplicas-d.DesiredReplicas != 1 {
		t.Errorf("down delta = %d, want exactly 1", d.CurrentReplicas-d.DesiredReplicas)
	}

	// Even far past the bounds the desired count moves by one, never to the
	// bound itself.
	a3, _ := newTestAutoscaler(s)
	d = mustDecide(t, a3, AutoscaleMetrics{CPUPercent: 100}, 2)
	assertDecision(t, d, ScaleUp, 2, 3)
}

// 10. Evaluation window: the condition must persist for N consecutive
// samples before the decision fires.
func TestAutoscaleEvaluationWindow(t *testing.T) {
	a, _ := newTestAutoscaler(specSettings())
	for i := 1; i <= 2; i++ {
		d := mustDecide(t, a, AutoscaleMetrics{CPUPercent: 80}, 4)
		assertNone(t, d, "")
	}
	assertDecision(t, mustDecide(t, a, AutoscaleMetrics{CPUPercent: 80}, 4), ScaleUp, 4, 5)
}

// 11. Broken evaluation window (spec example: 75/68/74 => no scale).
func TestAutoscaleBrokenWindow(t *testing.T) {
	a, _ := newTestAutoscaler(specSettings())
	for _, cpu := range []float64{75, 68, 74} {
		d := mustDecide(t, a, AutoscaleMetrics{CPUPercent: cpu}, 4)
		assertNone(t, d, "")
	}
	// The 68% sample reset the window but 74 already started a fresh one, so
	// two more consecutive hot samples complete it.
	assertNone(t, mustDecide(t, a, AutoscaleMetrics{CPUPercent: 75}, 4), "2 of 3")
	assertDecision(t, mustDecide(t, a, AutoscaleMetrics{CPUPercent: 75}, 4), ScaleUp, 4, 5)
}

// 12. Cooldown: no second decision until cooldown elapsed, and windows earned
// during cooldown are discarded so the first post-cooldown decision needs a
// fresh window.
func TestAutoscaleCooldown(t *testing.T) {
	s := specSettings()
	s.EvaluationWindows = 1
	a, clk := newTestAutoscaler(s)

	assertDecision(t, mustDecide(t, a, AutoscaleMetrics{CPUPercent: 90}, 4), ScaleUp, 4, 5)

	// Immediately and one second before expiry: suppressed.
	assertNone(t, mustDecide(t, a, AutoscaleMetrics{CPUPercent: 90}, 5), "cooldown")
	clk.advance(59 * time.Second)
	assertNone(t, mustDecide(t, a, AutoscaleMetrics{CPUPercent: 90}, 5), "cooldown")

	// Exactly at expiry the next hot sample decides again.
	clk.advance(time.Second)
	assertDecision(t, mustDecide(t, a, AutoscaleMetrics{CPUPercent: 90}, 5), ScaleUp, 5, 6)

	// Windows are discarded during cooldown: with window 3 the engine needs 3
	// fresh consecutive samples after cooldown, not 1.
	s3 := specSettings() // window 3
	b, bclk := newTestAutoscaler(s3)
	assertNone(t, mustDecide(t, b, AutoscaleMetrics{CPUPercent: 90}, 4), "1 of 3")
	assertNone(t, mustDecide(t, b, AutoscaleMetrics{CPUPercent: 90}, 4), "2 of 3")
	assertDecision(t, mustDecide(t, b, AutoscaleMetrics{CPUPercent: 90}, 4), ScaleUp, 4, 5)
	for range 2 { // hot samples inside the 60s cooldown reset the streaks
		bclk.advance(10 * time.Second)
		assertNone(t, mustDecide(t, b, AutoscaleMetrics{CPUPercent: 90}, 5), "cooldown")
	}
	bclk.advance(40 * time.Second) // cooldown over
	assertNone(t, mustDecide(t, b, AutoscaleMetrics{CPUPercent: 90}, 5), "1 of 3")
	assertNone(t, mustDecide(t, b, AutoscaleMetrics{CPUPercent: 90}, 5), "2 of 3")
	assertDecision(t, mustDecide(t, b, AutoscaleMetrics{CPUPercent: 90}, 5), ScaleUp, 5, 6)
}

// 13. Zero/no metrics read as idle: scale-down territory (never scale-up), and
// no-op once at the floor.
func TestAutoscaleZeroMetrics(t *testing.T) {
	a, _ := newTestAutoscaler(specSettings())
	for range 2 {
		d := mustDecide(t, a, AutoscaleMetrics{}, 4)
		if d.Direction == ScaleUp {
			t.Fatalf("zero metrics decided %v, want never ScaleUp", d.Direction)
		}
		assertNone(t, d, "")
	}
	assertDecision(t, mustDecide(t, a, AutoscaleMetrics{}, 4), ScaleDown, 4, 3)

	b, _ := newTestAutoscaler(specSettings())
	assertNone(t, mustDecide(t, b, AutoscaleMetrics{}, 2), "1 of 3")
	assertNone(t, mustDecide(t, b, AutoscaleMetrics{}, 2), "2 of 3")
	assertNone(t, mustDecide(t, b, AutoscaleMetrics{}, 2), "min_replicas 2")
}

// 14. CPU exactly at a threshold: >= up, <= down.
func TestAutoscaleCPUExactlyAtThreshold(t *testing.T) {
	s := specSettings()
	s.EvaluationWindows = 1

	a, _ := newTestAutoscaler(s)
	assertDecision(t, mustDecide(t, a, AutoscaleMetrics{CPUPercent: 70, P95Latency: 100 * time.Millisecond}, 4), ScaleUp, 4, 5)

	b, _ := newTestAutoscaler(s)
	assertDecision(t, mustDecide(t, b, AutoscaleMetrics{CPUPercent: 30, P95Latency: 100 * time.Millisecond}, 4), ScaleDown, 4, 3)
}

// 15. Latency exactly at a threshold: >= up, <= down.
func TestAutoscaleLatencyExactlyAtThreshold(t *testing.T) {
	s := specSettings()
	s.EvaluationWindows = 1

	a, _ := newTestAutoscaler(s)
	assertDecision(t, mustDecide(t, a, AutoscaleMetrics{CPUPercent: 50, P95Latency: 500 * time.Millisecond}, 4), ScaleUp, 4, 5)

	b, _ := newTestAutoscaler(s)
	assertDecision(t, mustDecide(t, b, AutoscaleMetrics{CPUPercent: 30, P95Latency: 150 * time.Millisecond}, 4), ScaleDown, 4, 3)
}

// 18. Disabled autoscaling: hot metrics never produce a decision.
func TestAutoscaleDisabled(t *testing.T) {
	s := specSettings()
	s.Enabled = false
	a, _ := newTestAutoscaler(s)
	for range 5 {
		assertNone(t, mustDecide(t, a, AutoscaleMetrics{CPUPercent: 100}, 4), "autoscaling disabled")
	}
}

// Wiring: a resolved phelix.yaml block feeds the engine end to end. (Cases 16
// invalid min/max and 17 non-rolling strategy are covered in the project
// package validation tests — the engine never sees a strategy.)
func TestAutoscalerFromResolvedConfig(t *testing.T) {
	cfg := &project.AutoscalingConfig{
		Enabled: true, MinReplicas: 2, MaxReplicas: 8,
		Interval: "15s", Cooldown: "60s",
		CPU:               &project.AutoscaleCPUConfig{ScaleUp: 70, ScaleDown: 30},
		Latency:           &project.AutoscaleLatencyConfig{ScaleUpP95: "500ms", ScaleDownP95: "150ms"},
		EvaluationWindows: 3,
	}
	a, _ := newTestAutoscaler(cfg.Resolve(2))
	for range 2 {
		assertNone(t, mustDecide(t, a, AutoscaleMetrics{CPUPercent: 75}, 2), "")
	}
	assertDecision(t, mustDecide(t, a, AutoscaleMetrics{CPUPercent: 75}, 2), ScaleUp, 2, 3)
}
