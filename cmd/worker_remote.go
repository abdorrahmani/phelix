package cmd

import (
	"context"

	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/logs"
)

// RemoteWorkerScaleCommand executes the remote worker_scale command through
// the same deploy.WorkerScale the local `phelix scale` command uses, under
// the same replica coordinator lock and with the same configured shutdown
// settings. The dispatcher owns idempotency and stamps the identity fields.
func RemoteWorkerScaleCommand(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
	if req.GetType() != phelixgrpc.WorkerCommandScale {
		return remoteWorkerError(req, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"unknown worker command type %q", req.GetType()))
	}
	appInfo, err := GetAppInfo(req.GetAppName())
	if err != nil {
		return remoteWorkerError(req, err)
	}
	if wstate, werr := workerAppState(appInfo.Name); werr != nil || wstate == nil {
		return remoteWorkerError(req, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"%s is not a worker app\nHint: worker_scale manages a type: worker app's replica set", appInfo.Name))
	}
	sig, grace, err := workerStopSettings(appInfo)
	if err != nil {
		return remoteWorkerError(req, err)
	}
	from, to, err := deploy.WorkerScale(context.Background(), appInfo.Name, int(req.GetReplicas()), sig, grace)
	if err != nil {
		phelixgrpc.ReportEvent(appInfo.ID, appInfo.Name, "scale", false, err.Error(), 0, "", "")
		return remoteWorkerError(req, err)
	}
	reconcileAppWithDeploy(appInfo.Name)
	phelixgrpc.ReportEvent(appInfo.ID, appInfo.Name, "scale", true, "", 0, "", "")
	logs.Info("worker", "remote scale of %s: %d -> %d replica(s) (request %s)", appInfo.Name, from, to, req.GetRequestId())
	return &pb.MonitorCommandResult{
		Status:       "success",
		FromReplicas: int32(from),
		ToReplicas:   int32(to),
	}
}

// remoteWorkerError builds a minimal error result; the dispatcher stamps
// RequestId/Command/AppName/Timestamp and redacts the message.
func remoteWorkerError(req *pb.MonitorCommandRequest, err error) *pb.MonitorCommandResult {
	return &pb.MonitorCommandResult{
		Status:    "error",
		Error:     err.Error(),
		ErrorCode: string(phelixerr.CodeOf(err)),
	}
}
