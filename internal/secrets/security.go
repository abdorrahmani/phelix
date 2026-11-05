package secrets

import (
	"sync"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/logs"
)

// SecurityState describes one application's secret-security posture. Healthy
// means the application's bindings resolve and every value is masked in this
// process. Degraded means resolution failed: launches for that application
// fail on their own (fail-closed) and its log streams are withheld, because
// output that cannot be proven masked must not leave the machine. The state
// is per application by design: one broken store never stops the rest of the
// host.
type SecurityState string

const (
	SecurityHealthy  SecurityState = "healthy"
	SecurityDegraded SecurityState = "degraded"
)

// securityEntry is the current posture of one application and why.
type securityEntry struct {
	State  SecurityState
	Reason string
	Since  time.Time
}

// securityMu guards security states. SecurityOf never mutates.
var securityMu sync.RWMutex

// securityStates holds one entry per application that has been evaluated.
var securityStates = map[string]securityEntry{}

// MarkDegraded records that an application's secrets cannot currently be
// resolved or masked in this process. The reason never contains a value;
// callers pass errors that were built to be display-safe, and the redactor
// runs over them one more time for good measure.
func MarkDegraded(appID, reason string) {
	securityMu.Lock()
	wasDegraded := false
	if entry, ok := securityStates[appID]; ok && entry.State == SecurityDegraded {
		wasDegraded = true
	}
	securityStates[appID] = securityEntry{State: SecurityDegraded, Reason: phelixerr.Redact(reason), Since: time.Now().UTC()}
	securityMu.Unlock()
	if !wasDegraded {
		logs.Warning("secrets", "application %s is degraded: its secrets cannot be resolved securely; launches and log transmission for it are halted until this is resolved", appID)
	}
}

// MarkHealthy records that an application's secrets resolved successfully,
// clearing a previous degraded state.
func MarkHealthy(appID string) {
	securityMu.Lock()
	wasDegraded := false
	if entry, ok := securityStates[appID]; ok && entry.State == SecurityDegraded {
		wasDegraded = true
	}
	securityStates[appID] = securityEntry{State: SecurityHealthy, Since: time.Now().UTC()}
	securityMu.Unlock()
	if wasDegraded {
		logs.Info("secrets", "application %s is healthy again: its secrets resolve and are masked", appID)
	}
}

// SecurityOf reports an application's current posture. An application that
// was never evaluated is healthy: it either has no bindings or its next
// launch will evaluate it.
func SecurityOf(appID string) (SecurityState, string) {
	securityMu.RLock()
	defer securityMu.RUnlock()
	entry, ok := securityStates[appID]
	if !ok || entry.State == SecurityHealthy {
		return SecurityHealthy, ""
	}
	return SecurityDegraded, entry.Reason
}
