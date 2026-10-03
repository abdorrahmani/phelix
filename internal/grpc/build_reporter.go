package grpc

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/buildreport"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/server"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Build-report telemetry transport: carries a terminal build outcome
// (buildreport.Report + regression deltas, or per-combination matrix reports)
// to the backend over ReportBuildEvent. A build emits exactly one terminal
// event, so — unlike the deployment reporter's streamed events — it is sent
// synchronously with a short-lived or shared client, mirroring sender.go's
// ApplicationEvent path. Fire-and-forget: failures only reach phelix.log and
// never fail the build.

// The build-report telemetry compiled into this build declares its capability.
func init() { RegisterCapability(CapabilityBuildReport) }

var (
	buildReportMu        sync.Mutex
	buildReportUnsupport bool // backend answered UNIMPLEMENTED; stop trying
)

func markBuildReportUnsupported() {
	buildReportMu.Lock()
	if !buildReportUnsupport {
		buildReportUnsupport = true
		logs.WarningFile("grpc", "[gRPC] Build telemetry: backend does not implement ReportBuildEvent; skipping for this run")
	}
	buildReportMu.Unlock()
}

func buildReportUnsupported() bool {
	buildReportMu.Lock()
	defer buildReportMu.Unlock()
	return buildReportUnsupport
}

// SendBuildEvent delivers one build event. An UNIMPLEMENTED reply from an older
// backend disables build telemetry for the process and returns nil, so the
// caller never treats it as a delivery failure (identical to SendDeploymentEvent).
func (c *Client) SendBuildEvent(ev *pb.BuildEvent) error {
	if !c.IsConnected() {
		return phelixerr.New(phelixerr.CodeConnection, "build telemetry: not connected")
	}
	svc := c.GetServiceClient()
	if svc == nil {
		return phelixerr.New(phelixerr.CodeConnection, "build telemetry: no service client")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	authCtx, err := attachAuthMetadata(ctx, server.GetServerID())
	if err != nil {
		return err
	}
	resp, err := svc.ReportBuildEvent(authCtx, ev)
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			markBuildReportUnsupported()
			return nil
		}
		c.reconnectIfNeeded()
		return err
	}
	if !resp.GetAccepted() {
		logs.WarningFile("grpc", "[gRPC] Build event rejected: %s", resp.GetMessage())
	}
	return nil
}

// ReportBuildEventForApp reports one native/classic/docker build outcome. It
// is fire-and-forget: gated on a session and the watched flag (like
// ReportEvent), it builds the event, computes the regression against prior
// comparable builds, and sends it synchronously. Errors only reach phelix.log.
// version is the recorded "vN" number (0 when none); report may be nil.
func ReportBuildEventForApp(appID, appName, buildMode string, success bool, version int, tag, gitCommit string, report *buildreport.Report, errorCode string) {
	ev := newBuildEvent(appID, appName, buildMode, success, version, tag, gitCommit, errorCode)
	if report != nil {
		ev.Report = toProtoBuildReport(report)
		if version > 0 {
			if hist, err := deploy.BuildReportHistory(appName, version); err == nil {
				ev.Report.Regression = toProtoBuildRegression(buildreport.Analyze(report, hist, buildreport.DefaultConfig()))
			}
		}
	}
	sendBuildEvent(ev)
}

// ReportMatrixBuildEventForApp reports one matrix build outcome: every
// combination carries its own build report (deploy.MatrixArtifact.Report).
func ReportMatrixBuildEventForApp(appID, appName string, success bool, version int, tag, gitCommit string, artifacts []deploy.MatrixArtifact, errorCode string) {
	ev := newBuildEvent(appID, appName, "matrix", success, version, tag, gitCommit, errorCode)
	for i := range artifacts {
		ev.Combinations = append(ev.Combinations, toProtoMatrixArtifact(&artifacts[i]))
	}
	sendBuildEvent(ev)
}

// newBuildEvent assembles the common BuildEvent envelope. The session token is
// never set in the body — it travels in gRPC metadata (see attachAuthMetadata);
// user_id is the operator's session id.
func newBuildEvent(appID, appName, buildMode string, success bool, version int, tag, gitCommit, errorCode string) *pb.BuildEvent {
	_ = server.Initialize()
	ev := &pb.BuildEvent{
		ServerId:  server.GetServerID(),
		AppId:     appID,
		AppName:   appName,
		Tag:       tag,
		GitCommit: gitCommit,
		BuildMode: buildMode,
		Success:   success,
		Timestamp: time.Now().UnixMilli(),
		ErrorCode: errorCode,
	}
	if version > 0 {
		ev.Version = "v" + strconv.Itoa(version)
	}
	if sid, _ := loadSessionIdentity(); sid != "" {
		ev.UserId = sid
	}
	return ev
}

