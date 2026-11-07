package deploy

import (
	"fmt"
	"time"

	"github.com/abdorrahmani/phelix/internal/project"
)

// The autoscaling decision engine observes metrics and decides whether the
// desired replica count of an app should change. It starts/stops nothing,
// touches no proxy state, no deploy.json and no rebuild pipeline: the
// controller in internal/autoscale collects samples and dispatches decisions
// to the existing scaling paths (deploy.WorkerScale for workers, the rolling
// scale executor for services).

// AutoscaleMissing flags which metric dimensions could not be collected for
// one sample. A missing dimension never contributes a scale-up signal and
// blocks scale-down while its thresholds are configured: the engine never
// shrinks a fleet on data it could not see.
type AutoscaleMissing uint8

const (
	MissingCPU AutoscaleMissing = 1 << iota
	MissingMemory
	MissingRPS
	MissingLatency
)

// Has reports whether the sample is missing the given dimension.
func (m AutoscaleMissing) Has(flag AutoscaleMissing) bool { return m&flag != 0 }

// AutoscaleMetrics is one observation sample fed to the decision engine. It
// is deliberately free of gopsutil, proxy and filesystem types so the engine
// is a pure metrics-in/decision-out function.
type AutoscaleMetrics struct {
	CPUPercent    float64
	MemoryPercent float64
	RPS           float64
	P95Latency    time.Duration
	P99Latency    time.Duration

	// At is the sample time. Zero disables the freshness check, which keeps
	// hand-built samples (tests, legacy callers) valid; the controller always
	// stamps samples.
	At time.Time

	// Missing flags the dimensions that failed collection.
	Missing AutoscaleMissing

	// TrafficApplicable reports whether proxy traffic metrics exist for this
	// app at all (a proxy is enrolled). When false, RPS and latency are
	// excluded from evaluation instead of reading as zero-idle, so proxy-less
	// apps are never scaled on invented traffic. For compatibility with
	// callers that predate this field, a nonzero P95/P99 latency implies
	// applicability.
	TrafficApplicable bool
}

// trafficApplicable resolves the sample's traffic applicability, including
// the legacy nonzero-latency implication.
func (m AutoscaleMetrics) trafficApplicable() bool {
	return m.TrafficApplicable || m.P95Latency > 0 || m.P99Latency > 0
}

// ScaleDirection is the outcome of one autoscaling evaluation.
type ScaleDirection int

const (
	ScaleNone ScaleDirection = iota
	ScaleUp
	ScaleDown
)

// ScaleDecision is the engine's verdict for one sample. A non-None decision
// is a desired target only; the controller applies it through the existing
// scaling paths and reports the outcome back via ConfirmExecution or
// AbortDecision.
type ScaleDecision struct {
	Direction       ScaleDirection
	CurrentReplicas int
	DesiredReplicas int
	Reason          string
}

// Autoscaler is the per-app autoscaling decision engine. It owns the
// hysteresis windows, the stabilization clock and the cooldown clocks; the
// current replica count is passed into Evaluate so the engine never reaches
// into deploy state.
type Autoscaler struct {
	settings project.AutoscaleSettings

	// now is the clock seam: tests inject a fake clock, production uses
	// time.Now.
	now func() time.Time

	upStreak   int // consecutive samples satisfying the scale-up condition
	downStreak int // consecutive samples satisfying the scale-down condition
	downSince  time.Time

	acted        bool      // a decision was produced; cooldown anchors are live
	lastActionAt time.Time // when the last decision was produced
	prevActed    bool      // pre-decision state, restored by AbortDecision
	prevLastAt   time.Time
}

// NewAutoscaler builds the engine from settings resolved by
// project.AutoscalingConfig.Resolve (defaults applied; the underlying config
// is assumed to have passed validation).
func NewAutoscaler(s project.AutoscaleSettings) *Autoscaler {
	return &Autoscaler{settings: s, now: time.Now}
}

