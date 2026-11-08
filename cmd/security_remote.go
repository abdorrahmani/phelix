package cmd

import (
	"context"
	"strings"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/abdorrahmani/phelix/internal/security"
	"github.com/abdorrahmani/phelix/internal/server"
)

// securityFindingsWireCap bounds the findings per remote response; the
// full record stays in the local store and the truncation flag tells the
// backend the answer was bounded.
const securityFindingsWireCap = 200

// RemoteSecurityCommand exposes scan data only with current local consent.
func RemoteSecurityCommand(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
	target, err := remoteSecurityTarget(req.GetAppName())
	if err != nil {
		return securityCommandError(req, err)
	}
	if err := remoteSecurityConsent(target); err != nil && req.GetType() != phelixgrpc.SecurityCommandConfig {
		return securityCommandError(req, err)
	}

	switch req.GetType() {
	case phelixgrpc.SecurityCommandReport:
		return remoteSecurityReport(req, target)
	case phelixgrpc.SecurityCommandHistory:
		return remoteSecurityHistory(req, target)
	case phelixgrpc.SecurityCommandDetail:
		return remoteSecurityDetail(req, target)
	case phelixgrpc.SecurityCommandAdvisory:
		return remoteSecurityAdvisory(req, target)
	case phelixgrpc.SecurityCommandConfig:
		return remoteSecurityConfig(req, target)
	case phelixgrpc.SecurityCommandRun:
		return remoteSecurityScanRun(req, target)
	case phelixgrpc.SecurityCommandConfigSet:
		return remoteSecurityConfigSet(req, target)
	default:
		return securityCommandError(req, phelixerr.Newf(phelixerr.CodeInvalidArgument, "unknown security command %q", req.GetType()))
	}
}

// appManagerLoadState loads the application registry for target
// resolution.
func appManagerLoadState() error {
	return app.Manager.LoadState()
}

// remoteSecurityTarget resolves the command's app_name to a registered
// application and its project directory.
func remoteSecurityTarget(identifier string) (*securityTarget, error) {
	if err := appManagerLoadState(); err != nil {
		return nil, err
	}
	info, err := GetAppInfo(identifier)
	if err != nil {
		return nil, err
	}
	target := &securityTarget{appID: info.ID, appName: info.Name, identityKey: info.ID, dir: info.Directory}
	target.cfg, err = project.LoadSecurityProject(target.dir, target.appName)
	if err != nil {
		return nil, err
	}
	return target, nil
}

// remoteSecurityConsent checks the local permission to disclose scan data.
func remoteSecurityConsent(target *securityTarget) error {
	if target.cfg == nil || target.cfg.Security == nil || target.cfg.Security.Vulnerability == nil {
		return phelixerr.New(phelixerr.CodeSecurityReportingDisabled,
			"security reporting is not enabled for this application in phelix.yaml (security.vulnerability.reporting: true opts in)")
	}
	if !target.cfg.Security.Vulnerability.ResolvedReporting() {
		return phelixerr.New(phelixerr.CodeSecurityReportingDisabled,
			"security reporting is disabled for this application (security.vulnerability.reporting: true opts in)")
	}
	return nil
}

func securityCommandError(req *pb.MonitorCommandRequest, err error) *pb.MonitorCommandResult {
	return &pb.MonitorCommandResult{
		Status:    "error",
		Error:     phelixerr.Redact(err.Error()),
		ErrorCode: string(phelixerr.CodeOf(err)),
	}
}

func securityCommandSuccess(record *pb.SecurityScanRecord) *pb.MonitorCommandResult {
	return &pb.MonitorCommandResult{Status: "success", SecurityScanRecord: record}
}

// remoteSecurityReport returns the app's latest completed scan with
// findings (filtered), optionally carrying the dependency graph.
func remoteSecurityReport(req *pb.MonitorCommandRequest, target *securityTarget) *pb.MonitorCommandResult {
	store := security.NewScanStore(server.DataDir())
	entry, err := store.Latest(target.identityKey)
	if err != nil {
		return securityCommandError(req, err)
	}
	if entry == nil || entry.ScanID == "" {
		return securityCommandError(req, phelixerr.Newf(phelixerr.CodeScanNotFound,
			"no completed scan found for %s; run phelix security scan first", target.appName))
	}
	record, err := store.LoadScan(entry.ScanID)
	if err != nil {
		return securityCommandError(req, err)
	}
	opts := req.GetSecurity()
	return securityCommandSuccess(recordToProto(req, target, record, opts.GetIncludeGraph(), opts))
}

