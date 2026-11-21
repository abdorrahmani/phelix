package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/builder"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/monitor"
	"github.com/abdorrahmani/phelix/internal/security"
	"github.com/abdorrahmani/phelix/internal/server"
	"github.com/abdorrahmani/phelix/internal/supplychain"
)

type supplyChainAnswer struct {
	Report    *supplychain.Report   `json:"report,omitempty"`
	Artifacts []*supplychain.Report `json:"artifacts,omitempty"`
	Total     int                   `json:"total"`
	Offset    int                   `json:"offset"`
	HasMore   bool                  `json:"has_more"`
	Policy    *supplychain.Policy   `json:"policy,omitempty"`
	Remote    *security.RemoteState `json:"remote,omitempty"`
	Document  []byte                `json:"document,omitempty"`
	Format    string                `json:"format,omitempty"`
	Output    string                `json:"output,omitempty"`
	Truncated bool                  `json:"truncated,omitempty"`
}

func supplyChainResult(req *pb.MonitorCommandRequest, answer supplyChainAnswer, operationErr error) *pb.MonitorCommandResult {
	raw, err := json.Marshal(answer)
	if err != nil {
		return securityCommandError(req, err)
	}
	result := &pb.MonitorCommandResult{Status: "success", SupplyChainJson: raw}
	if operationErr != nil {
		result.Status = "error"
		result.Error = phelixerr.Redact(operationErr.Error())
		result.ErrorCode = string(phelixerr.CodeOf(operationErr))
		if result.ErrorCode == "" {
			result.ErrorCode = "SECURITY_VERIFICATION_FAILED"
		}
		if errors.Is(operationErr, context.Canceled) {
			result.ErrorCode = string(phelixerr.CodeCancelled)
			result.Error = "operation was cancelled"
		} else if errors.Is(operationErr, context.DeadlineExceeded) {
			result.ErrorCode = string(phelixerr.CodeTimeout)
			result.Error = "operation timed out"
		}
	}
	return result
}

func remoteSupplyChainCommand(ctx context.Context, req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
	if err := phelixgrpc.ValidateSupplyChainCommand(req); err != nil {
		return securityCommandError(req, err)
	}
	if req.GetType() == phelixgrpc.SecurityRemoteStatus || req.GetType() == phelixgrpc.SecurityRemoteEnable || req.GetType() == phelixgrpc.SecurityRemoteDisable {
		state, err := security.LoadRemoteState(server.DataDir())
		if err != nil {
			return securityCommandError(req, err)
		}
		if req.GetType() != phelixgrpc.SecurityRemoteStatus {
			if err := remoteHostSecurityAuthorize(req); err != nil {
				return securityCommandError(req, err)
			}
			state, err = security.SetRemoteSecurity(server.DataDir(), req.GetType() == phelixgrpc.SecurityRemoteEnable)
			if err != nil {
				return securityCommandError(req, err)
			}
		}
		return supplyChainResult(req, supplyChainAnswer{Remote: &state}, nil)
	}
	target, err := remoteSecurityTarget(req.GetAppName())
	if err != nil {
		return securityCommandError(req, err)
	}
	if req.GetType() == phelixgrpc.SecurityAppStatus || req.GetType() == phelixgrpc.SecurityAppLogs || req.GetType() == phelixgrpc.SecurityAppBuild {
		return remoteApplicationInspection(ctx, req, target)
	}
	if err := remoteSecurityConsent(target); err != nil {
		return securityCommandError(req, err)
	}
	store := supplychain.NewStore(server.DataDir())
	opts := req.GetSecurity()
	switch req.GetType() {
	case phelixgrpc.SecurityArtifacts:
		limit := int(opts.GetLimit())
		if limit == 0 {
			limit = 20
		}
		items, err := store.List(target.appID, limit, int(opts.GetOffset()))
		if err != nil {
			return securityCommandError(req, err)
		}
		total, err := store.Count(target.appID)
		if err != nil {
			return securityCommandError(req, err)
		}
		for _, item := range items {
			item.Inventory = nil
			item.Scan = nil
			item.Licenses = nil
			item.Documents = nil
		}
		return supplyChainResult(req, supplyChainAnswer{Artifacts: items, Total: total, Offset: int(opts.GetOffset()), HasMore: int(opts.GetOffset())+len(items) < total}, nil)
	case phelixgrpc.SecurityPolicy, phelixgrpc.SecurityPolicySet:
		policy, err := store.LoadPolicy(target.appID)
		if err != nil {
			return securityCommandError(req, err)
		}
		if req.GetType() == phelixgrpc.SecurityPolicySet {
			decoder := json.NewDecoder(strings.NewReader(opts.GetPolicyJson()))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&policy); err != nil {
				return securityCommandError(req, err)
			}
			if err := store.SavePolicy(target.appID, policy); err != nil {
				return securityCommandError(req, err)
			}
		}
		return supplyChainResult(req, supplyChainAnswer{Policy: &policy}, nil)
	}
	report, err := store.ForBuild(target.appID, int(opts.GetVersion()), opts.GetArtifactDigest(), opts.GetBuildId())
	if err != nil {
		return securityCommandError(req, err)
	}
	switch req.GetType() {
	case phelixgrpc.SecurityArtifactDetail:
		return supplyChainResult(req, supplyChainAnswer{Report: report}, nil)
	case phelixgrpc.SecuritySBOM, phelixgrpc.SecurityProvenance:
		ref := report.Provenance
		if req.GetType() == phelixgrpc.SecuritySBOM {
			ref = supplychain.Document{}
			for _, document := range report.SBOMs {
				if strings.HasPrefix(document.Format, opts.GetFormat()) {
					ref = document
					break
				}
			}
		}
		raw, err := store.Document(ref.SHA256)
		if err != nil {
			return securityCommandError(req, err)
		}
		return supplyChainResult(req, supplyChainAnswer{Document: raw, Format: ref.Format}, nil)
	case phelixgrpc.SecurityArtifactRescan:
		err := supplychain.Rescan(ctx, store, report)
		err = errors.Join(err, ctx.Err())
		return supplyChainResult(req, supplyChainAnswer{Report: report}, err)
	case phelixgrpc.SecurityArtifactVerify:
		path, err := remoteArtifactPath(target, report)
		if err != nil {
			return securityCommandError(req, err)
		}
		verifyErr := supplychain.Verify(ctx, store, report, path)
		return supplyChainResult(req, supplyChainAnswer{Report: report}, errors.Join(verifyErr, store.Save(report)))
	default:
		return securityCommandError(req, phelixerr.New(phelixerr.CodeInvalidArgument, "unsupported supply chain operation"))
	}
}