// SetClock replaces the engine's time source. The controller injects its own
// clock so engine freshness checks and controller scheduling share one time
// source (and so tests can drive both with a fake clock).
func (a *Autoscaler) SetClock(now func() time.Time) {
	if now != nil {
		a.now = now
	}
}

// Evaluate folds one metrics sample into a decision for an app currently
// running currentReplicas replicas. Rules:
//
//	scale up:   ANY enabled metric breaches its upper threshold
//	scale down: EVERY enabled metric is at or below its lower threshold
//
// Scale up wins when both sides qualify on one sample. A condition must hold
// for its direction's evaluation_windows consecutive samples before a
// decision is produced, every decision moves the desired count by exactly one
// replica, and a direction cannot decide again until its cooldown has elapsed
// since the last decision. The scale-down condition must additionally have
// held continuously for stabilization_window before a scale-down decision
// fires. A stale sample (At older than twice the evaluation interval)
// produces no decision and resets both windows. Missing metrics never
// contribute scale-up signals and block scale-down while configured. Traffic
// metrics that do not apply to the app (no proxy enrolled) are excluded from
// both directions rather than read as zero.
func (a *Autoscaler) Evaluate(m AutoscaleMetrics, currentReplicas int) ScaleDecision {
	none := func(reason string) ScaleDecision {
		return ScaleDecision{Direction: ScaleNone, CurrentReplicas: currentReplicas, DesiredReplicas: currentReplicas, Reason: reason}
	}
	s := a.settings
	if !s.Enabled {
		return none("autoscaling disabled")
	}

	now := a.now()
	if !m.At.IsZero() && s.Interval > 0 {
		if age := now.Sub(m.At); age > 2*s.Interval {
			a.upStreak, a.downStreak, a.downSince = 0, 0, time.Time{}
			return none(fmt.Sprintf("stale metrics: sample is %s old, freshness bound is %s", age.Round(time.Millisecond), (2 * s.Interval)))
		}
	}

	upCool, downCool := upCooldownOf(s), downCooldownOf(s)
	upBlocked := a.acted && now.Sub(a.lastActionAt) < upCool
	downBlocked := a.acted && now.Sub(a.lastActionAt) < downCool
	if upBlocked {
		// Forget the up window during cooldown so the first sample after it
		// must earn a fresh evaluation window; that is what breaks the
		// up-down-up oscillation cooldown exists for.
		a.upStreak = 0
	}
	if downBlocked {
		a.downStreak, a.downSince = 0, time.Time{}
	}
	if upBlocked && downBlocked {
		return none(fmt.Sprintf("cooldown: %s elapsed since last decision (up %s, down %s)",
			now.Sub(a.lastActionAt).Round(time.Millisecond), upCool, downCool))
	}

	upWindows, downWindows := upWindowsOf(s), downWindowsOf(s)
	cpuUp := !m.Missing.Has(MissingCPU) && m.CPUPercent >= s.ScaleUpCPU
	memUp := !m.Missing.Has(MissingMemory) && s.ScaleUpMemory > 0 && m.MemoryPercent >= s.ScaleUpMemory
	rpsUp := !m.Missing.Has(MissingRPS) && m.trafficApplicable() && s.ScaleUpRPS > 0 && m.RPS >= s.ScaleUpRPS
	latUp := !m.Missing.Has(MissingLatency) && m.trafficApplicable() &&
		(m.P95Latency >= s.ScaleUpP95 || (s.ScaleUpP99 > 0 && m.P99Latency >= s.ScaleUpP99))

	cpuDown := !m.Missing.Has(MissingCPU) && m.CPUPercent <= s.ScaleDownCPU
	memDown := s.ScaleDownMemory <= 0 || (!m.Missing.Has(MissingMemory) && m.MemoryPercent <= s.ScaleDownMemory)
	rpsDown := s.ScaleDownRPS <= 0 || (m.trafficApplicable() && !m.Missing.Has(MissingRPS) && m.RPS <= s.ScaleDownRPS)
	latDown := true
	if m.trafficApplicable() {
		latDown = !m.Missing.Has(MissingLatency) && m.P95Latency <= s.ScaleDownP95 &&
			(s.ScaleDownP99 <= 0 || m.P99Latency <= s.ScaleDownP99)
	}

	switch {
	case cpuUp || memUp || rpsUp || latUp:
		if upBlocked {
			return none(fmt.Sprintf("cooldown: %s elapsed since last decision, scale-up blocked for %s",
				now.Sub(a.lastActionAt).Round(time.Millisecond), upCool))
		}
		a.upStreak++
		a.downStreak, a.downSince = 0, time.Time{}
		if a.upStreak < upWindows {
			return none(fmt.Sprintf("scale-up condition held for %d of %d samples", a.upStreak, upWindows))
		}
		if currentReplicas >= s.MaxReplicas {
			a.upStreak = 0
			return none(fmt.Sprintf("scale-up condition met but already at max_replicas %d", s.MaxReplicas))
		}
		reason := fmt.Sprintf("%s for %d consecutive sample(s)", scaleUpReason(m, s, cpuUp, memUp, rpsUp, latUp), a.upStreak)
		a.anchorDecision(now)
		a.upStreak, a.downStreak = 0, 0
		return ScaleDecision{Direction: ScaleUp, CurrentReplicas: currentReplicas, DesiredReplicas: currentReplicas + 1, Reason: reason}

	case cpuDown && memDown && rpsDown && latDown:
		if downBlocked {
			return none(fmt.Sprintf("cooldown: %s elapsed since last decision, scale-down blocked for %s",
				now.Sub(a.lastActionAt).Round(time.Millisecond), downCool))
		}
		a.downStreak++
		a.upStreak = 0
		if a.downStreak == 1 {
			a.downSince = now
		}
		if a.downStreak < downWindows {
			return none(fmt.Sprintf("scale-down condition held for %d of %d samples", a.downStreak, downWindows))
		}
		if s.StabilizationWindow > 0 {
			if held := now.Sub(a.downSince); held < s.StabilizationWindow {
				return none(fmt.Sprintf("scale-down condition held but stabilization window at %s of %s", held.Round(time.Millisecond), s.StabilizationWindow))
			}
		}
		if currentReplicas <= s.MinReplicas {
			a.downStreak, a.downSince = 0, time.Time{}
			return none(fmt.Sprintf("scale-down condition met but already at min_replicas %d", s.MinReplicas))
		}
		reason := fmt.Sprintf("%s for %d consecutive sample(s)", scaleDownReason(m, s), a.downStreak)
		a.anchorDecision(now)
		a.upStreak, a.downStreak, a.downSince = 0, 0, time.Time{}
		return ScaleDecision{Direction: ScaleDown, CurrentReplicas: currentReplicas, DesiredReplicas: currentReplicas - 1, Reason: reason}

	default:
		// Inside the hysteresis band: a sample satisfying neither condition
		// breaks both windows.
		a.upStreak, a.downStreak, a.downSince = 0, 0, time.Time{}
		return none(fmt.Sprintf("metrics within thresholds (cpu %.1f%%, memory %.1f%%, rps %.1f, p95 %s)",
			m.CPUPercent, m.MemoryPercent, m.RPS, m.P95Latency))
	}
}

