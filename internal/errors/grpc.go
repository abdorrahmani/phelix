package phelixerr

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// FromGRPC maps a gRPC error onto a Phelix structured error. The original
// gRPC status is preserved as the cause so --debug can still reveal the
// transport detail, while the normal CLI surface shows the mapped category.
//
//   - nil errors pass through as nil.
//   - Non-gRPC errors are returned unchanged (they may already be structured).
//   - err == context.DeadlineExceeded / codes.DeadlineExceeded map to Timeout.
func FromGRPC(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Code: CodeTimeout, Message: "operation timed out", Err: err}
	}
	if AsError(err) != nil {
		return err // already structured; don't double-wrap
	}
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	code := grpcToCode(st.Code())
	msg := st.Message()
	if msg == "" {
		msg = "remote operation failed"
	}
	return &Error{Code: code, Message: fmt.Sprintf("%s: %s", msg, code), Err: err}
}

func grpcToCode(g codes.Code) Code {
	switch g {
	case codes.InvalidArgument:
		return CodeInvalidArgument
	case codes.NotFound:
		return CodeNotFound
	case codes.AlreadyExists:
		return CodeAlreadyExists
	case codes.Unauthenticated:
		return CodeUnauthenticated
	case codes.PermissionDenied:
		return CodePermissionDenied
	case codes.DeadlineExceeded:
		return CodeTimeout
	case codes.Unavailable, codes.Canceled:
		return CodeConnection
	case codes.ResourceExhausted:
		return CodeServer
	case codes.Internal:
		return CodeServer
	case codes.Unimplemented:
		return CodeUnknown
	default:
		return CodeGRPC
	}
}
