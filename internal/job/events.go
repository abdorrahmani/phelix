package job

import "time"

// Phase is one observable lifecycle transition of an execution.
type Phase string

const (
	PhaseAccepted       Phase = "accepted"
	PhaseStarted        Phase = "started"
	PhaseAttemptStarted Phase = "attempt_started"
	PhaseAttemptFailed  Phase = "attempt_failed"
	PhaseSucceeded      Phase = "succeeded"
	PhaseFailed         Phase = "failed"
	PhaseTimedOut       Phase = "timed_out"
	PhaseCancelled      Phase = "cancelled"
	PhaseSkipped        Phase = "skipped"
	PhaseInterrupted    Phase = "interrupted"
)

// Event is one lifecycle transition. Events are observations only: the
// durable record remains the source of truth. They are emitted only when
// Store.Events is installed, so the CLI and tests pay nothing.
type Event struct {
	AppName      string
	AppID        string
	ExecutionID  string
	Trigger      Trigger
	Phase        Phase
	Status       Status
	Timestamp    time.Time
	Attempt      int
	Error        string
	ErrorCode    string
	ScheduledFor *time.Time
}

// terminalPhase maps a final execution status to its lifecycle phase.
func terminalPhase(status Status) Phase {
	switch status {
	case StatusSucceeded:
		return PhaseSucceeded
	case StatusTimedOut:
		return PhaseTimedOut
	case StatusCancelled:
		return PhaseCancelled
	case StatusInterrupted:
		return PhaseInterrupted
	case StatusSkipped:
		return PhaseSkipped
	default:
		return PhaseFailed
	}
}

// eventFor builds a lifecycle event from a record.
func eventFor(record *Record, phase Phase, attempt int, now time.Time) Event {
	message := record.Error
	switch phase {
	case PhaseAccepted, PhaseStarted, PhaseAttemptStarted:
		message = ""
	}
	return Event{
		AppName:      record.AppName,
		AppID:        record.AppID,
		ExecutionID:  record.ID,
		Trigger:      record.Trigger,
		Phase:        phase,
		Status:       record.Status,
		Timestamp:    now,
		Attempt:      attempt,
		Error:        message,
		ErrorCode:    string(phase),
		ScheduledFor: record.ScheduledFor,
	}
}

func (s *Store) emit(event Event) {
	if s == nil || s.Events == nil {
		return
	}
	s.Events(event)
}

// emitRecord emits a phase for the given record.
func (s *Store) emitRecord(record *Record, phase Phase, attempt int) {
	if s == nil || s.Events == nil || record == nil {
		return
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	s.emit(eventFor(record, phase, attempt, now()))
}
