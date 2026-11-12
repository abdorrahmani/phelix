package grpc

import (
	"context"
	"sync"
	"time"

	"github.com/abdorrahmani/phelix/internal/deployanalysis"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/server"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Deployment analysis transport. Reports are stored locally first and then
// delivered from a durable queue, so a deployment performed offline is
// synchronized on a later attempt without duplicates. Nothing here blocks a
// deployment: delivery failures leave the report queued and only reach
// phelix.log.

var (
	deploymentAnalysisMu          sync.Mutex
	deploymentAnalysisUnsupported bool
	deploymentAnalysisInFlight    sync.Map
)

func init() { RegisterCapability(CapabilityDeploymentAnalysis) }

func deploymentAnalysisUnsupportedBackend() bool {
	deploymentAnalysisMu.Lock()
	defer deploymentAnalysisMu.Unlock()
	return deploymentAnalysisUnsupported
}

func markDeploymentAnalysisUnsupported() {
	deploymentAnalysisMu.Lock()
	if !deploymentAnalysisUnsupported {
		deploymentAnalysisUnsupported = true
		logs.WarningFile("grpc", "[gRPC] Deployment analysis: backend does not implement ReportDeploymentAnalysis; keeping reports queued")
	}
	deploymentAnalysisMu.Unlock()
}

// SendDeploymentAnalysis delivers one report. An UNIMPLEMENTED reply leaves the
// report queued for a future backend.
func (c *Client) SendDeploymentAnalysis(ev *pb.DeploymentAnalysisEvent) error {
	if !c.IsConnected() {
		return phelixerr.New(phelixerr.CodeConnection, "deployment analysis: not connected")
	}
	svc := c.GetServiceClient()
	if svc == nil {
		return phelixerr.New(phelixerr.CodeConnection, "deployment analysis: no service client")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	authCtx, err := attachAuthMetadata(ctx, server.GetServerID())
	if err != nil {
		return err
	}
	resp, err := svc.ReportDeploymentAnalysis(authCtx, ev)
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			markDeploymentAnalysisUnsupported()
			return err
		}
		c.reconnectIfNeeded()
		return err
	}
	if !resp.GetAccepted() {
		logs.WarningFile("grpc", "[gRPC] Deployment analysis rejected: %s", resp.GetMessage())
	}
	return nil
}

// SyncDeploymentAnalyses delivers every queued report for one app. It is
// synchronous and bounded, suitable for short-lived CLI commands.
func SyncDeploymentAnalyses(appName string) {
	if appName == "" || !sessionAvailable() || deploymentAnalysisUnsupportedBackend() {
		return
	}
	batch, err := deployanalysis.SyncBatch(appName, 50)
	if err != nil || len(batch) == 0 {
		return
	}
	client := NewClient()
	if err := client.Connect(); err != nil {
		logs.WarningFile("grpc", "[gRPC] Deployment analysis: connect failed: %v", err)
		return
	}
	defer client.Close()

	delivered := make([]string, 0, len(batch))
	for i := range batch {
		if deploymentAnalysisUnsupportedBackend() {
			break
		}
		ev := &pb.DeploymentAnalysisEvent{Report: toProtoDeploymentAnalysis(&batch[i])}
		if err := client.SendDeploymentAnalysis(ev); err != nil {
			logs.WarningFile("grpc", "[gRPC] Deployment analysis: send failed for %s: %v", batch[i].AnalysisID, err)
			break
		}
		delivered = append(delivered, batch[i].AnalysisID)
	}
	if err := deployanalysis.AckSynced(appName, delivered); err != nil {
		logs.ErrorFile("grpc", "[gRPC] Deployment analysis: failed to acknowledge delivery: %v", err)
	}
}

// SyncDeploymentAnalysesAsync runs the same synchronization without blocking
// the monitor tick. Concurrent requests for one app collapse into one run.
func SyncDeploymentAnalysesAsync(appName string) {
	if appName == "" {
		return
	}
	if _, loaded := deploymentAnalysisInFlight.LoadOrStore(appName, true); loaded {
		return
	}
	go func() {
		defer deploymentAnalysisInFlight.Delete(appName)
		SyncDeploymentAnalyses(appName)
	}()
}

func toProtoDeploymentAnalysis(rep *deployanalysis.Report) *pb.DeploymentAnalysisReport {
	out := &pb.DeploymentAnalysisReport{
		SchemaVersion:   int32(rep.SchemaVersion),
		AnalysisId:      rep.AnalysisID,
		DeploymentId:    rep.DeploymentID,
		AppId:           rep.AppID,
		AppName:         rep.AppName,
		ServerId:        rep.ServerID,
		Strategy:        rep.Strategy,
		PreviousVersion: rep.PreviousVersion,
		NewVersion:      rep.NewVersion,
		Outcome:         rep.Outcome,
		Status:          rep.Status,
		Recommendation:  rep.Recommendation,
		ImpactScore:     rep.ImpactScore,
		ImpactGrade:     rep.ImpactGrade,
		DeployedAt:      rep.DeployedAt.UnixMilli(),
		CompletedAt:     rep.CompletedAt.UnixMilli(),
		AnalyzedAt:      rep.AnalyzedAt.UnixMilli(),
		Baseline: &pb.DeploymentAnalysisWindow{
			StartedAt: rep.Baseline.StartedAt.UnixMilli(),
			EndedAt:   rep.Baseline.EndedAt.UnixMilli(),
			Samples:   int32(rep.Baseline.Samples),
		},
		Observed: &pb.DeploymentAnalysisWindow{
			StartedAt: rep.Observed.StartedAt.UnixMilli(),
			EndedAt:   rep.Observed.EndedAt.UnixMilli(),
			Samples:   int32(rep.Observed.Samples),
		},
		FailureCode: rep.FailureCode,
	}
	for i := range rep.Metrics {
		m := &rep.Metrics[i]
		out.Metrics = append(out.Metrics, &pb.DeploymentAnalysisMetric{
			Key:             m.Key,
			Label:           m.Label,
			Unit:            m.Unit,
			Status:          m.Status,
			Before:          m.Before,
			After:           m.After,
			Delta:           m.Delta,
			PercentDelta:    m.Percent,
			BeforePeak:      m.BeforePeak,
			AfterPeak:       m.AfterPeak,
			BaselineSamples: int32(m.BaselineSamples),
			ObservedSamples: int32(m.ObservedSamples),
			SkipReason:      m.SkipReason,
		})
	}
	if rep.Rollout != nil {
		r := &pb.DeploymentAnalysisRollout{
			Strategy:    rep.Rollout.Strategy,
			Promoted:    rep.Rollout.Promoted,
			TotalSteps:  int32(rep.Rollout.TotalSteps),
			FailedStep:  int32(rep.Rollout.FailedStep),
			FailureCode: rep.Rollout.FailureCode,
			Reason:      rep.Rollout.Reason,
		}
		if v := rep.Rollout.Verification; v != nil {
			r.Verification = &pb.DeploymentAnalysisVerification{
				CanaryErrorRate:   v.CanaryErrorRate,
				BaselineErrorRate: v.BaselineErrorRate,
				CanaryP95Ms:       v.CanaryP95MS,
				BaselineP95Ms:     v.BaselineP95MS,
				CanaryRequests:    v.CanaryRequests,
				CanaryErrors:      v.CanaryErrors,
				BaselineRequests:  v.BaselineRequests,
				Passed:            v.Passed,
				SkipReason:        v.SkipReason,
			}
		}
		out.Rollout = r
	}
	return out
}
