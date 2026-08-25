// Package connstate tracks this agent's connection/authentication state with
// the Phelix backend. It is deliberately separate from the local runtime
// status (is the monitor daemon running, are apps running): logout must not
// stop the runtime, and a stopped runtime is not the same as being logged out.
package connstate

import "sync"

// Connection/auth states reported to the backend and shown by `phelix auth
// status`. These values are part of the wire contract (CLIMetadata.
// connection_state / AgentLogoutRequest.reason) — do not rename.
const (
	// Connected means authenticated sync with the backend is active.
	Connected = "connected"
	// Disconnected means the user logged out (or never logged in). The local
	// runtime keeps running; only dashboard synchronization stops.
	Disconnected = "disconnected"
	// AuthExpired means the backend rejected our credentials
	// (UNAUTHENTICATED / revoked API key). The runtime keeps running; the user
	// recovers with 'phelix auth login'.
	AuthExpired = "auth_expired"
)

var (
	mu    sync.RWMutex
	state string // "" = never synced; treated as unknown/absent on the wire
)

// Set records the current backend connection state. Safe for concurrent use
// by the monitor daemon's streams and short-lived CLI commands.
func Set(s string) {
	mu.Lock()
	defer mu.Unlock()
	state = s
}

// Get returns the last recorded connection state ("" when none was recorded).
func Get() string {
	mu.RLock()
	defer mu.RUnlock()
	return state
}

// MarkConnected records an authenticated, working backend session.
func MarkConnected() { Set(Connected) }

// MarkDisconnected records an explicit user logout.
func MarkDisconnected() { Set(Disconnected) }

// MarkAuthExpired records that the backend rejected the credentials.
func MarkAuthExpired() { Set(AuthExpired) }