// sendBuildEvent delivers one build event via the shared client when connected,
// else a temporary connection. Fire-and-forget: never fails the build, and a
// backend that has already answered UNIMPLEMENTED is skipped.
func sendBuildEvent(ev *pb.BuildEvent) {
	if ev == nil || !app.IsWatched(ev.GetAppId(), ev.GetAppName()) {
		return
	}
	if !sessionAvailable() || buildReportUnsupported() {
		return
	}
	if c := GetClient(); c != nil && c.IsConnected() {
		if err := c.SendBuildEvent(ev); err != nil {
			logs.ErrorFile("grpc", "[gRPC] Build telemetry: send failed: %v", err)
		}
		return
	}
	c := NewClient()
	if err := c.Connect(); err != nil {
		logs.ErrorFile("grpc", "[gRPC] Build telemetry: connect failed: %v", err)
		return
	}
	defer c.Close()
	if err := c.SendBuildEvent(ev); err != nil {
		logs.ErrorFile("grpc", "[gRPC] Build telemetry: send failed: %v", err)
	}
}

// toProtoBuildReport maps buildreport.Report onto the wire. The error summary
// is redacted; a nil report yields nil.
func toProtoBuildReport(r *buildreport.Report) *pb.BuildReport {
	if r == nil {
		return nil
	}
	return &pb.BuildReport{
		Language:          r.Language,
		Compiler:          r.Compiler,
		CompilerVersion:   r.CompilerVersion,
		StartedAt:         unixMilli(r.StartedAt),
		EndedAt:           unixMilli(r.EndedAt),
		DurationMs:        r.DurationMS,
		BuildArgs:         append([]string(nil), r.BuildArgs...),
		CacheStatus:       string(r.Cache.Status),
		CacheSource:       r.Cache.Source,
		ArtifactType:      r.Artifact.Type,
		ArtifactSizeBytes: r.Artifact.SizeBytes,
		ArtifactPlatform:  r.Artifact.Platform,
		Failed:            r.Failed,
		Stage:             r.Stage,
		Error:             phelixerr.Redact(r.Error),
	}
}

// toProtoBuildRegression maps buildreport.Analyze output onto the wire. A nil
// analysis (no baseline) yields nil; a non-comparable metric degrades to its
// skip reason rather than a misleading number.
func toProtoBuildRegression(a *buildreport.Analysis) *pb.BuildRegression {
	if a == nil || (a.Size == nil && a.Duration == nil) {
		return nil
	}
	out := &pb.BuildRegression{}
	if m := a.Size; m != nil {
		out.SizeComparable = m.Comparable
		out.SizePreviousBytes = m.Previous
		out.SizeCurrentBytes = m.Current
		out.SizeDeltaBytes = m.Delta
		out.SizePercentDelta = m.PercentDelta
		out.SizeRegression = m.Regression
		out.HistoryUsed = int32(m.HistoryUsed)
		out.BaselineSizeBytes = m.BaselineAvg
		out.SkipReason = m.SkipReason
	}
	if m := a.Duration; m != nil {
		out.DurationComparable = m.Comparable
		out.DurationPreviousMs = m.Previous
		out.DurationCurrentMs = m.Current
		out.DurationDeltaMs = m.Delta
		out.DurationPercentDelta = m.PercentDelta
		out.DurationRegression = m.Regression
		if out.HistoryUsed == 0 {
			out.HistoryUsed = int32(m.HistoryUsed)
		}
		out.BaselineDurationMs = m.BaselineAvg
		if out.SkipReason == "" {
			out.SkipReason = m.SkipReason
		}
	}
	return out
}

// toProtoMatrixArtifact maps one recorded matrix combination artifact onto the
// wire combination state, carrying its own build report. Errors are redacted.
func toProtoMatrixArtifact(a *deploy.MatrixArtifact) *pb.MatrixCombinationState {
	if a == nil {
		return nil
	}
	artifact := a.Binary
	if artifact == "" {
		artifact = a.ImageTag
	}
	return &pb.MatrixCombinationState{
		ToolchainVersion: a.Version,
		Platform:         a.Platform,
		Status:           a.Status,
		Artifact:         artifact,
		Sha256:           a.SHA256,
		SizeBytes:        a.SizeBytes,
		Error:            phelixerr.Redact(a.Error),
		Report:           toProtoBuildReport(a.Report),
	}
}
