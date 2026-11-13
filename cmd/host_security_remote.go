package cmd

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/abdorrahmani/phelix/internal/authz"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/hostsecurity"
	"github.com/abdorrahmani/phelix/internal/securityscore"
	"github.com/abdorrahmani/phelix/internal/server"
)

// hostSecurityQueryTimeout bounds one host security query or scan. Scans
// carry their own shorter internal deadline; the outer bound covers store
// access around them.
const hostSecurityQueryTimeout = 10 * time.Minute

// hostSecurityFindingsWireCap bounds the findings per remote record
// response; the full record stays in the local store and the truncation
// flag tells the backend the answer was bounded.
const hostSecurityFindingsWireCap = 200

// hostSecurityQueryLimitDefault bounds the remote page size when the
// request does not carry one.
const hostSecurityQueryLimitDefault = 20

// RemoteHostSecurityCommand answers host_security_* commands. Every command
// except host_security_config requires the local host reporting consent:
// consent is local-only and can never be toggled remotely, so the config
// answer stays available for the dashboard to render the disabled state.
func RemoteHostSecurityCommand(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
	dataDir := server.DataDir()
	if !hostsecurity.HostReportingEnabled(dataDir) && req.GetType() != phelixgrpc.HostSecurityCommandConfig {
		return securityCommandError(req, phelixerr.New(phelixerr.CodeHostSecurityConsentDisabled,
			"host security reporting is disabled on this host; run 'phelix security server config --reporting' to consent"))
	}
	switch req.GetType() {
	case phelixgrpc.HostSecurityCommandReport:
		return remoteHostSecurityReport(req, dataDir)
	case phelixgrpc.HostSecurityCommandHistory:
		return remoteHostSecurityHistory(req, dataDir)
	case phelixgrpc.HostSecurityCommandDetail:
		return remoteHostSecurityDetail(req, dataDir)
	case phelixgrpc.HostSecurityCommandInventory:
		return remoteHostSecurityInventory(req, dataDir)
	case phelixgrpc.HostSecurityCommandConfig:
		return remoteHostSecurityConfig(req, dataDir)
	case phelixgrpc.HostSecurityCommandRebootStatus:
		return remoteHostSecurityRebootStatus(req, dataDir)
	case phelixgrpc.HostSecurityCommandScanRun:
		return remoteHostSecurityScanRun(req, dataDir)
	case phelixgrpc.HostSecurityCommandUpdatePreview:
		return remoteHostSecurityUpdatePreview(req, dataDir)
	case phelixgrpc.HostSecurityCommandUpdateApply:
		return remoteHostSecurityUpdateApply(req, dataDir)
	case phelixgrpc.HostSecurityCommandUpdateVerify:
		return remoteHostSecurityUpdateVerify(req, dataDir)
	default:
		return securityCommandError(req, phelixerr.Newf(phelixerr.CodeInvalidArgument, "unknown host security command %q", req.GetType()))
	}
}

// remoteHostSecurityAuthorize gates remote host security mutations through
// the host authorization boundary. The target is this agent's stable
// identity: host security mutates the host, not an application.
func remoteHostSecurityAuthorize(req *pb.MonitorCommandRequest) error {
	id, err := server.EnsureAgentID()
	if err != nil {
		return err
	}
	return authorizeDirectExecution(authz.ActionHostSecurity, id, id)
}

// hostSecuritySnapshotForConsentingHost builds the latest completed host
// scan summary when reporting consent is enabled. Without consent it
// returns nil so nothing is pushed.
func hostSecuritySnapshotForConsentingHost() *pb.HostSecuritySnapshotSummary {
	dataDir := server.DataDir()
	if !hostsecurity.HostReportingEnabled(dataDir) {
		return nil
	}
	id, err := server.EnsureAgentID()
	if err != nil {
		return nil
	}
	store, err := hostsecurity.OpenStore(dataDir)
	if err != nil {
		return nil
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	scan, err := store.LatestFinished(ctx, id)
	if err != nil {
		return nil
	}
	return hostSecuritySnapshotFromScan(scan)
}

func hostSecurityQueryContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), hostSecurityQueryTimeout)
}

func hostSecurityOpenStore(dataDir string) (*hostsecurity.Store, error) {
	return hostsecurity.OpenStore(dataDir)
}