func remoteArtifactPath(target *securityTarget, report *supplychain.Report) (string, error) {
	versions, err := deploy.LoadVersions(target.appName)
	if err != nil {
		return "", err
	}
	for _, version := range versions.Versions {
		if version.Version != report.Version {
			continue
		}
		if version.DockerImage != "" {
			return "", errors.New("container image verification is unavailable for native artifact reports")
		}
		for _, combination := range version.MatrixArtifacts {
			if combination.SHA256 == report.Artifact.SHA256 && combination.Binary != "" && (report.BuildID == "" || (combination.Security != nil && combination.Security.BuildID == report.BuildID)) {
				return combination.Binary, nil
			}
		}
		dir, err := deploy.AppDataDir(target.appName)
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, "builds", fmt.Sprintf("v%d", report.Version), "binary"), nil
	}
	return "", errors.New("artifact version is not registered")
}

func newRemoteSecurityHandler() phelixgrpc.SecurityHandlerFunc {
	type runningOperation struct {
		appID  string
		cancel context.CancelFunc
	}
	var mu sync.Mutex
	running := make(map[string]runningOperation)
	return func(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
		if req.GetType() == phelixgrpc.SecurityOperationCancel {
			if err := phelixgrpc.ValidateSupplyChainCommand(req); err != nil {
				return securityCommandError(req, err)
			}
			mu.Lock()
			operation, found := running[req.GetSecurity().GetScanId()]
			if found && operation.appID == req.GetAppName() {
				operation.cancel()
			}
			mu.Unlock()
			if !found || operation.appID != req.GetAppName() {
				return securityCommandError(req, phelixerr.New(phelixerr.CodeNotFound, "no cancellable operation exists for this application"))
			}
			return &pb.MonitorCommandResult{Status: "success"}
		}
		if req.GetType() != phelixgrpc.SecurityArtifactRescan && req.GetType() != phelixgrpc.SecurityArtifactVerify && req.GetType() != phelixgrpc.SecurityAppBuild && req.GetType() != phelixgrpc.SecurityCommandRun {
			return RemoteSecurityCommand(req)
		}
		timeout := 2 * time.Minute
		if req.GetType() == phelixgrpc.SecurityAppBuild {
			timeout = 30 * time.Minute
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		mu.Lock()
		if len(running) >= 4 {
			mu.Unlock()
			return securityCommandError(req, phelixerr.New(phelixerr.CodeUnavailable, "artifact security operation capacity reached"))
		}
		running[req.GetRequestId()] = runningOperation{appID: req.GetAppName(), cancel: cancel}
		mu.Unlock()
		defer func() {
			mu.Lock()
			delete(running, req.GetRequestId())
			mu.Unlock()
		}()
		if req.GetType() == phelixgrpc.SecurityCommandRun {
			target, err := remoteSecurityTarget(req.GetAppName())
			if err != nil {
				return securityCommandError(req, err)
			}
			if err := remoteSecurityConsent(target); err != nil {
				return securityCommandError(req, err)
			}
			return remoteSecurityScanRunContext(ctx, req, target)
		}
		return remoteSupplyChainCommand(ctx, req)
	}
}

func remoteApplicationInspection(ctx context.Context, req *pb.MonitorCommandRequest, target *securityTarget) *pb.MonitorCommandResult {
	if req.GetType() == phelixgrpc.SecurityAppStatus {
		reconcileAppWithDeploy(target.appID)
		status, err := app.Manager.StatusApplication(target.appID)
		if err != nil {
			return securityCommandError(req, err)
		}
		raw, err := json.Marshal(buildStatusResult(status, collectStatusObservation(target.appName)))
		if err != nil {
			return securityCommandError(req, err)
		}
		return supplyChainResult(req, supplyChainAnswer{Output: string(raw)}, nil)
	}
	info, err := GetAppInfo(target.appID)
	if err != nil {
		return securityCommandError(req, err)
	}
	if req.GetType() == phelixgrpc.SecurityAppBuild {
		release, err := deploy.AcquireDeployLock(target.appName, "build")
		if err != nil {
			return securityCommandError(req, err)
		}
		defer release()
		dir, err := deploy.AppDataDir(target.appName)
		if err != nil {
			return securityCommandError(req, err)
		}
		if err := os.MkdirAll(dir, 0700); err != nil {
			return securityCommandError(req, err)
		}
		temp, err := os.MkdirTemp(dir, ".remote-build-")
		if err != nil {
			return securityCommandError(req, err)
		}
		defer os.RemoveAll(temp)
		path := filepath.Join(temp, "binary")
		manager := builder.NewBuildManager()
		manager.SetContext(ctx)
		manager.SetTimeout(30 * time.Minute)
		report, err := buildApplication(info.ID, nil, manager, path)
		if err != nil {
			return securityCommandError(req, err)
		}
		recorded, err := deploy.RecordFreshBuild(info.Name, info.ID, path, deploy.DetectGitCommit(info.Directory), "", report, deploy.DefaultRetention{Max: 5}, &colorLogger{})
		if err != nil {
			return securityCommandError(req, err)
		}
		return supplyChainResult(req, supplyChainAnswer{Output: fmt.Sprintf("Built and recorded %s v%d. Deployment was not requested.", info.Name, recorded.Version)}, nil)
	}
	if err := deploy.RefreshSecretCoverage(app.AppListItem{ID: info.ID, Name: info.Name, Directory: info.Directory, PID: info.PID, StartTicks: info.StartTicks, SecretEnvs: info.SecretEnvs}); err != nil {
		return securityCommandError(req, err)
	}
	targets := monitor.AppLogTargets(app.AppListItem{ID: info.ID, Name: info.Name, PID: info.PID, Status: info.Status})
	var output strings.Builder
	truncated := len(targets) > 16
	for i, logTarget := range targets {
		if i >= 16 {
			break
		}
		path := logTarget.Path
		if path == "" {
			path = logs.AppLogPath(info.ID)
		}
		lines, partial, err := readRemoteLog(path)
		if err != nil {
			return securityCommandError(req, err)
		}
		truncated = truncated || partial
		for _, line := range lines {
			line = phelixerr.Redact(line)
			if output.Len()+len(line)+1 > 65536 {
				truncated = true
				break
			}
			output.WriteString(line)
			output.WriteByte('\n')
		}
	}
	return supplyChainResult(req, supplyChainAnswer{Output: output.String(), Truncated: truncated}, nil)
}

func readRemoteLog(path string) ([]string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	start := max(int64(0), stat.Size()-65536)
	raw, err := io.ReadAll(io.NewSectionReader(f, start, 65536))
	if err != nil {
		return nil, false, err
	}
	partial := start > 0
	if start > 0 {
		if newline := bytes.IndexByte(raw, '\n'); newline >= 0 {
			raw = raw[newline+1:]
		} else {
			raw = nil
		}
	}
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		partial = true
		raw = raw[:bytes.LastIndexByte(raw, '\n')+1]
	}
	lines, total, err := readLogHistory(bytes.NewReader(raw), 100, nil)
	return lines, partial || total > 100, err
}