// remoteSecurityHistory returns a page of the app's scan history, newest
// first. Summaries only: findings stay in the detail query.
func remoteSecurityHistory(req *pb.MonitorCommandRequest, target *securityTarget) *pb.MonitorCommandResult {
	store := security.NewScanStore(server.DataDir())
	opts := req.GetSecurity()
	limit := securityQueryLimitFor(opts)
	offset := 0
	if opts != nil {
		offset = int(opts.GetOffset())
	}
	records, err := store.ListScans(target.identityKey)
	if err != nil {
		return securityCommandError(req, err)
	}
	total := len(records)
	list := &pb.SecurityScanList{Total: int32(total), Offset: int32(offset)}
	for i := offset; i < len(records) && len(list.Scans) < limit; i++ {
		list.Scans = append(list.Scans, recordToProto(req, target, records[i], false, nil))
	}
	list.HasMore = offset+len(list.Scans) < total
	return &pb.MonitorCommandResult{Status: "success", SecurityScanList: list}
}

// remoteSecurityDetail returns one scan by id, optionally with its graph.
func remoteSecurityDetail(req *pb.MonitorCommandRequest, target *securityTarget) *pb.MonitorCommandResult {
	store := security.NewScanStore(server.DataDir())
	scanID := ""
	if opts := req.GetSecurity(); opts != nil {
		scanID = opts.GetScanId()
	}
	record, err := store.LoadScan(scanID)
	if err != nil {
		return securityCommandError(req, err)
	}
	if record.IdentityKey != target.identityKey {
		return securityCommandError(req, phelixerr.Newf(phelixerr.CodeScanNotFound,
			"scan %s belongs to a different application", scanID))
	}
	opts := req.GetSecurity()
	return securityCommandSuccess(recordToProto(req, target, record, opts.GetIncludeGraph(), opts))
}

// remoteSecurityAdvisory serves one advisory from the local cache,
// fetching it on miss exactly like the CLI show command.
func remoteSecurityAdvisory(req *pb.MonitorCommandRequest, target *securityTarget) *pb.MonitorCommandResult {
	id := ""
	if opts := req.GetSecurity(); opts != nil {
		id = opts.GetAdvisoryId()
	}
	store := security.NewScanStore(server.DataDir())
	cache := security.NewAdvisoryCache(store.AdvisoryCacheDir())
	adv, _, err := cache.Get(id)
	cached := err == nil && adv != nil
	if adv == nil {
		client := &security.OSVClient{BaseURL: security.OSVBaseURL()}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		adv, err = client.FetchAdvisory(ctx, id)
		if err != nil {
			return securityCommandError(req, phelixerr.Wrap(phelixerr.CodeConnection,
				"advisory is not cached and OSV.dev could not be reached", err))
		}
		_ = cache.Put(adv, time.Now())
	}
	return &pb.MonitorCommandResult{Status: "success", SecurityAdvisory: advisoryToProto(adv, cached)}
}

// remoteSecurityConfig returns the effective configuration plus scheduler
// status.
func remoteSecurityConfig(req *pb.MonitorCommandRequest, target *securityTarget) *pb.MonitorCommandResult {
	state, err := securityConfigState(target)
	if err != nil {
		return securityCommandError(req, err)
	}
	return &pb.MonitorCommandResult{Status: "success", SecurityConfig: state}
}

// remoteSecurityScanRun triggers a scan of the application. The scan runs
// synchronously under the per-app scan lock: a concurrent scan (local or
// remote) answers UNAVAILABLE instead of duplicating work. The result is
// the completed (or failed) scan record.
func remoteSecurityScanRun(req *pb.MonitorCommandRequest, target *securityTarget) *pb.MonitorCommandResult {
	store := security.NewScanStore(server.DataDir())
	lock, err := store.TryAcquireScanLock(target.identityKey)
	if err != nil {
		return securityCommandError(req, phelixerr.New(phelixerr.CodeUnavailable,
			"a scan is already running for this application; retry after it completes"))
	}
	defer lock.Release()

	opts := security.ScanOptions{
		AppID:       target.appID,
		AppName:     target.appName,
		IdentityKey: target.identityKey,
		Dir:         target.dir,
		Trigger:     security.TriggerScheduled,
		DataDir:     server.DataDir(),
		OSVBaseURL:  security.OSVBaseURL(),
	}
	result, err := security.RunScan(context.Background(), opts)
	if err != nil {
		return securityCommandError(req, err)
	}
	return securityCommandSuccess(recordToProto(req, target, result.Record, false, nil))
}