// hostSecurityPageBounds resolves the request paging: 0 means the default
// page size, the maximum is the wire cap.
func hostSecurityPageBounds(opts *pb.HostSecurityCommandOptions) (int, int) {
	limit := hostSecurityQueryLimitDefault
	if opts != nil && opts.GetLimit() > 0 {
		limit = int(opts.GetLimit())
	}
	offset := 0
	if opts != nil {
		offset = int(opts.GetOffset())
	}
	return limit, offset
}

// remoteHostSecurityReport returns the latest completed host scan with
// filtered, paged findings.
func remoteHostSecurityReport(req *pb.MonitorCommandRequest, dataDir string) *pb.MonitorCommandResult {
	id, err := server.EnsureAgentID()
	if err != nil {
		return securityCommandError(req, err)
	}
	store, err := hostSecurityOpenStore(dataDir)
	if err != nil {
		return securityCommandError(req, err)
	}
	defer store.Close()
	ctx, cancel := hostSecurityQueryContext()
	defer cancel()
	scan, err := store.LatestFinished(ctx, id)
	if err = hostSecurityStoreError(err); err != nil {
		return securityCommandError(req, err)
	}
	opts := req.GetHostSecurity()
	limit, _ := hostSecurityPageBounds(opts)
	page := int(opts.GetFindingsOffset())
	filtered, total := filterServerFindings(scan.Findings, opts.GetSeverity(), opts.GetState(), opts.GetPackage(), limit, page)
	return &pb.MonitorCommandResult{
		Status: "success",
		HostSecurityScanRecord: hostSecurityRecordFromScan(scan, filtered, total,
			page+len(filtered) < total),
	}
}

// remoteHostSecurityHistory returns a page of the host scan history,
// newest first. Summaries only: findings stay in the report and detail
// queries.
func remoteHostSecurityHistory(req *pb.MonitorCommandRequest, dataDir string) *pb.MonitorCommandResult {
	id, err := server.EnsureAgentID()
	if err != nil {
		return securityCommandError(req, err)
	}
	store, err := hostSecurityOpenStore(dataDir)
	if err != nil {
		return securityCommandError(req, err)
	}
	defer store.Close()
	ctx, cancel := hostSecurityQueryContext()
	defer cancel()
	limit, offset := hostSecurityPageBounds(req.GetHostSecurity())
	page, err := store.History(ctx, id, limit, offset)
	if err = hostSecurityStoreError(err); err != nil {
		return securityCommandError(req, err)
	}
	list := &pb.HostSecurityScanList{Total: int32(page.Total), Offset: int32(page.Offset), Limit: int32(page.Limit)}
	for _, scan := range page.Items {
		list.Records = append(list.Records, hostSecuritySummaryFromScan(scan))
	}
	return &pb.MonitorCommandResult{Status: "success", HostSecurityScanList: list}
}

// remoteHostSecurityDetail returns one stored host scan by id, with
// filtered, paged findings.
func remoteHostSecurityDetail(req *pb.MonitorCommandRequest, dataDir string) *pb.MonitorCommandResult {
	id, err := server.EnsureAgentID()
	if err != nil {
		return securityCommandError(req, err)
	}
	store, err := hostSecurityOpenStore(dataDir)
	if err != nil {
		return securityCommandError(req, err)
	}
	defer store.Close()
	ctx, cancel := hostSecurityQueryContext()
	defer cancel()
	opts := req.GetHostSecurity()
	scan, err := store.Scan(ctx, id, opts.GetScanId())
	if err = hostSecurityStoreError(err); err != nil {
		return securityCommandError(req, err)
	}
	limit, _ := hostSecurityPageBounds(opts)
	page := int(opts.GetFindingsOffset())
	filtered, total := filterServerFindings(scan.Findings, opts.GetSeverity(), opts.GetState(), opts.GetPackage(), limit, page)
	return &pb.MonitorCommandResult{
		Status: "success",
		HostSecurityScanRecord: hostSecurityRecordFromScan(scan, filtered, total,
			page+len(filtered) < total),
	}
}