// ConfirmExecution re-anchors the cooldown clocks at the completion of a
// successfully executed decision, so the next decision in that direction is
// measured from confirmed execution rather than decision production. at is
// the execution completion time; a time at or before the decision time leaves
// the anchor unchanged.
func (a *Autoscaler) ConfirmExecution(at time.Time) {
	if at.After(a.lastActionAt) {
		a.lastActionAt = at
	}
	a.prevActed, a.prevLastAt = false, time.Time{}
}

// AbortDecision rolls back the cooldown anchor recorded by the last decision,
// for executions that failed: a failed scaling operation must not consume its
// direction's cooldown, so the controller can retry on a later evaluation.
// Window resets from the decision are kept; the failed sample still counts as
// consumed.
func (a *Autoscaler) AbortDecision() {
	a.acted, a.lastActionAt = a.prevActed, a.prevLastAt
	a.prevActed, a.prevLastAt = false, time.Time{}
}

// anchorDecision records the cooldown anchor and the pre-decision state that
// AbortDecision restores.
func (a *Autoscaler) anchorDecision(now time.Time) {
	a.prevActed, a.prevLastAt = a.acted, a.lastActionAt
	a.acted, a.lastActionAt = true, now
}

// upCooldownOf resolves the scale-up cooldown, falling back to the legacy
// top-level cooldown for settings that predate the per-direction split.
func upCooldownOf(s project.AutoscaleSettings) time.Duration {
	if s.ScaleUpCooldown > 0 {
		return s.ScaleUpCooldown
	}
	return s.Cooldown
}