// remoteSecurityConfigSet applies the requested configuration fields to
// the application's phelix.yaml and returns the resulting state. Fields
// the command does not set keep their current values.
func remoteSecurityConfigSet(req *pb.MonitorCommandRequest, target *securityTarget) *pb.MonitorCommandResult {
	if target.dir == "" {
		return securityCommandError(req, phelixerr.New(phelixerr.CodeInvalidArgument,
			"the application has no project directory"))
	}
	opts := req.GetSecurity()

	store := security.NewScanStore(server.DataDir())
	lock, err := store.AcquireConfigLock(target.identityKey)
	if err != nil {
		return securityCommandError(req, err)
	}
	defer lock.Release()

	cfg, err := project.LoadSecurityProject(target.dir, target.appName)
	if err != nil {
		return securityCommandError(req, err)
	}
	target.cfg = cfg
	if err := remoteSecurityConsent(target); err != nil {
		return securityCommandError(req, err)
	}
	vuln := project.VulnerabilityConfig{ScanInterval: project.ScanInterval(project.ScanIntervalDefault.String()), FailThreshold: project.ScanSeverityDefault}
	if cfg.Security != nil && cfg.Security.Vulnerability != nil {
		vuln = *cfg.Security.Vulnerability
	}
	if opts.GetSetEnabled() {
		vuln.Enabled = opts.GetEnabled()
	}
	if opts.GetScanInterval() != "" {
		vuln.ScanInterval = project.ScanInterval(opts.GetScanInterval())
	}
	if opts.GetFailThreshold() != "" {
		vuln.FailThreshold = project.ScanSeverity(opts.GetFailThreshold())
	}
	if err := project.SaveSecurityConfig(target.dir, &project.SecurityConfig{Vulnerability: &vuln}); err != nil {
		return securityCommandError(req, err)
	}

	refreshed, err := project.Load(target.dir)
	if err != nil {
		return securityCommandError(req, err)
	}
	target.cfg = refreshed
	state, err := securityConfigState(target)
	if err != nil {
		return securityCommandError(req, err)
	}
	return &pb.MonitorCommandResult{Status: "success", SecurityConfig: state}
}

// securityConfigState assembles the config view shared by the read and
// write commands.
func securityConfigState(target *securityTarget) (*pb.SecurityConfigState, error) {
	state := &pb.SecurityConfigState{AppId: target.appID, AppName: target.appName, Reporting: "disabled"}
	if remoteSecurityConsent(target) != nil {
		return state, nil
	}
	if target.cfg != nil && target.cfg.Security != nil && target.cfg.Security.Vulnerability != nil {
		v := target.cfg.Security.Vulnerability
		state.Enabled = v.Enabled
		state.Reporting = boolString(v.ResolvedReporting())
		if interval, err := v.ResolvedScanInterval(); err == nil {
			state.ScanInterval = interval.String()
		}
		state.FailThreshold = string(v.ResolvedFailThreshold())
	}
	store := security.NewScanStore(server.DataDir())
	if entry, err := store.Latest(target.identityKey); err == nil && entry != nil {
		state.LastScanId = entry.ScanID
		if entry.FinishedAt != nil {
			state.LastScanAtMs = entry.FinishedAt.UnixMilli()
		}
		if entry.NextScanAt != nil {
			state.NextScanAtMs = entry.NextScanAt.UnixMilli()
			state.HasNextScan = true
		}
	}
	return state, nil
}

func boolString(b bool) string {
	if b {
		return "enabled"
	}
	return "disabled"
}

// securityQueryLimitFor resolves the history page size.
func securityQueryLimitFor(opts *pb.SecurityCommandOptions) int {
	if opts == nil {
		return 20
	}
	requested := int(opts.GetLimit())
	if requested <= 0 {
		return 20
	}
	if requested > 100 {
		return 100
	}
	return requested
}