// remoteHostSecurityInventory returns one stored scan snapshot's installed
// packages with the package filter and paging.
func remoteHostSecurityInventory(req *pb.MonitorCommandRequest, dataDir string) *pb.MonitorCommandResult {
	id, err := server.EnsureAgentID()
	if err != nil {
		return securityCommandError(req, err)
	}
	store, err := hostSecurityOpenStore(dataDir)
	if err != nil {
		return securityCommandError(req, err)
	}
	defer store.Close()
	ctx, cancel := hostSecurityQueryContext()
	defer cancel()
	opts := req.GetHostSecurity()
	scan, err := store.Scan(ctx, id, opts.GetScanId())
	if err = hostSecurityStoreError(err); err != nil {
		return securityCommandError(req, err)
	}
	limit, offset := hostSecurityPageBounds(opts)
	page, err := store.Inventory(ctx, id, scan.ID, opts.GetPackage(), limit, offset)
	if err = hostSecurityStoreError(err); err != nil {
		return securityCommandError(req, err)
	}
	list := &pb.HostSecurityPackageList{
		Total:  int32(page.Total),
		Offset: int32(page.Offset),
		Limit:  int32(page.Limit),
		ScanId: scan.ID,
	}
	for _, p := range page.Items {
		list.Packages = append(list.Packages, hostSecurityPackageToProto(p))
	}
	return &pb.MonitorCommandResult{Status: "success", HostSecurityInventory: list}
}

// remoteHostSecurityConfig returns the local consent configuration. It is
// the one host security command answerable without consent.
func remoteHostSecurityConfig(req *pb.MonitorCommandRequest, dataDir string) *pb.MonitorCommandResult {
	reporting := hostsecurity.HostReportingEnabled(dataDir)
	return &pb.MonitorCommandResult{
		Status: "success",
		HostSecurityConfig: &pb.HostSecurityConfigState{
			ReportingEnabled: reporting,
			ScanInterval:     hostsecurity.HostScanInterval(dataDir).String(),
			SchedulerActive:  reporting,
		},
	}
}

// remoteHostSecurityRebootStatus returns the reboot state recorded with the
// latest completed host scan. Fields the scanner could not establish are
// left unset: absence means unknown, never "no reboot needed".
func remoteHostSecurityRebootStatus(req *pb.MonitorCommandRequest, dataDir string) *pb.MonitorCommandResult {
	id, err := server.EnsureAgentID()
	if err != nil {
		return securityCommandError(req, err)
	}
	store, err := hostSecurityOpenStore(dataDir)
	if err != nil {
		return securityCommandError(req, err)
	}
	defer store.Close()
	ctx, cancel := hostSecurityQueryContext()
	defer cancel()
	scan, err := store.LatestFinished(ctx, id)
	if err = hostSecurityStoreError(err); err != nil {
		return securityCommandError(req, err)
	}
	return &pb.MonitorCommandResult{Status: "success", HostSecurityReboot: hostSecurityRebootFromScan(scan)}
}

// remoteHostSecurityScanRun runs one host security scan under the host scan
// lock. A concurrent scan (local, scheduled or remote) returns
// HOST_SECURITY_SCAN_RUNNING instead of duplicating work. The result is the
// completed (or failed) scan record with findings capped for the wire.
func remoteHostSecurityScanRun(req *pb.MonitorCommandRequest, dataDir string) *pb.MonitorCommandResult {
	lock, err := hostsecurity.TryAcquireHostScanLock(dataDir)
	if errors.Is(err, hostsecurity.ErrHostScanLockHeld) {
		return securityCommandError(req, phelixerr.New(phelixerr.CodeHostSecurityScanRunning,
			"a host security scan is already running; retry after it completes"))
	}
	if err != nil {
		return securityCommandError(req, err)
	}
	defer lock.Release()

	id, err := server.EnsureAgentID()
	if err != nil {
		return securityCommandError(req, err)
	}
	store, err := hostSecurityOpenStore(dataDir)
	if err != nil {
		return securityCommandError(req, err)
	}
	defer store.Close()
	ctx, cancel := hostSecurityQueryContext()
	defer cancel()
	scan, err := hostsecurity.ScanHost(ctx, newServerHostSystem(), store, id, nil, serverIntelligence(dataDir))
	if err != nil {
		return securityCommandError(req, err)
	}
	return &pb.MonitorCommandResult{Status: "success", HostSecurityScanRecord: hostSecurityCappedRecord(scan)}
}

// remoteHostSecurityUpdatePreview, remoteHostSecurityUpdateApply and
// remoteHostSecurityUpdateVerify execute the package update engine; they
// are only answerable when the engine is available on this host.
func remoteHostSecurityUpdatePreview(req *pb.MonitorCommandRequest, dataDir string) *pb.MonitorCommandResult {
	return securityCommandError(req, phelixerr.New(phelixerr.CodeHostSecurityUnsupported,
		"package update operations are unavailable on this host"))
}