// downCooldownOf resolves the scale-down cooldown the same way.
func downCooldownOf(s project.AutoscaleSettings) time.Duration {
	if s.ScaleDownCooldown > 0 {
		return s.ScaleDownCooldown
	}
	return s.Cooldown
}

// upWindowsOf resolves the scale-up evaluation window with the same
// fallbacks, defaulting to one sample.
func upWindowsOf(s project.AutoscaleSettings) int {
	if s.ScaleUpWindows > 0 {
		return s.ScaleUpWindows
	}
	if s.EvaluationWindows > 0 {
		return s.EvaluationWindows
	}
	return 1
}

// downWindowsOf resolves the scale-down evaluation window the same way.
func downWindowsOf(s project.AutoscaleSettings) int {
	if s.ScaleDownWindows > 0 {
		return s.ScaleDownWindows
	}
	if s.EvaluationWindows > 0 {
		return s.EvaluationWindows
	}
	return 1
}

// scaleUpReason names which side of the up OR fired (CPU, memory, RPS,
// latency or a combination).
func scaleUpReason(m AutoscaleMetrics, s project.AutoscaleSettings, cpuUp, memUp, rpsUp, latUp bool) string {
	parts := make([]string, 0, 4)
	if cpuUp {
		parts = append(parts, fmt.Sprintf("cpu %.1f%% >= %.1f%%", m.CPUPercent, s.ScaleUpCPU))
	}
	if memUp {
		parts = append(parts, fmt.Sprintf("memory %.1f%% >= %.1f%%", m.MemoryPercent, s.ScaleUpMemory))
	}
	if rpsUp {
		parts = append(parts, fmt.Sprintf("rps %.1f >= %.1f", m.RPS, s.ScaleUpRPS))
	}
	if latUp {
		if s.ScaleUpP99 > 0 && m.P99Latency >= s.ScaleUpP99 {
			parts = append(parts, fmt.Sprintf("p99 %s >= %s", m.P99Latency, s.ScaleUpP99))
		} else {
			parts = append(parts, fmt.Sprintf("p95 %s >= %s", m.P95Latency, s.ScaleUpP95))
		}
	}
	reason := parts[0]
	for _, p := range parts[1:] {
		reason += " and " + p
	}
	return reason
}

// scaleDownReason lists the satisfied down thresholds.
func scaleDownReason(m AutoscaleMetrics, s project.AutoscaleSettings) string {
	reason := fmt.Sprintf("cpu %.1f%% <= %.1f%%", m.CPUPercent, s.ScaleDownCPU)
	if s.ScaleDownMemory > 0 {
		reason += fmt.Sprintf(" and memory %.1f%% <= %.1f%%", m.MemoryPercent, s.ScaleDownMemory)
	}
	if s.ScaleDownRPS > 0 {
		reason += fmt.Sprintf(" and rps %.1f <= %.1f", m.RPS, s.ScaleDownRPS)
	}
	reason += fmt.Sprintf(" and p95 %s <= %s", m.P95Latency, s.ScaleDownP95)
	if s.ScaleDownP99 > 0 {
		reason += fmt.Sprintf(" and p99 %s <= %s", m.P99Latency, s.ScaleDownP99)
	}
	return reason
}
