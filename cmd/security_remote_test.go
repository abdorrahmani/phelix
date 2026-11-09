package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/security"
)

const securityRemoteAppID = "11111111-1111-4111-8111-111111111111"

// securityTestDataDir is the isolated data directory the fixture
// installs; the app registry and the security store share it.
func securityTestDataDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".phelix")
}

// cmdGoResolver is a scripted GoResolver for handler tests.
type cmdGoResolver struct{}

func (cmdGoResolver) ResolveModules(ctx context.Context, dir string) ([]security.ResolvedModule, error) {
	return []security.ResolvedModule{{Path: "example.com/vuln", Version: "v1.0.0", Indirect: false}}, nil
}

func (cmdGoResolver) ModuleGraph(ctx context.Context, dir string) ([]security.ModuleGraphEdge, error) {
	return []security.ModuleGraphEdge{
		{ParentPath: "example.com/app", ChildPath: "example.com/vuln", ChildVersion: "v1.0.0"},
	}, nil
}

// securityRemoteFixture registers one application, points it at a project
// with reporting consent per the flag, and returns the project directory.
// HOME is isolated so the store and app registry are private to the test.
func securityRemoteFixture(t *testing.T, reporting bool) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".phelix"), 0o755); err != nil {
		t.Fatal(err)
	}

	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "go.mod"),
		[]byte("module example.com/app\n\ngo 1.24\n\nrequire example.com/vuln v1.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	phelix := "name: remoteapp\n"
	if reporting {
		phelix += "\nsecurity:\n  vulnerability:\n    enabled: true\n    reporting: true\n"
	}
	if err := os.WriteFile(filepath.Join(projectDir, "phelix.yaml"), []byte(phelix), 0o644); err != nil {
		t.Fatal(err)
	}

	registry := `{"` + securityRemoteAppID + `":{"id":"` + securityRemoteAppID + `","name":"remoteapp","status":"stopped","port":8080,"directory":"` + projectDir + `"}}`
	if err := os.WriteFile(filepath.Join(home, ".phelix", "apps.json"), []byte(registry), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := app.SetStateDirForTesting(filepath.Join(home, ".phelix")); err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	t.Cleanup(func() {
		_ = app.Manager.LoadState()
	})
	return projectDir
}

// securityRemoteScan seeds one completed scan for the fixture app through
// the real scan pipeline against a fake OSV endpoint.
func securityRemoteScan(t *testing.T, projectDir, osvURL string) *security.ScanRecord {
	t.Helper()
	opts := security.ScanOptions{
		AppID:       securityRemoteAppID,
		AppName:     "remoteapp",
		IdentityKey: securityRemoteAppID,
		Dir:         projectDir,
		Trigger:     security.TriggerManual,
		DataDir:     securityTestDataDir(),
		OSVBaseURL:  osvURL,
		GoResolver:  cmdGoResolver{},
	}
	result, err := security.RunScan(context.Background(), opts)
	if err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	return result.Record
}

func securityRequest(id, command, app string, opts *pb.SecurityCommandOptions) *pb.MonitorCommandRequest {
	return &pb.MonitorCommandRequest{
		RequestId: id,
		Type:      command,
		AppName:   app,
		Security:  opts,
	}
}

func resultErrorCode(result *pb.MonitorCommandResult) string {
	if result == nil {
		return ""
	}
	return result.GetErrorCode()
}

func TestRemoteSecurity_ConsentGate(t *testing.T) {
	projectDir := securityRemoteFixture(t, false)
	server := securityOSV(t, "high")
	defer server.Close()
	securityRemoteScan(t, projectDir, server.URL)

	result := RemoteSecurityCommand(securityRequest("r1", phelixgrpc.SecurityCommandReport, "remoteapp", nil))
	if result.GetStatus() != "error" {
		t.Fatalf("reporting consent must gate every security command: %+v", result)
	}
	if result.GetErrorCode() != "SECURITY_REPORTING_DISABLED" {
		t.Fatalf("the consent error code must be explicit: %s", result.GetErrorCode())
	}
}

func TestRemoteSecurityFindingPagesAndHistorySummaries(t *testing.T) {
	dir := securityRemoteFixture(t, true)
	osv := securityOSV(t, "high")
	defer osv.Close()
	record := securityRemoteScan(t, dir, osv.URL)
	first := record.Findings[0]
	record.Findings = make([]security.Finding, 205)
	for i := range record.Findings {
		record.Findings[i] = first
	}
	record.Counts.Affected = 205
	record.Counts.PotentiallyAffected = 0
	store := security.NewScanStore(securityTestDataDir())
	if err := store.SaveScan(record); err != nil {
		t.Fatal(err)
	}
	req := securityRequest("page", phelixgrpc.SecurityCommandDetail, securityRemoteAppID, &pb.SecurityCommandOptions{ScanId: record.ScanID, FindingsOffset: 200, IncludeGraph: true})
	page := RemoteSecurityCommand(req).GetSecurityScanRecord()
	if len(page.GetFindings()) != 5 || page.GetFindingsTotal() != 205 || page.GetFindingsTruncated() || page.GetGraph().GetEcosystem() != "go" {
		t.Fatalf("page = %v", page)
	}
	history := RemoteSecurityCommand(securityRequest("history", phelixgrpc.SecurityCommandHistory, securityRemoteAppID, nil)).GetSecurityScanList()
	if len(history.GetScans()) != 1 || len(history.Scans[0].Findings) != 0 || !history.Scans[0].FindingsTruncated {
		t.Fatalf("history must contain summaries: %v", history)
	}
}

func TestRemoteSecurity_CurrentConfiguration(t *testing.T) {
	dir := securityRemoteFixture(t, true)
	osv := securityOSV(t, "low")
	defer osv.Close()
	securityRemoteScan(t, dir, osv.URL)
	for _, tc := range []struct{ content, code, reporting string }{
		{"name: remoteapp\nsecurity:\n  vulnerability:\n    reporting: true\n", "", "enabled"},
		{"name: remoteapp\nsecurity:\n  vulnerability:\n    reporting: false\n", "SECURITY_REPORTING_DISABLED", "disabled"},
		{"security: [", "SECURITY_CONFIG_INVALID", ""},
		{"name: another\nsecurity:\n  vulnerability:\n    reporting: true\n", "", "enabled"},
	} {
		if err := os.WriteFile(filepath.Join(dir, "phelix.yaml"), []byte(tc.content), 0600); err != nil {
			t.Fatal(err)
		}
		result := RemoteSecurityCommand(securityRequest("r", phelixgrpc.SecurityCommandReport, securityRemoteAppID, nil))
		if result.GetErrorCode() != tc.code {
			t.Fatalf("report: %v, want %s", result, tc.code)
		}
		cfg := RemoteSecurityCommand(securityRequest("c", phelixgrpc.SecurityCommandConfig, securityRemoteAppID, nil))
		if tc.reporting != "" && cfg.GetSecurityConfig().GetReporting() != tc.reporting {
			t.Fatalf("config: %v", cfg)
		}
		if tc.reporting == "disabled" && (cfg.GetSecurityConfig().GetLastScanId() != "" || cfg.GetSecurityConfig().GetScanInterval() != "") {
			t.Fatal("disabled status disclosed scan settings")
		}
		snapshots := securitySnapshotsForConsentingApps()
		if (len(snapshots) > 0) != (tc.reporting == "enabled") {
			t.Fatalf("snapshots ignored current config: %v", snapshots)
		}
	}
	if err := os.Remove(filepath.Join(dir, "phelix.yaml")); err != nil {
		t.Fatal(err)
	}
	result := RemoteSecurityCommand(securityRequest("m", phelixgrpc.SecurityCommandReport, securityRemoteAppID, nil))
	if result.GetErrorCode() != "SECURITY_CONFIG_MISSING" {
		t.Fatalf("missing config: %v", result)
	}
}

func TestRemoteSecurity_UnknownApp(t *testing.T) {
	securityRemoteFixture(t, true)
	result := RemoteSecurityCommand(securityRequest("r1", phelixgrpc.SecurityCommandReport, "nosuchapp", nil))
	if result.GetErrorCode() != "NOT_FOUND" {
		t.Fatalf("an unknown application is NOT_FOUND: %s", result.GetErrorCode())
	}
	missing := RemoteSecurityCommand(securityRequest("r2", phelixgrpc.SecurityCommandReport, "remoteapp", nil))
	if missing.GetErrorCode() != "SCAN_NOT_FOUND" {
		t.Fatalf("a registered app without scans is SCAN_NOT_FOUND: %s", missing.GetErrorCode())
	}
}

func TestRemoteSecurity_ReportAndHistory(t *testing.T) {
	projectDir := securityRemoteFixture(t, true)
	server := securityOSV(t, "high")
	defer server.Close()
	securityRemoteScan(t, projectDir, server.URL)
	securityRemoteScan(t, projectDir, server.URL)

	report := RemoteSecurityCommand(securityRequest("r1", phelixgrpc.SecurityCommandReport, "remoteapp", nil))
	if report.GetStatus() != "success" {
		t.Fatalf("report failed: %s %s", report.GetStatus(), report.GetError())
	}
	record := report.GetSecurityScanRecord()
	if record == nil || record.GetScanId() == "" {
		t.Fatalf("the report carries the latest record: %+v", record)
	}
	if record.GetCounts().GetAffected() != 1 {
		t.Fatalf("the resolved vulnerable dependency is affected: %+v", record.GetCounts())
	}
	if len(record.GetFindings()) != 1 || record.GetFindings()[0].GetState() != "affected" {
		t.Fatalf("a resolved version inside the range is affected: %+v", record.GetFindings())
	}

	filtered := RemoteSecurityCommand(securityRequest("r2", phelixgrpc.SecurityCommandReport, "remoteapp",
		&pb.SecurityCommandOptions{Severity: "critical"}))
	if got := len(filtered.GetSecurityScanRecord().GetFindings()); got != 0 {
		t.Fatalf("severity filtering must apply to findings: %d", got)
	}

	history := RemoteSecurityCommand(securityRequest("r3", phelixgrpc.SecurityCommandHistory, "remoteapp",
		&pb.SecurityCommandOptions{Limit: 1, Offset: 0}))
	list := history.GetSecurityScanList()
	if len(list.GetScans()) != 1 || !list.GetHasMore() || list.GetTotal() != 2 {
		t.Fatalf("history paging wrong: %+v", list)
	}
	secondPage := RemoteSecurityCommand(securityRequest("r4", phelixgrpc.SecurityCommandHistory, "remoteapp",
		&pb.SecurityCommandOptions{Limit: 1, Offset: 1}))
	if len(secondPage.GetSecurityScanList().GetScans()) != 1 || secondPage.GetSecurityScanList().GetHasMore() {
		t.Fatalf("the second page must carry the older scan only: %+v", secondPage.GetSecurityScanList())
	}
}

func TestRemoteSecurity_DetailAndGraph(t *testing.T) {
	projectDir := securityRemoteFixture(t, true)
	server := securityOSV(t, "high")
	defer server.Close()
	record := securityRemoteScan(t, projectDir, server.URL)

	detail := RemoteSecurityCommand(securityRequest("r1", phelixgrpc.SecurityCommandDetail, "remoteapp",
		&pb.SecurityCommandOptions{ScanId: record.ScanID, IncludeGraph: true}))
	if detail.GetStatus() != "success" {
		t.Fatalf("detail failed: %s %s", detail.GetStatus(), detail.GetError())
	}
	graph := detail.GetSecurityScanRecord().GetGraph()
	if graph == nil || len(graph.GetRoots()) == 0 || len(graph.GetEdges()) == 0 {
		t.Fatalf("the graph rides when requested: %+v", graph)
	}

	withoutGraph := RemoteSecurityCommand(securityRequest("r2", phelixgrpc.SecurityCommandDetail, "remoteapp",
		&pb.SecurityCommandOptions{ScanId: record.ScanID}))
	if withoutGraph.GetSecurityScanRecord().GetGraph() != nil {
		t.Fatal("the graph stays off the wire unless requested")
	}

	// A scan id from another identity must not leak.
	store := security.NewScanStore(securityTestDataDir())
	foreign := &security.ScanRecord{
		ScanID:      security.NewScanID(),
		AppName:     "otherapp",
		IdentityKey: "22222222-2222-4222-8222-222222222222",
		Status:      security.ScanCompleted,
		StartedAt:   record.StartedAt,
	}
	if err := store.SaveScan(foreign); err != nil {
		t.Fatal(err)
	}
	other := &pb.MonitorCommandRequest{RequestId: "r3", Type: phelixgrpc.SecurityCommandDetail, AppName: "remoteapp",
		Security: &pb.SecurityCommandOptions{ScanId: foreign.ScanID}}
	if got := resultErrorCode(RemoteSecurityCommand(other)); got != "SCAN_NOT_FOUND" {
		t.Fatalf("a foreign scan id is SCAN_NOT_FOUND, not an existence leak: %s", got)
	}
	missing := &pb.MonitorCommandRequest{RequestId: "r4", Type: phelixgrpc.SecurityCommandDetail, AppName: "remoteapp",
		Security: &pb.SecurityCommandOptions{ScanId: "scn_ffffffffffffffff"}}
	if got := resultErrorCode(RemoteSecurityCommand(missing)); got != "SCAN_NOT_FOUND" {
		t.Fatalf("a nonexistent scan is SCAN_NOT_FOUND: %s", got)
	}
}

func TestRemoteSecurity_ConfigAndConfigSet(t *testing.T) {
	projectDir := securityRemoteFixture(t, true)
	server := securityOSV(t, "low")
	defer server.Close()
	securityRemoteScan(t, projectDir, server.URL)

	config := RemoteSecurityCommand(securityRequest("r1", phelixgrpc.SecurityCommandConfig, "remoteapp", nil))
	state := config.GetSecurityConfig()
	if state == nil || !state.GetEnabled() || state.GetFailThreshold() != "high" || state.GetReporting() != "enabled" {
		t.Fatalf("config state wrong: %+v", state)
	}
	if state.GetLastScanId() == "" {
		t.Fatalf("the config view includes scheduler status: %+v", state)
	}

	updated := RemoteSecurityCommand(securityRequest("r2", phelixgrpc.SecurityCommandConfigSet, "remoteapp",
		&pb.SecurityCommandOptions{SetEnabled: true, Enabled: false, FailThreshold: "critical"}))
	newState := updated.GetSecurityConfig()
	if newState == nil || newState.GetEnabled() || newState.GetFailThreshold() != "critical" {
		t.Fatalf("the update did not apply: %+v", newState)
	}
	raw, err := os.ReadFile(filepath.Join(projectDir, "phelix.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "fail_threshold: critical") || !strings.Contains(string(raw), "reporting: true") {
		t.Fatalf("unrelated settings must survive the remote edit:\n%s", raw)
	}
}

func TestRemoteSecurity_ScanRunAndDedup(t *testing.T) {
	securityRemoteFixture(t, true)
	server := securityOSV(t, "high")
	defer server.Close()
	t.Setenv("PHELIX_OSV_URL", server.URL)

	run := RemoteSecurityCommand(securityRequest("r1", phelixgrpc.SecurityCommandRun, "remoteapp", nil))
	if run.GetStatus() != "success" || run.GetSecurityScanRecord() == nil {
		t.Fatalf("remote scan run failed: %s %s", run.GetStatus(), run.GetError())
	}
	if run.GetSecurityScanRecord().GetTrigger() != "remote" {
		t.Fatalf("a remote run records its trigger: %+v", run.GetSecurityScanRecord())
	}

	store := security.NewScanStore(securityTestDataDir())
	lock, err := store.TryAcquireScanLock(securityRemoteAppID)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	busy := RemoteSecurityCommand(securityRequest("r2", phelixgrpc.SecurityCommandRun, "remoteapp", nil))
	if resultErrorCode(busy) != "SECURITY_SCAN_RUNNING" {
		t.Fatalf("a scan while the lock is held answers SECURITY_SCAN_RUNNING: %+v", busy)
	}
}

func TestRemoteSecurity_AdvisoryFromCache(t *testing.T) {
	projectDir := securityRemoteFixture(t, true)
	server := securityOSV(t, "high")
	defer server.Close()
	securityRemoteScan(t, projectDir, server.URL)

	result := RemoteSecurityCommand(securityRequest("r1", phelixgrpc.SecurityCommandAdvisory, "remoteapp",
		&pb.SecurityCommandOptions{AdvisoryId: "GO-2024-3001"}))
	adv := result.GetSecurityAdvisory()
	if adv == nil {
		t.Fatalf("the advisory comes from the scan cache: error=%s code=%s", result.GetError(), result.GetErrorCode())
	}
	if adv.GetId() != "GO-2024-3001" || !adv.GetFromCache() {
		t.Fatalf("the advisory comes from the scan cache: %+v", adv)
	}
	if adv.GetSeverity() != "high" {
		t.Fatalf("the severity label flows through: %+v", adv)
	}
}
