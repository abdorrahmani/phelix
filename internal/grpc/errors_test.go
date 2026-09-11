package grpc

import (
	"context"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestRPCFailedMapsCodes verifies that rpcFailed classifies RPC failures by
// their gRPC status code, distinguishing authentication, permission, deadline
// and connection failures (Task 4 Section 7) instead of labelling every RPC
// failure a connection failure.
func TestRPCFailedMapsCodes(t *testing.T) {
	cases := []struct {
		name string
		g    codes.Code
		want phelixerr.Code
	}{
		{"invalid argument", codes.InvalidArgument, phelixerr.CodeInvalidArgument},
		{"not found", codes.NotFound, phelixerr.CodeNotFound},
		{"already exists", codes.AlreadyExists, phelixerr.CodeAlreadyExists},
		{"unauthenticated", codes.Unauthenticated, phelixerr.CodeUnauthenticated},
		{"permission denied", codes.PermissionDenied, phelixerr.CodePermissionDenied},
		{"unavailable", codes.Unavailable, phelixerr.CodeConnection},
		{"deadline exceeded", codes.DeadlineExceeded, phelixerr.CodeTimeout},
		{"internal", codes.Internal, phelixerr.CodeServer},
		{"resource exhausted", codes.ResourceExhausted, phelixerr.CodeServer},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := status.New(tc.g, "remote")
			err := rpcFailed("test operation", st.Err())
			if got := phelixerr.CodeOf(err); got != tc.want {
				t.Fatalf("rpcFailed(%s) code = %s, want %s", tc.g, got, tc.want)
			}
			// The underlying status must remain inspectable through the wrap.
			if ws, ok := status.FromError(err); !ok || ws.Code() != tc.g {
				t.Fatalf("status.Code(rpcFailed err) lost the original status %s", tc.g)
			}
			// The human message should mention the operation.
			if !contains(err.Error(), "test operation") {
				t.Fatalf("rpcFailed error message lost the operation context: %q", err.Error())
			}
		})
	}
}

// TestRPCFailedNilAndPlain verifies rpcFailed passes nil through and wraps
// non-status errors under the connection category (they are transport-level).
func TestRPCFailedNilAndPlain(t *testing.T) {
	if rpcFailed("op", nil) != nil {
		t.Fatal("rpcFailed(nil) should stay nil")
	}
	plain := context.DeadlineExceeded
	err := rpcFailed("op", plain)
	if phelixerr.CodeOf(err) != phelixerr.CodeTimeout {
		t.Fatalf("context deadline should map to TIMEOUT, got %s", phelixerr.CodeOf(err))
	}
}
