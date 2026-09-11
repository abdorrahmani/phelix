package phelixerr

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestNewAndError(t *testing.T) {
	err := New(CodeNotFound, "application not found")
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	if err.Error() != "application not found" {
		t.Fatalf("unexpected message: %q", err.Error())
	}
	if CodeOf(err) != CodeNotFound {
		t.Fatalf("unexpected code: %s", CodeOf(err))
	}
}

func TestWrappingPreservesRootCause(t *testing.T) {
	root := errors.New("connection refused")

	err := Wrap(
		CodeConnection,
		"failed to connect to server",
		root,
	)

	if !errors.Is(err, root) {
		t.Fatal("root cause was lost")
	}

	if CodeOf(err) != CodeConnection {
		t.Fatalf("unexpected error code: %s", CodeOf(err))
	}

	var se *Error
	if !errors.As(err, &se) {
		t.Fatal("expected to unwrap into *Error")
	}
	if se.Code != CodeConnection {
		t.Fatalf("unexpected inner code: %s", se.Code)
	}
}

func TestWrapNilReturnsNil(t *testing.T) {
	if got := Wrap(CodeServer, "nope", nil); got != nil {
		t.Fatalf("Wrap with nil cause should return nil, got %v", got)
	}
	if got := Wrapf(CodeServer, nil, "nope %d", 1); got != nil {
		t.Fatalf("Wrapf with nil cause should return nil, got %v", got)
	}
}

func TestCodeOfChain(t *testing.T) {
	root := Wrap(CodeBuildFailed, "build failed", errors.New("compile"))
	chained := Wrap(CodeDeployFailed, "deploy failed", root)

	if CodeOf(chained) != CodeDeployFailed {
		t.Fatalf("expected outermost code, got %s", CodeOf(chained))
	}
	if !IsCode(chained, CodeDeployFailed) {
		t.Fatal("expected IsCode(chained, DEPLOY_FAILED)")
	}
	// A wrapped non-structured error walking should still find nested codes.
	if !IsCode(fmt.Errorf("wrap: %w", root), CodeBuildFailed) {
		t.Fatal("expected nested code to be found through fmt wrap")
	}
}

func TestCodeOfContract(t *testing.T) {
	// nil is the absence of a failure and must never panic or crash a switch.
	if CodeOf(nil) != CodeUnknown {
		t.Fatalf("nil should map to UNKNOWN, got %s", CodeOf(nil))
	}
	// An ordinary, non-Phelix error carries no code.
	if CodeOf(errors.New("plain")) != CodeUnknown {
		t.Fatalf("plain error should map to UNKNOWN, got %s", CodeOf(errors.New("plain")))
	}
}

func TestCodeOfOutermostWins(t *testing.T) {
	root := Wrap(CodeBuildFailed, "build failed", errors.New("compile"))
	chained := Wrap(CodeDeployFailed, "deploy failed", root)

	if CodeOf(chained) != CodeDeployFailed {
		t.Fatalf("expected outermost code, got %s", CodeOf(chained))
	}
	if !IsCode(chained, CodeDeployFailed) {
		t.Fatal("expected IsCode(chained, DEPLOY_FAILED)")
	}
}

func TestNestedWrappingKeepsRootDiscoverable(t *testing.T) {
	root := errors.New("connection refused")

	// connection refused
	//      ↓ failed to connect to backend
	//      ↓ monitor connection failed
	chained := Wrap(CodeConnection, "monitor connection failed",
		Wrap(CodeConnection, "failed to connect to backend",
			fmt.Errorf("dial: %w", root)))

	// Root cause must remain discoverable through any number of layers,
	// including a plain fmt.Errorf layer in between.
	if !errors.Is(chained, root) {
		t.Fatal("root cause was lost through nested wrapping")
	}
	if CodeOf(chained) != CodeConnection {
		t.Fatalf("expected outermost code, got %s", CodeOf(chained))
	}
	if got := Cause(chained); !errors.Is(got, root) {
		t.Fatalf("expected Cause to reach root, got %v", got)
	}
}

func TestCodeFoundThroughPlainWrap(t *testing.T) {
	// A structured code must remain findable when a plain fmt.Errorf %w layer
	// is interposed by unrelated code.
	structured := Wrap(CodeBuildFailed, "build failed", errors.New("compile"))
	if !IsCode(fmt.Errorf("wrap: %w", structured), CodeBuildFailed) {
		t.Fatal("expected nested code to be found through fmt wrap")
	}
}

func TestCauseFindsRoot(t *testing.T) {
	root := errors.New("root")
	err := Wrap(CodeDeployFailed, "d", Wrap(CodeBuildFailed, "b", fmt.Errorf("compile: %w", root)))
	if got := Cause(err); !errors.Is(got, root) {
		t.Fatalf("expected root cause, got %v", got)
	}
}