// recordToProto converts a persisted scan record into the wire shape,
// applying finding filters and the wire cap. The graph rides only when
// requested.
func recordToProto(req *pb.MonitorCommandRequest, target *securityTarget, record *security.ScanRecord, includeGraph bool, opts *pb.SecurityCommandOptions) *pb.SecurityScanRecord {
	out := &pb.SecurityScanRecord{
		ScanId:        record.ScanID,
		AppId:         record.AppID,
		AppName:       record.AppName,
		Status:        string(record.Status),
		Trigger:       string(record.Trigger),
		Freshness:     record.Freshness,
		Coverage:      record.Coverage,
		Outcome:       record.Outcome,
		PhelixVersion: record.PhelixVersion,
		Warnings:      record.Warnings,
	}
	if !record.StartedAt.IsZero() {
		out.StartedAtMs = record.StartedAt.UnixMilli()
	}
	if record.FinishedAt != nil {
		out.FinishedAtMs = record.FinishedAt.UnixMilli()
	}
	out.Counts = &pb.SecurityScanCounts{
		Modules:             int32(record.Counts.Modules),
		Matchable:           int32(record.Counts.Matchable),
		Evaluated:           int32(record.Counts.Evaluated),
		Affected:            int32(record.Counts.Affected),
		PotentiallyAffected: int32(record.Counts.PotentiallyAffected),
		Unknown:             int32(record.Counts.Unknown),
		NotAffected:         int32(record.Counts.NotAffected),
	}

	for _, finding := range security.FilterFindings(record.Findings, optsToFilter(opts)) {
		if len(out.Findings) >= securityFindingsWireCap {
			out.FindingsTruncated = true
			break
		}
		out.Findings = append(out.Findings, findingToProto(finding))
	}

	if includeGraph && record.Inventory != nil {
		graph := &pb.SecurityGraph{Roots: record.Inventory.Roots}
		for _, mod := range record.Inventory.Modules {
			graph.Nodes = append(graph.Nodes, moduleToProto(mod))
		}
		for _, edge := range record.Inventory.Edges {
			graph.Edges = append(graph.Edges, &pb.SecurityGraphEdge{From: edge.From, To: edge.To})
		}
		out.Graph = graph
	}
	return out
}

// optsToFilter translates wire filters into the domain filter.
func optsToFilter(opts *pb.SecurityCommandOptions) security.FindingFilter {
	if opts == nil {
		return security.FindingFilter{}
	}
	return security.FindingFilter{
		Severity:  opts.GetSeverity(),
		Ecosystem: opts.GetEcosystem(),
		State:     opts.GetState(),
		Package:   opts.GetPackage(),
	}
}

func findingToProto(finding security.Finding) *pb.SecurityFinding {
	return &pb.SecurityFinding{
		AdvisoryId:      finding.AdvisoryID,
		Aliases:         finding.Aliases,
		Package:         finding.Package,
		Ecosystem:       string(finding.Ecosystem),
		Version:         finding.Version,
		VersionResolved: finding.VersionResolved,
		Direct:          finding.Direct,
		Dev:             finding.Dev,
		Optional:        finding.Optional,
		State:           finding.State,
		Reason:          finding.Reason,
		FixedVersion:    finding.FixedVersion,
		Ranges:          finding.Ranges,
		Severity:        finding.Severity,
		SeveritySource:  finding.SeveritySource,
		CvssVectors:     finding.CVSSVectors,
		Summary:         finding.Summary,
		Url:             finding.URL,
		Reachability:    finding.Reachability,
		Confidence:      string(finding.Confidence),
	}
}

func moduleToProto(mod security.Module) *pb.SecurityModule {
	return &pb.SecurityModule{
		Name:             mod.Name,
		Key:              mod.Key,
		Version:          mod.Version,
		Requirement:      mod.Requirement,
		Direct:           mod.Direct,
		Resolved:         mod.Resolved,
		ResolvedFrom:     mod.ResolvedFrom,
		Confidence:       string(mod.Confidence),
		SourceKind:       string(mod.SourceKind),
		SourceDetail:     mod.SourceDetail,
		Matchable:        mod.Matchable,
		Reachable:        mod.Reachable,
		ObservedVersions: mod.ObservedVersions,
		ReplacedFrom:     mod.ReplacedFrom,
		Dev:              mod.Dev,
		Optional:         mod.Optional,
	}
}

func advisoryToProto(adv *security.OSVAdvisory, cached bool) *pb.SecurityAdvisory {
	label, _, vectors := advisorySeverityLabel(adv)
	out := &pb.SecurityAdvisory{
		Id:          adv.ID,
		Aliases:     adv.Aliases,
		Summary:     truncateCell(sanitizeText(adv.Summary), 300),
		Severity:    label,
		CvssVectors: vectors,
		Modified:    adv.Modified,
		FromCache:   cached,
	}
	for _, affected := range adv.Affected {
		out.AffectedPackages = append(out.AffectedPackages, affected.Package.Ecosystem+":"+affected.Package.Name)
	}
	for _, ref := range adv.References {
		if strings.HasPrefix(ref.URL, "http") {
			out.References = append(out.References, ref.URL)
		}
	}
	return out
}
