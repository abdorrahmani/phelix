package session

// SessionResult is the machine-contract result object for a single session.
// The session's own fields are promoted (flattened) into the result; show adds
// an optional resolved block. create/checkpoint/complete/fail/cancel return the
// bare session (Resolved nil, omitted).
type SessionResult struct {
	*Session
	Resolved *Resolved `json:"resolved,omitempty"`
}

// ListResult is the bounded result of session list. Truncated is true when the
// limit dropped matching sessions, so a consumer knows the view is incomplete.
type ListResult struct {
	Sessions  []*Session `json:"sessions"`
	Count     int        `json:"count"`
	Truncated bool       `json:"truncated"`
}

// Resolved reports the live state of a session's references, resolved through
// the existing plan/operation read APIs at show time. It never fabricates
// state: a reference that cannot be confirmed is reported as missing, unknown
// or unavailable rather than invented, and one bad reference never fails the
// whole show.
type Resolved struct {
	Plans       []RefView `json:"plans"`
	Operations  []RefView `json:"operations"`
	Deployments []RefView `json:"deployments"`
}

// Reference resolution states.
const (
	RefPresent     = "present"     // the entity exists and was read
	RefMissing     = "missing"     // the entity does not exist (e.g. pruned)
	RefUnavailable = "unavailable" // the entity exists but could not be read (corrupt/tampered)
	RefUnknown     = "unknown"     // existence cannot be determined by id (deployments)
)

// RefView is one resolved reference. Status/Detail carry entity-specific facts
// (a plan's status + hash, an operation's external status + kind, the
// operation a deployment correlates with). ErrorCode is the code when a
// reference is unavailable.
type RefView struct {
	ID        string `json:"id"`
	Present   bool   `json:"present"`
	State     string `json:"state"`
	Status    string `json:"status,omitempty"`
	Detail    string `json:"detail,omitempty"`
	ErrorCode string `json:"error_code,omitempty"`
}
