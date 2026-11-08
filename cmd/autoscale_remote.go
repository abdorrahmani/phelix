package cmd

import (
	"context"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/authz"
	"github.com/abdorrahmani/phelix/internal/autoscale"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/grpc"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/project"
)

// Remote autoscaling command implementation. These run inside the monitor
// daemon when the backend sends autoscale_* over the MonitorStream, using
// the same project/autoscale helpers the local `phelix autoscale` commands
// use: there is no second implementation of the toggle or evaluation logic.

// RemoteAutoscaleCommand dispatches one autoscale command.
func RemoteAutoscaleCommand(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
	switch req.GetType() {
	case grpc.AutoscaleCommandStatus:
		return remoteAutoscaleStatus(req)
	case grpc.AutoscaleCommandHistory:
		return remoteAutoscaleHistory(req)
	case grpc.AutoscaleCommandEnable:
		return remoteAutoscaleToggle(req, true)
	case grpc.AutoscaleCommandDisable:
		return remoteAutoscaleToggle(req, false)
	case grpc.AutoscaleCommandPause:
		return remoteAutoscalePause(req, true)
	case grpc.AutoscaleCommandResume:
		return remoteAutoscalePause(req, false)
	case grpc.AutoscaleCommandEvaluate:
		return remoteAutoscaleEvaluate(req)
	default:
		return remoteAutoscaleError(req, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"unknown autoscale command type %q", req.GetType()))
	}
}

func remoteAutoscaleError(req *pb.MonitorCommandRequest, err error) *pb.MonitorCommandResult {
	return &pb.MonitorCommandResult{Status: "error", Error: err.Error()}
}

// autoscaleCommandTarget resolves the app reference from the request.
func autoscaleCommandTarget(req *pb.MonitorCommandRequest) (autoscale.AppRef, error) {
	if err := app.Manager.LoadState(); err != nil {
		return autoscale.AppRef{}, phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load app state", err)
	}
	appInfo, err := GetAppInfo(req.GetAppName())
	if err != nil {
		return autoscale.AppRef{}, err
	}
	return autoscale.AppRef{Name: appInfo.Name, ID: appInfo.ID, Directory: appInfo.Directory}, nil
}

func remoteAutoscaleStatus(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
	ref, err := autoscaleCommandTarget(req)
	if err != nil {
		return remoteAutoscaleError(req, err)
	}
	state, err := deploy.Load(ref.Name)
	if err != nil {
		return remoteAutoscaleError(req, err)
	}
	pause, err := autoscale.LoadPauseState(ref.Name)
	if err != nil {
		return remoteAutoscaleError(req, err)
	}
	st := &pb.AutoscaleStatusState{
		App:      ref.Name,
		Mode:     string(state.Mode),
		Replicas: int32(len(state.Replicas)),
		Paused:   pause.Paused,
	}
	if pause.Paused && !pause.PausedAt.IsZero() {
		st.PausedAt = pause.PausedAt.UTC().Format(time.RFC3339)
		st.PausedBy = pause.PausedBy
	}
	if cfg, cfgErr := project.Load(ref.Directory); cfgErr == nil && cfg != nil && cfg.Deploy != nil && cfg.Deploy.Autoscaling != nil {
		s := cfg.Deploy.Autoscaling.Resolve(cfg.Deploy.Replicas)
		st.Enabled = s.Enabled
		st.MinReplicas = int32(s.MinReplicas)
		st.MaxReplicas = int32(s.MaxReplicas)
		st.Interval = s.Interval.String()
		st.CpuScaleUp = s.ScaleUpCPU
		st.CpuScaleDown = s.ScaleDownCPU
		st.MemoryScaleUp = s.ScaleUpMemory
		st.MemoryScaleDown = s.ScaleDownMemory
		st.RpsScaleUp = s.ScaleUpRPS
		st.RpsScaleDown = s.ScaleDownRPS
		st.P95ScaleUp = s.ScaleUpP95.String()
		st.P95ScaleDown = s.ScaleDownP95.String()
		st.P99ScaleUp = s.ScaleUpP99.String()
		st.P99ScaleDown = s.ScaleDownP99.String()
		st.WindowsUp = int32(s.ScaleUpWindows)
		st.WindowsDown = int32(s.ScaleDownWindows)
		st.CooldownUp = s.ScaleUpCooldown.String()
		st.CooldownDown = s.ScaleDownCooldown.String()
		st.StabilizationWindow = s.StabilizationWindow.String()
	}
	return &pb.MonitorCommandResult{
		Status:          "success",
		AutoscaleStatus: st,
	}
}

