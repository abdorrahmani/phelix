// Package supervisor keeps worker replicas alive. One reconcile loop (owned
// by the monitor daemon) walks every type: worker app, compares the persisted
// desired state with the observed processes, applies the restart policy with
// jittered backoff, and runs the configured health probes. Every decision is
// taken under the app's replica coordinator lock, so a deploy or a manual
// command is always the sole decision-maker while it holds the lock.
package supervisor

import (
	"time"

	"github.com/abdorrahmani/phelix/internal/project"
)

// Decision is what the restart policy says about one exited replica.
type Decision int

const (
	// DecisionRestart relaunches the replica (counts toward max attempts).
	DecisionRestart Decision = iota
	// DecisionComplete marks a clean natural exit with no restart due.
	DecisionComplete
	// DecisionFail marks an abnormal exit with no restart due.
	DecisionFail
)

// Evaluate applies the restart policy to one exit. Signal deaths and
// resource-OOM kills count as failures regardless of any reported code. Exit
// 0 under on-failure is a completion, not a failure: a persistent worker that
// exits cleanly is unexpected, but it must not be labeled failed.
func Evaluate(policy string, exitCode int) Decision {
	failed := exitCode != 0
	switch policy {
	case project.RestartNever:
		if failed {
			return DecisionFail
		}
		return DecisionComplete
	case project.RestartAlways:
		return DecisionRestart
	default:
		if failed {
			return DecisionRestart
		}
		return DecisionComplete
	}
}

// Backoff computes the restart delay for attempt (0-based): the initial delay
// grown by multiplier per consecutive attempt, capped at max.
func Backoff(initial, max time.Duration, multiplier float64, attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	d := initial
	for i := 0; i < attempt && d < max; i++ {
		d = time.Duration(float64(d) * multiplier)
	}
	if d > max {
		d = max
	}
	if d <= 0 {
		d = time.Second
	}
	return d
}

// JitterFactor bounds the supervisor's backoff randomization: the delay is
// scaled by 1 + r where r is drawn from [-JitterFactor, +JitterFactor], so a
// fleet of replicas crashing together does not stampede a shared backend.
const JitterFactor = 0.2

// ApplyJitter scales a delay by 1 + r, r in [-JitterFactor, +JitterFactor].
// r is supplied by the caller so tests are deterministic.
func ApplyJitter(d time.Duration, r float64) time.Duration {
	if r > JitterFactor {
		r = JitterFactor
	}
	if r < -JitterFactor {
		r = -JitterFactor
	}
	scaled := time.Duration(float64(d) * (1 + r))
	if scaled < 0 {
		return d
	}
	return scaled
}