func remoteHostSecurityUpdateApply(req *pb.MonitorCommandRequest, dataDir string) *pb.MonitorCommandResult {
	return securityCommandError(req, phelixerr.New(phelixerr.CodeHostSecurityUnsupported,
		"package update operations are unavailable on this host"))
}

func remoteHostSecurityUpdateVerify(req *pb.MonitorCommandRequest, dataDir string) *pb.MonitorCommandResult {
	return securityCommandError(req, phelixerr.New(phelixerr.CodeHostSecurityUnsupported,
		"package update operations are unavailable on this host"))
}

// hostSecurityStoreError maps a store miss to the host security not-found
// code and passes every other error through.
func hostSecurityStoreError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return phelixerr.New(phelixerr.CodeHostSecurityNotFound, "no completed host scan found; run phelix security server scan first")
	}
	return err
}

// hostSecurityRecordFromScan converts one scan and its pre-filtered,
// pre-paged findings into the wire record.
func hostSecurityRecordFromScan(scan hostsecurity.Scan, findings []hostsecurity.Finding, findingsTotal int, truncated bool) *pb.HostSecurityScanRecord {
	record := &pb.HostSecurityScanRecord{
		ScanId:            scan.ID,
		ServerId:          scan.ServerID,
		Status:            scan.Status,
		Coverage:          scan.Coverage,
		MethodVersion:     scan.MethodVersion,
		ScannerVersion:    scan.ScannerVersion,
		SnapshotId:        scan.SnapshotID,
		PackageCount:      int32(scan.PackageCount),
		UnknownPackages:   int32(scan.UnknownPackages),
		FindingsTotal:     int32(findingsTotal),
		FindingsTruncated: truncated,
		Warnings:          scan.Warnings,
		StartedAtMs:       timeMillis(scan.StartedAt),
		FinishedAtMs:      timeMillis(scan.FinishedAt),
		DurationMs:        scan.DurationMS,
		RebootRequired:    scan.RebootRequired,
		RebootReasons:     scan.RebootReasons,
		Counts:            hostSecurityCountsToProto(scan.Counts),
		Score:             hostSecurityScoreToProto(scan.Score),
	}
	if record.Counts == nil {
		record.Counts = map[string]int32{}
	}
	for _, finding := range findings {
		record.Findings = append(record.Findings, hostSecurityFindingToProto(finding))
	}
	for _, source := range scan.Sources {
		record.Sources = append(record.Sources, hostSecuritySourceToProto(source))
	}
	return record
}

// hostSecurityCappedRecord converts a scan result for the wire with the
// findings capped at the wire limit; the full record stays in the store.
func hostSecurityCappedRecord(scan hostsecurity.Scan) *pb.HostSecurityScanRecord {
	findings := scan.Findings
	truncated := false
	if len(findings) > hostSecurityFindingsWireCap {
		findings = findings[:hostSecurityFindingsWireCap]
		truncated = true
	}
	return hostSecurityRecordFromScan(scan, findings, len(scan.Findings), truncated)
}

func hostSecuritySummaryFromScan(scan hostsecurity.Scan) *pb.HostSecurityScanSummary {
	return &pb.HostSecurityScanSummary{
		ScanId:       scan.ID,
		Status:       scan.Status,
		Coverage:     scan.Coverage,
		PackageCount: int32(scan.PackageCount),
		FindingCount: int32(len(scan.Findings)),
		Counts:       hostSecurityCountsToProto(scan.Counts),
		Score:        hostSecurityScoreToProto(scan.Score),
		StartedAtMs:  timeMillis(scan.StartedAt),
		FinishedAtMs: timeMillis(scan.FinishedAt),
		DurationMs:   scan.DurationMS,
	}
}

func hostSecuritySnapshotFromScan(scan hostsecurity.Scan) *pb.HostSecuritySnapshotSummary {
	return &pb.HostSecuritySnapshotSummary{
		ScanId:          scan.ID,
		Status:          scan.Status,
		Coverage:        scan.Coverage,
		PackageCount:    int32(scan.PackageCount),
		UnknownPackages: int32(scan.UnknownPackages),
		Counts:          hostSecurityCountsToProto(scan.Counts),
		Score:           hostSecurityScoreToProto(scan.Score),
		StartedAtMs:     timeMillis(scan.StartedAt),
		FinishedAtMs:    timeMillis(scan.FinishedAt),
		RebootRequired:  scan.RebootRequired,
		Warnings:        scan.Warnings,
	}
}

