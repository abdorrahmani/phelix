package cmd

import (
	"context"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/authz"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/logs"
)

// RemoteWorkerCommand dispatches every remote worker command to the same
// deploy functions the local CLI uses, under the same replica coordinator
// lock and with the same configured shutdown settings. The dispatcher owns
// idempotency and stamps the identity fields.
func RemoteWorkerCommand(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
	switch req.GetType() {
	case phelixgrpc.WorkerCommandScale:
		return remoteWorkerScale(req)
	case phelixgrpc.WorkerCommandRestartReplica:
		return remoteWorkerRestartReplica(req)
	case phelixgrpc.ScaleCommand:
		return remoteScale(req)
	default:
		return remoteWorkerError(req, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"unknown worker command type %q", req.GetType()))
	}
}

// remoteScale changes a worker or rolling service replica count through the
// same scaleApplication path the local `phelix scale` command uses.
func remoteScale(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
	appInfo, err := GetAppInfo(req.GetAppName())
	if err != nil {
		return remoteWorkerError(req, err)
	}
	state, err := deploy.Load(appInfo.Name)
	if err != nil {
		return remoteWorkerError(req, err)
	}
	if state.Mode != deploy.ModeWorker && state.Mode != deploy.ModeRolling {
		return remoteWorkerError(req, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"%s is a %s deployment; scale changes replica counts for worker and rolling apps", appInfo.Name, state.Mode))
	}
	if err := validateScaleRange(appInfo.Directory, int(req.GetReplicas())); err != nil {
		return remoteWorkerError(req, err)
	}
	from, to, err := scaleApplication(context.Background(), appInfo, state, int(req.GetReplicas()))
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

// remoteWorkerScale executes worker_scale through deploy.WorkerScale.
func remoteWorkerScale(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
	appInfo, err := workerCommandTarget(req)
	if err != nil {
		return remoteWorkerError(req, err)
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

// remoteWorkerRestartReplica executes worker_restart_replica through
// deploy.WorkerRestartReplica for one slot.
func remoteWorkerRestartReplica(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
	appInfo, err := workerCommandTarget(req)
	if err != nil {
		return remoteWorkerError(req, err)
	}
	sig, grace, err := workerStopSettings(appInfo)
	if err != nil {
		return remoteWorkerError(req, err)
	}
	if err := deploy.WorkerRestartReplica(context.Background(), appInfo.Name, req.GetReplica(), sig, grace); err != nil {
		phelixgrpc.ReportEvent(appInfo.ID, appInfo.Name, "restart", false, err.Error(), 0, "", "")
		return remoteWorkerError(req, err)
	}
	reconcileAppWithDeploy(appInfo.Name)
	phelixgrpc.ReportEvent(appInfo.ID, appInfo.Name, "restart", true, "", 0, "", "")
	logs.Info("worker", "remote restart of %s replica %s (request %s)", appInfo.Name, req.GetReplica(), req.GetRequestId())
	return &pb.MonitorCommandResult{
		Status:  "success",
		Replica: req.GetReplica(),
	}
}

// workerCommandTarget resolves the app and verifies it is a worker before any
// worker command touches state.
func workerCommandTarget(req *pb.MonitorCommandRequest) (*app.AppInfo, error) {
	appInfo, err := GetAppInfo(req.GetAppName())
	if err != nil {
		return nil, err
	}
	if wstate, werr := workerAppState(appInfo.Name); werr != nil || wstate == nil {
		return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"%s is not a worker app\nHint: worker commands manage a type: worker app's replica set", appInfo.Name)
	}
	return appInfo, nil
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

// remoteWorkerAuthorize gates remote replica mutations through the host
// authorization boundary with the dedicated scale action, before any ledger
// entry or state change.
func remoteWorkerAuthorize(req *pb.MonitorCommandRequest) error {
	appInfo, err := GetAppInfo(req.GetAppName())
	if err != nil {
		return err
	}
	return authorizeDirectExecution(authz.ActionScale, appInfo.Name, appInfo.ID)
}
