package grpc

import (
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// rpcFailed converts a gRPC RPC failure into a structured Phelix error. The
// code is derived from the gRPC status code so Unauthenticated, PermissionDenied,
// Unavailable and DeadlineExceeded remain distinguishable — not every RPC
// failure is a connection failure. The original status error is preserved as
// the cause so status.Code / errors.Is still inspect it through the wrap.
func rpcFailed(operation string, err error) error {
	if err == nil {
		return nil
	}
	code := phelixerr.CodeConnection
	if mapped := phelixerr.FromGRPC(err); mapped != nil {
		if e := phelixerr.AsError(mapped); e != nil {
			code = e.Code
		}
	}
	if code == phelixerr.CodeUnknown {
		code = phelixerr.CodeConnection
	}
	return phelixerr.Wrapf(code, err, "failed to %s", operation)
}