// --- gRPC mapping ---------------------------------------------------------

func TestFromGRPCNilAndPlain(t *testing.T) {
	if FromGRPC(nil) != nil {
		t.Fatal("nil should stay nil")
	}
	plain := errors.New("plain")
	if FromGRPC(plain) != plain {
		t.Fatal("non-gRPC error should pass through unchanged")
	}
}

func TestFromGRPCMapping(t *testing.T) {
	cases := []struct {
		g    codes.Code
		want Code
	}{
		{codes.InvalidArgument, CodeInvalidArgument},
		{codes.NotFound, CodeNotFound},
		{codes.AlreadyExists, CodeAlreadyExists},
		{codes.Unauthenticated, CodeUnauthenticated},
		{codes.PermissionDenied, CodePermissionDenied},
		{codes.DeadlineExceeded, CodeTimeout},
		{codes.Unavailable, CodeConnection},
		{codes.Internal, CodeServer},
		{codes.Unimplemented, CodeUnknown},
	}
	for _, tc := range cases {
		st := status.New(tc.g, "remote")
		err := FromGRPC(st.Err())
		if CodeOf(err) != tc.want {
			t.Fatalf("grpc %s: want %s, got %s", tc.g, tc.want, CodeOf(err))
		}
		// Original gRPC cause must be preserved for --debug.
		if Cause(err) == nil {
			t.Fatalf("grpc %s: cause was lost", tc.g)
		}
	}
}

func TestFromGRPCDoesNotDoubleWrap(t *testing.T) {
	structured := Wrap(CodeDeployFailed, "deploy", errors.New("boom"))
	if got := FromGRPC(structured); got != structured {
		t.Fatal("structured error should pass through unchanged")
	}
}

func TestFromGRPCContextTimeout(t *testing.T) {
	err := FromGRPC(context.DeadlineExceeded)
	if CodeOf(err) != CodeTimeout {
		t.Fatalf("deadline exceeded should map to TIMEOUT, got %s", CodeOf(err))
	}
}

func TestFromGRPCError(t *testing.T) {
	// A non-status, non-context error still round-trips.
	plain := errors.New("transport is closing")
	if got := FromGRPC(plain); got != plain {
		t.Fatal("non-status error should pass through")
	}
}

// TestWrappedGRPCStatusInspectable verifies that a structured error wrapping a
// gRPC status error still lets status.Code and status.FromError reach the
// original status through the Unwrap chain. This is the guarantee Task 4's
// Section 5 requires: migrating to structured errors must not hide the
// transport-level status from code that inspects it.
func TestWrappedGRPCStatusInspectable(t *testing.T) {
	st := status.New(codes.NotFound, "remote app missing")
	inner := st.Err()

	// Simulate the wrapping used by internal/grpc: FromGRPC first, then a
	// context Wrap with a higher-level code.
	wrapped := Wrap(CodeConnection, "failed to open stream", FromGRPC(inner))

	if got := CodeOf(wrapped); got != CodeConnection {
		t.Fatalf("CodeOf(wrapped) = %s, want CONNECTION_ERROR", got)
	}
	// The original gRPC status must still be recoverable through the chain.
	ws, ok := status.FromError(wrapped)
	if !ok {
		t.Fatal("status.FromError could not recover the gRPC status through the wrap")
	}
	if ws.Code() != codes.NotFound {
		t.Fatalf("recovered status code = %v, want NotFound", ws.Code())
	}
	// And status.Code must agree with status.FromError.
	if got := status.Code(wrapped); got != codes.NotFound {
		t.Fatalf("status.Code(wrapped) = %v, want NotFound", got)
	}
}

// TestWrapGRPCCodePreservedThroughFromGRPC verifies that FromGRPC preserves the
// mapped code while the cause stays the original gRPC status, and that a
// double FromGRPC (already-structured) does not rewrap.
func TestWrapGRPCCodePreservedThroughFromGRPC(t *testing.T) {
	st := status.New(codes.Unavailable, "backend down")
	structured := FromGRPC(st.Err())
	if CodeOf(structured) != CodeConnection {
		t.Fatalf("Unavailable should map to CONNECTION_ERROR, got %s", CodeOf(structured))
	}
	// The deepest cause must still be the original gRPC status error (for
	// --debug) — semantically, not by pointer (st.Err() allocates on demand).
	if ws, ok := status.FromError(Cause(structured)); !ok || ws.Code() != codes.Unavailable {
		t.Fatalf("cause was lost: %v (status=%v)", Cause(structured), ws)
	}
}
