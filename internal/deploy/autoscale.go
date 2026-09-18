package deploy

import (
	"fmt"
	"time"

	"github.com/abdorrahmani/phelix/internal/project"
)

// Phase 1 autoscaling: the decision engine only observes metrics and decides
// whether the desired replica count of a rolling deployment should change.
// It starts/stops nothing, touches no proxy state, no deploy.json and no
// rebuild pipeline; Phase 2 wires a metrics collector (per-replica CPU, proxy
// p95) and a decision executor on top.

// AutoscaleMetrics is one observation sample fed to the decision engine. It
// is deliberately free of gopsutil, proxy and filesystem types so the engine
// is a pure metrics-in/decision-out function.
type AutoscaleMetrics struct {
	CPUPercent float64
	P95Latency time.Duration
}

// ScaleDirection is the outcome of one autoscaling evaluation.
type ScaleDirection int

const (
	ScaleNone ScaleDirection = iota
	ScaleUp
	ScaleDown
)

// ScaleDecision is the engine's verdict for one sample. A non-None decision is
// a desired target only — nothing in Phase 1 applies it, so CurrentReplicas
// stays what the caller passed until an executor exists.
type ScaleDecision struct {
	Direction       ScaleDirection
	CurrentReplicas int
	DesiredReplicas int
	Reason          string
}

// Autoscaler is the per-app autoscaling decision engine. It owns the
// hysteresis windows and the cooldown clock; the current replica count is
// passed into Evaluate so the engine never reaches into deploy state.
type Autoscaler struct {
	settings project.AutoscaleSettings

	// now is the clock seam: tests inject a fake clock, production uses
	// time.Now.
	now func() time.Time

	upStreak   int // consecutive samples satisfying the scale-up condition
	downStreak int // consecutive samples satisfying the scale-down condition

	acted        bool      // a decision was produced; cooldown anchor is live
	lastActionAt time.Time // when the last decision was produced
}

// NewAutoscaler builds the engine from settings resolved by
// project.AutoscalingConfig.Resolve (defaults applied; the underlying config
// is assumed to have passed validation).
func NewAutoscaler(s project.AutoscaleSettings) *Autoscaler {
	return &Autoscaler{settings: s, now: time.Now}
}

// Evaluate folds one metrics sample into a decision for an app currently
// running currentReplicas replicas. Rules:
//
//	scale up:   cpu >= scale_up OR p95 >= scale_up_p95
//	scale down: cpu <= scale_down AND p95 <= scale_down_p95
//
// A condition must hold for EvaluationWindows consecutive samples before a
// decision is produced, every decision moves the desired count by exactly one
// replica, and no further decision is produced until Cooldown has elapsed
// since the last one (anchored on the decision itself; Phase 2 can re-arm it
// on confirmed execution). Zero metrics count as "idle": they satisfy the
// scale-down condition, never the scale-up one.
func (a *Autoscaler) Evaluate(m AutoscaleMetrics, currentReplicas int) ScaleDecision {
	none := func(reason string) ScaleDecision {
		return ScaleDecision{Direction: ScaleNone, CurrentReplicas: currentReplicas, DesiredReplicas: currentReplicas, Reason: reason}
	}
	s := a.settings
	if !s.Enabled {
		return none("autoscaling disabled")
	}

	now := a.now()
	if a.acted && now.Sub(a.lastActionAt) < s.Cooldown {
		// Forget partial windows during cooldown so the first sample after it
		// must earn a fresh evaluation window — that is what breaks the
		// up→down→up oscillation cooldown exists for.
		a.upStreak, a.downStreak = 0, 0
		return none(fmt.Sprintf("cooldown: %s of %s elapsed since last decision", now.Sub(a.lastActionAt).Round(time.Millisecond), s.Cooldown))
	}

	cpuUp := m.CPUPercent >= s.ScaleUpCPU
	latUp := m.P95Latency >= s.ScaleUpP95
	cpuDown := m.CPUPercent <= s.ScaleDownCPU
	latDown := m.P95Latency <= s.ScaleDownP95

	switch {
	case cpuUp || latUp:
		a.upStreak++
		a.downStreak = 0
		if a.upStreak < s.EvaluationWindows {
			return none(fmt.Sprintf("scale-up condition held for %d of %d samples", a.upStreak, s.EvaluationWindows))
		}
		if currentReplicas >= s.MaxReplicas {
			a.upStreak = 0
			return none(fmt.Sprintf("scale-up condition met but already at max_replicas %d", s.MaxReplicas))
		}
		reason := fmt.Sprintf("%s for %d consecutive sample(s)", scaleUpReason(m, s, cpuUp, latUp), a.upStreak)
		a.acted, a.lastActionAt = true, now
		a.upStreak, a.downStreak = 0, 0
		return ScaleDecision{Direction: ScaleUp, CurrentReplicas: currentReplicas, DesiredReplicas: currentReplicas + 1, Reason: reason}

	case cpuDown && latDown:
		a.downStreak++
		a.upStreak = 0
		if a.downStreak < s.EvaluationWindows {
			return none(fmt.Sprintf("scale-down condition held for %d of %d samples", a.downStreak, s.EvaluationWindows))
		}
		if currentReplicas <= s.MinReplicas {
			a.downStreak = 0
			return none(fmt.Sprintf("scale-down condition met but already at min_replicas %d", s.MinReplicas))
		}
		reason := fmt.Sprintf("cpu %.1f%% <= %.1f%% and p95 %s <= %s for %d consecutive sample(s)",
			m.CPUPercent, s.ScaleDownCPU, m.P95Latency, s.ScaleDownP95, a.downStreak)
		a.acted, a.lastActionAt = true, now
		a.upStreak, a.downStreak = 0, 0
		return ScaleDecision{Direction: ScaleDown, CurrentReplicas: currentReplicas, DesiredReplicas: currentReplicas - 1, Reason: reason}

	default:
		// Inside the hysteresis band: a sample satisfying neither condition
		// breaks both windows.
		a.upStreak, a.downStreak = 0, 0
		return none(fmt.Sprintf("cpu %.1f%% and p95 %s within thresholds", m.CPUPercent, m.P95Latency))
	}
}

// scaleUpReason names which side of the OR fired (CPU, latency or both).
func scaleUpReason(m AutoscaleMetrics, s project.AutoscaleSettings, cpuUp, latUp bool) string {
	switch {
	case cpuUp && latUp:
		return fmt.Sprintf("cpu %.1f%% >= %.1f%% and p95 %s >= %s", m.CPUPercent, s.ScaleUpCPU, m.P95Latency, s.ScaleUpP95)
	case cpuUp:
		return fmt.Sprintf("cpu %.1f%% >= %.1f%%", m.CPUPercent, s.ScaleUpCPU)
	default:
		return fmt.Sprintf("p95 %s >= %s", m.P95Latency, s.ScaleUpP95)
	}
}
