package connstate

import "testing"

func TestTransitions(t *testing.T) {
	if got := Get(); got != "" {
		t.Fatalf("initial state = %q, want empty", got)
	}
	MarkConnected()
	if Get() != Connected {
		t.Fatalf("after MarkConnected = %q", Get())
	}
	// Temporary network failure: disconnected, then back to connected.
	MarkDisconnected()
	if Get() != Disconnected {
		t.Fatalf("after MarkDisconnected = %q", Get())
	}
	MarkConnected()
	// Backend rejects credentials.
	MarkAuthExpired()
	if Get() != AuthExpired {
		t.Fatalf("after MarkAuthExpired = %q", Get())
	}
	// Re-login restores connected with the same agent identity — the state is
	// just a label, nothing was deleted anywhere.
	MarkConnected()
	if Get() != Connected {
		t.Fatalf("final = %q, want %q", Get(), Connected)
	}
}

func TestStateValuesAreStable(t *testing.T) {
	// Wire contract: sent in CLIMetadata.connection_state and
	// AgentLogoutRequest.reason. Renaming would break backend parsing.
	want := map[string]string{
		Connected:    "connected",
		Disconnected: "disconnected",
		AuthExpired:  "auth_expired",
	}
	for got, exp := range want {
		if got != exp {
			t.Errorf("state value %q != contract value %q", got, exp)
		}
	}
}