func remoteAutoscaleHistory(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
	ref, err := autoscaleCommandTarget(req)
	if err != nil {
		return remoteAutoscaleError(req, err)
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = 20
	}
	records, err := autoscale.ReadHistory(ref.Name, limit)
	if err != nil {
		return remoteAutoscaleError(req, err)
	}
	entries := make([]*pb.AutoscaleHistoryEntry, 0, len(records))
	for _, rec := range records {
		entries = append(entries, &pb.AutoscaleHistoryEntry{
			Time:      rec.Time,
			Kind:      rec.Kind,
			Direction: rec.Direction,
			From:      int32(rec.From),
			To:        int32(rec.To),
			Reason:    rec.Reason,
			Error:     rec.Error,
		})
	}
	return &pb.MonitorCommandResult{Status: "success", AutoscaleHistory: entries}
}

func remoteAutoscaleToggle(req *pb.MonitorCommandRequest, enabled bool) *pb.MonitorCommandResult {
	ref, err := autoscaleCommandTarget(req)
	if err != nil {
		return remoteAutoscaleError(req, err)
	}
	if ref.Directory == "" {
		return remoteAutoscaleError(req, phelixerr.New(phelixerr.CodeNotFound, "app directory unknown; cannot edit phelix.yaml"))
	}
	if err := project.SetAutoscalingEnabled(ref.Directory, enabled); err != nil {
		return remoteAutoscaleError(req, err)
	}
	logs.Info("autoscale", "remote autoscale %s for %s (request %s)", toggleWord(enabled), ref.Name, req.GetRequestId())
	return &pb.MonitorCommandResult{Status: "success", Enabled: enabled}
}

func remoteAutoscalePause(req *pb.MonitorCommandRequest, pause bool) *pb.MonitorCommandResult {
	ref, err := autoscaleCommandTarget(req)
	if err != nil {
		return remoteAutoscaleError(req, err)
	}
	if pause {
		err = autoscale.Pause(ref.Name, "remote", req.GetReason())
	} else {
		err = autoscale.Resume(ref.Name)
	}
	if err != nil {
		return remoteAutoscaleError(req, err)
	}
	logs.Info("autoscale", "remote autoscale %s for %s (request %s)", pauseWord(pause), ref.Name, req.GetRequestId())
	return &pb.MonitorCommandResult{Status: "success", Paused: pause}
}

func remoteAutoscaleEvaluate(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
	ref, err := autoscaleCommandTarget(req)
	if err != nil {
		return remoteAutoscaleError(req, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	decision, execErr := autoscale.EvaluateOnce(ctx, ref, autoscaleProxyClient())
	logs.Info("autoscale", "remote evaluate of %s: direction %d, %d->%d (%s) (request %s)",
		ref.Name, int(decision.Direction), decision.CurrentReplicas, decision.DesiredReplicas, decision.Reason, req.GetRequestId())
	result := &pb.MonitorCommandResult{
		Status:       "success",
		FromReplicas: int32(decision.CurrentReplicas),
		ToReplicas:   int32(decision.DesiredReplicas),
	}
	if execErr != nil {
		result.Status = "error"
		result.Error = execErr.Error()
	}
	return result
}

func toggleWord(enabled bool) string {
	if enabled {
		return "enable"
	}
	return "disable"
}

func pauseWord(pause bool) string {
	if pause {
		return "pause"
	}
	return "resume"
}

// remoteAutoscaleAuthorize gates remote autoscale mutations through the host
// authorization boundary with the dedicated scale action. The application
// identifier resolves to its stable id before the decision so policies can
// target apps precisely.
func remoteAutoscaleAuthorize(req *pb.MonitorCommandRequest) error {
	if err := app.Manager.LoadState(); err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load app state", err)
	}
	appInfo, err := GetAppInfo(req.GetAppName())
	if err != nil {
		return err
	}
	return authorizeDirectExecution(authz.ActionScale, appInfo.Name, appInfo.ID)
}
