package authz

import (
	"os"

	"github.com/abdorrahmani/phelix/internal/machine"
)

// Caller types. These name the transport a request arrived on, not a
// privilege level. Only CallerCLI is produced in Phase 4; the others exist so
// a future authentication layer (API credentials, service identity, an MCP
// server, an agent credential) can populate an actor without redesigning
// either the authorization rules or the execution path.
const (
	CallerCLI     = "cli"
	CallerAPI     = "api"
	CallerAgent   = "agent"
	CallerMCP     = "mcp"
	CallerService = "service"
)

// Actor is the authorization layer's view of who is asking. It is produced
// exclusively by an [Authenticator] — never from a CLI flag, an HTTP header or
// any other caller-supplied value — which is what makes privilege escalation
// by self-identification structurally impossible rather than merely
// discouraged.
//
// The three fields mirror the Phase 1 machine.Actor block so the wire shape
// agents already consume does not change. The invariant that matters:
//
//	ID != "" implies Authenticated
//
// An unauthenticated actor has no ID, so it can never match a rule that names
// one. [Actor.Validate] enforces this at the boundary.
type Actor struct {
	Type          string
	ID            string
	Authenticated bool
}

// Validate rejects an actor that cannot be trusted to be what it claims. A
// nonsensical actor is a programming error in an authentication adapter, not
// a caller problem, and it fails closed: AUTHZ_INVALID, no mutation.
func (a Actor) Validate() error {
	switch a.Type {
	case CallerCLI, CallerAPI, CallerAgent, CallerMCP, CallerService:
	default:
		return invalidf("unknown caller type %q", a.Type)
	}
	if a.ID != "" && !a.Authenticated {
		return invalidf("caller type %q presented an identity without authentication", a.Type)
	}
	return nil
}

// Machine converts the actor into the Phase 1 machine-contract actor block.
// The ID stays null unless it was established by authentication, so the
// documented promise that `actor` is provenance until a real authentication
// phase lands remains literally true.
func (a Actor) Machine() *machine.Actor {
	m := &machine.Actor{Type: a.Type, Authenticated: a.Authenticated}
	if a.ID != "" {
		id := a.ID
		m.ID = &id
	}
	return m
}

// String renders the actor for audit records and decision reasons. It is
// deliberately not an identity claim: an unauthenticated CLI caller always
// renders as "cli:unauthenticated", whatever it would like to be called.
func (a Actor) String() string {
	if !a.Authenticated {
		return a.Type + ":unauthenticated"
	}
	if a.ID == "" {
		return a.Type + ":authenticated"
	}
	return a.Type + ":" + a.ID
}

// Authenticator resolves the caller of the current request into an [Actor].
// This is the seam a future authentication phase implements: an API server
// would inspect its credentials, an MCP server its session, a service its
// identity document — and the authorization layer below would not change by a
// line, because it only ever sees the resolved Actor.
//
// The authorization layer must never inspect CLI flags, HTTP headers or MCP
// metadata itself; that coupling is exactly what this interface prevents.
type Authenticator interface {
	Authenticate() (Actor, error)
}

// LocalCLIAuthenticator is Phase 4's only authenticator: the local CLI
// process. It reports the honest truth about a `phelix …` invocation — a local
// process with no verified identity — and has no input at all, so there is
// nothing a caller could pass to influence it.
//
// What a local CLI caller actually proves is filesystem authority over the
// Phelix data directory, which is recorded as provenance (the host login
// name) but never as an authenticated identity: the authorization rules
// cannot match on it, and it grants nothing.
type LocalCLIAuthenticator struct{}

// Authenticate returns the unauthenticated local CLI actor.
func (LocalCLIAuthenticator) Authenticate() (Actor, error) {
	return Actor{Type: CallerCLI, Authenticated: false}, nil
}

// LocalProvenance returns the host login name of the current process, for
// audit records only. It is not an identity: it is unverified, it is not
// placed on the Actor, and no authorization rule can match it.
func LocalProvenance() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u := os.Getenv("LOGNAME"); u != "" {
		return u
	}
	return ""
}
