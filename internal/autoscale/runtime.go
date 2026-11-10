package autoscale

import (
	"sync"
	"time"
)

// RuntimeState is what the monitor's controller knows about one app's
// autoscaling activity in this process. It is intentionally in-memory: the
// durable history remains the source of truth across restarts, and readers
// fall back to it when no live controller has evaluated the app yet.
type RuntimeState struct {
	LastEvaluation      time.Time
	LastSuccessfulScale time.Time
	LastDecision        *HistoryRecord
}

var (
	runtimeMu    sync.RWMutex
	runtimeState = make(map[string]*RuntimeState)
)

// RecordEvaluation notes that the controller started evaluating an app.
func RecordEvaluation(app string, at time.Time) {
	runtimeMu.Lock()
	defer runtimeMu.Unlock()
	state := runtimeStateForLocked(app)
	state.LastEvaluation = at
}

// RecordDecision notes the most recent recorded verdict for an app.
func RecordDecision(app string, record HistoryRecord) {
	runtimeMu.Lock()
	defer runtimeMu.Unlock()
	state := runtimeStateForLocked(app)
	state.LastDecision = &record
}

// RecordScaleSuccess notes when a scaling operation last completed.
func RecordScaleSuccess(app string, at time.Time) {
	runtimeMu.Lock()
	defer runtimeMu.Unlock()
	state := runtimeStateForLocked(app)
	state.LastSuccessfulScale = at
}

// RuntimeStateFor returns a copy of one app's live autoscaling state. The
// zero value means no controller has evaluated the app in this process.
func RuntimeStateFor(app string) RuntimeState {
	runtimeMu.RLock()
	defer runtimeMu.RUnlock()
	state := runtimeState[app]
	if state == nil {
		return RuntimeState{}
	}
	out := *state
	if state.LastDecision != nil {
		decision := *state.LastDecision
		out.LastDecision = &decision
	}
	return out
}

func runtimeStateForLocked(app string) *RuntimeState {
	state := runtimeState[app]
	if state == nil {
		state = &RuntimeState{}
		runtimeState[app] = state
	}
	return state
}
