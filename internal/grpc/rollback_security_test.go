package grpc

import (
	"testing"
	"time"
)

// --- Security contract: the live session token must never be serialized ---
//
// These tests pin the fix that removed the raw session token from the
// rollback lifecycle event body. The token still authenticates the request via
// gRPC authorization metadata (attachAuthMetadata); it simply must not also be
// carried inside the serialized message, where it would travel through the
// event queue, the local rollback log, and the wire as a second, unredacted
// copy of a credential. The event carries only the session ID, for backend
// attribution.

func TestRollbackEvent_NoSessionTokenInBody(t *testing.T) {
	setupTestSession(t) // writes session.json with token "test-token"

	r := NewRollbackReporter(
		"", /* server ID (initialized by setupTestSession) */
		"cli-app",
		"resolved-app",
		"leak-app",
		"blue-green",
		"graceful",
		"v1",
		"v2",
		"v1.2.0",
	)
	r.Emit("start", true, "starting rollback", 10*time.Millisecond, "")

	ev := r.Event()
	if ev.SessionToken != "" {
		t.Fatalf("raw session token leaked into rollback event body: %q", ev.SessionToken)
	}
	if ev.UserId != "test-session" {
		t.Fatalf("expected session ID for backend attribution, got %q", ev.UserId)
	}
}

// TestSessionIdentity_KeepsTokenForMetadataAuth ensures the token is still
// available to the auth-metadata path even though it no longer appears in the
// event body. Removing it from the event is a hardening step, not a removal of
// the credential from the system.
func TestSessionIdentity_KeepsTokenForMetadataAuth(t *testing.T) {
	setupTestSession(t)

	sid, tok := loadSessionIdentity()
	if sid != "test-session" {
		t.Fatalf("expected session ID, got %q", sid)
	}
	if tok != "test-token" {
		t.Fatalf("expected token to remain available for metadata auth, got %q", tok)
	}
}

// TestSessionIdentity_EmptyWithoutSession covers the no-session case: identity
// is absent entirely, with no panic and no stub value.
func TestSessionIdentity_EmptyWithoutSession(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	sid, tok := loadSessionIdentity()
	if sid != "" || tok != "" {
		t.Fatalf("expected empty identity without session.json, got sid=%q tok=%q", sid, tok)
	}
}