// hostSecurityCountsToProto converts the severity count map to the wire
// type; a nil map stays a nil wire map.
func hostSecurityCountsToProto(counts map[string]int) map[string]int32 {
	if counts == nil {
		return nil
	}
	out := make(map[string]int32, len(counts))
	for severity, count := range counts {
		out[severity] = int32(count)
	}
	return out
}

func hostSecurityRebootFromScan(scan hostsecurity.Scan) *pb.HostSecurityRebootState {
	reasons := scan.RebootReasons
	if reasons == nil {
		reasons = []string{}
	}
	return &pb.HostSecurityRebootState{
		Required: scan.RebootRequired,
		Reasons:  reasons,
	}
}

func hostSecurityScoreToProto(score securityscore.Score) *pb.HostSecurityScore {
	out := &pb.HostSecurityScore{
		Version: score.Version,
		Status:  score.Status,
		AtMs:    timeMillis(score.At),
		Reasons: score.Reasons,
	}
	if score.Value != nil {
		out.HasValue = true
		out.Value = int32(*score.Value)
	}
	for _, contribution := range score.Breakdown {
		out.Breakdown = append(out.Breakdown, &pb.HostSecurityContribution{
			Id:       contribution.ID,
			Points:   float64(contribution.Points),
			Priority: contribution.Priority,
			Reasons:  contribution.Reasons,
		})
	}
	return out
}

func hostSecurityFindingToProto(finding hostsecurity.Finding) *pb.HostSecurityFinding {
	out := &pb.HostSecurityFinding{
		Id:            finding.ID,
		Package:       finding.Package,
		Version:       finding.Version,
		Cve:           finding.CVE,
		AdvisoryId:    finding.AdvisoryID,
		FixedVersion:  finding.FixedVersion,
		AffectedRange: finding.AffectedRange,
		State:         finding.State,
		Severity:      finding.Severity,
		Reason:        finding.Reason,
		References:    finding.References,
		PublishedAtMs: rfc3339Millis(finding.Published),
		ModifiedAtMs:  rfc3339Millis(finding.Modified),
	}
	for _, source := range finding.Sources {
		out.Sources = append(out.Sources, source.Name)
	}
	risk := &pb.HostSecurityRisk{
		Confidence:      finding.Risk.Confidence,
		Priority:        finding.Priority.Priority,
		PriorityPoints:  float64(finding.Priority.Points),
		PriorityReasons: finding.Priority.Reasons,
	}
	if finding.Risk.CVSS != nil {
		risk.Cvss = *finding.Risk.CVSS
		risk.HasCvss = true
	}
	if finding.Risk.EPSS != nil {
		risk.Epss = *finding.Risk.EPSS
		risk.HasEpss = true
	}
	if finding.Risk.KnownExploited != nil {
		risk.KnownExploited = *finding.Risk.KnownExploited
	}
	if finding.Risk.ExploitAvailable != nil {
		risk.ExploitAvailable = *finding.Risk.ExploitAvailable
	}
	out.Risk = risk
	return out
}

func hostSecuritySourceToProto(source hostsecurity.Source) *pb.HostSecuritySource {
	return &pb.HostSecuritySource{
		Name:          source.Name,
		Url:           source.URL,
		RetrievedAtMs: timeMillis(source.RetrievedAt),
		ModifiedAtMs:  rfc3339Millis(source.Modified),
		Stale:         source.Stale,
		Error:         source.Error,
		Digest:        source.Digest,
	}
}

func hostSecurityPackageToProto(p hostsecurity.Package) *pb.HostSecurityPackageEntry {
	return &pb.HostSecurityPackageEntry{
		Name:    p.Name,
		Version: p.Version,
		Release: p.Release,
		Arch:    p.Architecture,
		Manager: p.Manager,
		Origin:  p.Origin,
	}
}

func timeMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// rfc3339Millis parses an advisory timestamp into unix milliseconds;
// unparseable values stay zero instead of fabricating a time.
func rfc3339Millis(value string) int64 {
	if value == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}
