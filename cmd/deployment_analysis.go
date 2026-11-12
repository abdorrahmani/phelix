package cmd

import (
	"fmt"
	"io"
	"time"

	"github.com/abdorrahmani/phelix/internal/deploy"
	"github.com/abdorrahmani/phelix/internal/deployanalysis"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/abdorrahmani/phelix/internal/server"
)

// Deployment analysis is an observability layer around the existing deploy
// flows. Recording a report never changes a deploy's outcome: every failure
// here is reported on stderr at most and otherwise ignored, so a successful
// deployment cannot be failed by analysis.

// finalizeDeploymentAnalyses closes any pending observation window for appName
// before starting another deployment of it, so a later deploy never folds the
// previous version's traffic into its own baseline.
func finalizeDeploymentAnalyses(out io.Writer, appName string, cfg *project.Config) {
	if deployanalysis.Disabled(cfg) {
		return
	}
	if _, err := deployanalysis.FinalizeDue(appName, deployanalysis.FromProject(cfg)); err != nil {
		fmt.Fprintf(out, "  Warning: could not finalize previous deployment analysis: %v\n", err)
	}
	phelixgrpc.SyncDeploymentAnalyses(appName)
}

// beginDeploymentAnalysis records a successful deployment so the impact report
// can be finalized once its observation window elapses. The baseline is
// aggregated now, while the metric history still covers it.
func beginDeploymentAnalysis(out io.Writer, appName, appID, strategy string, previousVer, newVer int, tracker *deploy.Tracker, cfg *project.Config, completedAt time.Time) {
	if deployanalysis.Disabled(cfg) {
		return
	}
	input := deploymentAnalysisInput(appName, appID, strategy, previousVer, newVer, tracker, "", "", "", completedAt)
	if err := deployanalysis.Begin(input, deployanalysis.FromProject(cfg)); err != nil {
		fmt.Fprintf(out, "  Warning: could not queue deployment analysis: %v\n", err)
	}
}

// recordFailedDeploymentAnalysis writes an immediate report for a deployment
// that never served traffic. The report has no post-deployment window, so its
// metrics stay unavailable and its score is unavailable rather than perfect.
func recordFailedDeploymentAnalysis(out io.Writer, appName, appID, strategy string, previousVer, newVer int, tracker *deploy.Tracker, cfg *project.Config, outcome, failureCode string, completedAt time.Time) {
	if deployanalysis.Disabled(cfg) {
		return
	}
	input := deploymentAnalysisInput(appName, appID, strategy, previousVer, newVer, tracker, outcome, failureCode, "", completedAt)
	rep := deployanalysis.Analyze(input, deployanalysis.WindowAggregate{}, deployanalysis.WindowAggregate{}, deployanalysis.FromProject(cfg))
	rep.Outcome = outcome
	if err := deployanalysis.SaveReport(appName, rep); err != nil {
		fmt.Fprintf(out, "  Warning: could not record deployment analysis: %v\n", err)
		return
	}
	phelixgrpc.SyncDeploymentAnalyses(appName)
}

func deploymentAnalysisInput(appName, appID, strategy string, previousVer, newVer int, tracker *deploy.Tracker, outcome, failureCode, serverID string, completedAt time.Time) deployanalysis.ReportInput {
	now := time.Now().UTC()
	if completedAt.IsZero() {
		completedAt = now
	}
	input := deployanalysis.ReportInput{
		DeploymentID:    tracker.DeploymentID(),
		AppName:         appName,
		AppID:           appID,
		ServerID:        serverID,
		Strategy:        strategy,
		PreviousVersion: deployVersionLabel(previousVer),
		NewVersion:      deployVersionLabel(newVer),
		Outcome:         outcome,
		FailureCode:     failureCode,
		DeployedAt:      now,
		CompletedAt:     completedAt,
	}
	if snap := tracker.Snapshot(); snap != nil {
		input.DeploymentID = snap.DeploymentID
		input.Strategy = firstNonEmpty(snap.Strategy, strategy)
		input.ServerID = firstNonEmpty(serverID, server.GetServerID())
		if snap.StartedAt.Before(input.DeployedAt) {
			input.DeployedAt = snap.StartedAt
		}
		if snap.Rollout != nil {
			input.Rollout = rolloutFromSnapshot(snap.Rollout)
		}
	}
	if input.DeploymentID == "" {
		input.DeploymentID = fmt.Sprintf("local-%d", completedAt.UnixNano())
	}
	input.AnalysisID = deployanalysis.NewAnalysisID(appName, input.DeploymentID, input.NewVersion, input.DeployedAt)
	return input
}

func rolloutFromSnapshot(r *deploy.RolloutInfo) *deployanalysis.Rollout {
	if r == nil {
		return nil
	}
	out := &deployanalysis.Rollout{
		Strategy:    r.Strategy,
		Promoted:    r.Status == "promoted",
		TotalSteps:  r.TotalSteps,
		Reason:      r.Reason,
		FailureCode: r.AbortCode,
	}
	if !out.Promoted && r.Status != "running" {
		out.FailedStep = r.StepIndex + 1
	}
	if v := r.Verification; v != nil {
		out.Verification = &deployanalysis.RolloutVerification{
			CanaryErrorRate:   v.CanaryErrorRate,
			BaselineErrorRate: v.BaselineErrorRate,
			CanaryP95MS:       v.CanaryP95Ms,
			BaselineP95MS:     v.BaselineP95Ms,
			CanaryRequests:    v.CanaryRequests,
			CanaryErrors:      v.CanaryErrors,
			BaselineRequests:  v.BaselineRequests,
			Passed:            v.Passed,
			SkipReason:        v.SkipReason,
		}
	}
	return out
}

func deployVersionLabel(ver int) string {
	if ver <= 0 {
		return ""
	}
	return fmt.Sprintf("v%d", ver)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
