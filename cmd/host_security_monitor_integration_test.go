package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/config"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/hostsecurity"
	"github.com/abdorrahmani/phelix/internal/server"
	"google.golang.org/grpc"
)

// hostSecurityFixtureSystem builds a Debian 12 (bookworm) fixture root with
// one installed package whose source is covered by the advisory server.
func hostSecurityFixtureSystem(t *testing.T) hostsecurity.System {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/os-release"),
		[]byte("ID=debian\nVERSION_ID=12\nVERSION_CODENAME=bookworm\nPRETTY_NAME=\"Debian GNU/Linux 12\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dpkg := "ii \tlibssl3\t3.0.11-1~deb12u2\tamd64\topenssl\t3.0.11-1~deb12u2\tDebian\t\tlibc6 (>= 2.34)\n"
	return hostsecurity.System{
		Root: root,
		Lookup: func(name string) (string, error) {
			return name, nil
		},
		Runner: fixtureRunnerFunc(func(ctx context.Context, name string, args ...string) ([]byte, error) {
			switch {
			case name == "apt":
				return []byte("apt 2.6.1 (amd64)\n"), nil
			case name == "uname":
				return []byte("x86_64\n"), nil
			case name == "dpkg-query":
				return []byte(dpkg), nil
			}
			return nil, fmt.Errorf("unexpected command %s", name)
		}),
	}
}

// hostSecurityAdvisoryServer serves the Debian tracker shape plus the
// enrichment endpoints the intelligence layer consults, all on one local
// server so the scan never touches the real advisory networks.
func hostSecurityAdvisoryServer(t *testing.T) map[string]string {
	t.Helper()
	tracker := `{"openssl":{"CVE-2024-3001":{"description":"fixture advisory","releases":{"bookworm":{"status":"open","fixed_version":"3.0.12-1~deb12u2","urgency":"high**"}}}}}`
	responses := map[string]string{
		"/debian": tracker,
		"/osv":    `{"id":"CVE-2024-3001","published":"2024-01-01T00:00:00Z","modified":"2024-02-01T00:00:00Z"}`,
		"/github": `[]`,
		"/nvd":    `{"vulnerabilities":[]}`,
		"/kev":    `{"vulnerabilities":[]}`,
		"/epss":   `{"data":[{"cve":"CVE-2024-3001","epss":"0.12"}]}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body, ok := responses[r.URL.Path]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	return map[string]string{
		"debian":               server.URL + "/debian",
		"osv-CVE-2024-3001":    server.URL + "/osv",
		"github-CVE-2024-3001": server.URL + "/github",
		"nvd-CVE-2024-3001":    server.URL + "/nvd",
		"cisa-kev":             server.URL + "/kev",
		"epss":                 server.URL + "/epss",
	}
}

// hostSecurityIntelligence builds the advisory intelligence bound to the
// fixture advisory server.
func hostSecurityIntelligence(t *testing.T) *hostsecurity.Intelligence {
	t.Helper()
	return &hostsecurity.Intelligence{
		CacheDir: filepath.Join(securityTestDataDir(), "host-security", "intel-cache"),
		URLs:     hostSecurityAdvisoryServer(t),
	}
}

// seedHostScan stores one completed host scan directly, for handler tests
// that do not need the full scan pipeline.
func seedHostScan(t *testing.T) hostsecurity.Scan {
	t.Helper()
	store, err := hostsecurity.OpenStore(securityTestDataDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	id, err := server.EnsureAgentID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	scan := hostsecurity.Scan{
		ID: "scan-fixture-0001", ServerID: id, Domain: "server", Status: "completed", Coverage: "complete",
		MethodVersion: "host-match-v1", ScannerVersion: "test", SnapshotID: "snap-fixture",
		PackageCount: 1, UnknownPackages: 0, Counts: map[string]int{"high": 1},
		Findings: []hostsecurity.Finding{{
			ID: "abc123", Package: "libssl3", Version: "3.0.11-1~deb12u2", CVE: "CVE-2024-3001",
			AdvisoryID: "CVE-2024-3001", FixedVersion: "3.0.12-1~deb12u2", AffectedRange: "< 3.0.12-1~deb12u2",
			State: "confirmed", Severity: "high", Reason: "fixture",
		}},
		Sources:   []hostsecurity.Source{{Name: "debian", URL: "https://fixture.invalid/tracker", RetrievedAt: now}},
		StartedAt: now.Add(-time.Second), FinishedAt: now, DurationMS: 1000,
		Warnings: []string{},
	}
	packages := []hostsecurity.Package{{Key: "libssl3", Name: "libssl3", Version: "3.0.11-1~deb12u2", Architecture: "amd64", Manager: "apt", Status: "installed", InventoryAt: now}}
	if err := store.SaveScan(context.Background(), scan, packages); err != nil {
		t.Fatal(err)
	}
	return scan
}

func hostSecuritySetConsent(t *testing.T, reporting bool) {
	t.Helper()
	if err := hostsecurity.SaveHostConfig(securityTestDataDir(), hostsecurity.HostConfig{Reporting: reporting, ScanInterval: "24h"}); err != nil {
		t.Fatal(err)
	}
}

func hostSecurityRequest(id, command string, opts *pb.HostSecurityCommandOptions) *pb.MonitorCommandRequest {
	return &pb.MonitorCommandRequest{
		RequestId:    id,
		Type:         command,
		HostSecurity: opts,
	}
}

// TestRemoteHostSecurity_ConsentGate pins the local-only consent gate:
// every command except host_security_config is refused while reporting is
// disabled, and the config answer reports the disabled state.
func TestRemoteHostSecurity_ConsentGate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".phelix"), 0o755); err != nil {
		t.Fatal(err)
	}
	seedHostScan(t)
	hostSecuritySetConsent(t, false)

	if got := RemoteHostSecurityCommand(hostSecurityRequest("r1", phelixgrpc.HostSecurityCommandReport, nil)); got.GetErrorCode() != "HOST_SECURITY_CONSENT_DISABLED" || got.GetHostSecurityScanRecord() != nil {
		t.Fatalf("report must be refused without consent: %+v", got)
	}
	if got := RemoteHostSecurityCommand(hostSecurityRequest("r2", phelixgrpc.HostSecurityCommandScanRun, nil)); got.GetErrorCode() != "HOST_SECURITY_CONSENT_DISABLED" {
		t.Fatalf("scan_run must be refused without consent: %+v", got)
	}
	config := RemoteHostSecurityCommand(hostSecurityRequest("r3", phelixgrpc.HostSecurityCommandConfig, nil))
	if config.GetStatus() != "success" || config.GetHostSecurityConfig().GetReportingEnabled() {
		t.Fatalf("config must stay answerable without consent: %+v", config)
	}

	hostSecuritySetConsent(t, true)
	if got := RemoteHostSecurityCommand(hostSecurityRequest("r4", phelixgrpc.HostSecurityCommandReport, nil)); got.GetStatus() != "success" || len(got.GetHostSecurityScanRecord().GetFindings()) != 1 {
		t.Fatalf("report with consent: %+v", got)
	}
	if got := RemoteHostSecurityCommand(hostSecurityRequest("r5", phelixgrpc.HostSecurityCommandHistory, &pb.HostSecurityCommandOptions{Limit: 10})); got.GetHostSecurityScanList().GetTotal() != 1 {
		t.Fatalf("history with consent: %+v", got)
	}
	if got := RemoteHostSecurityCommand(hostSecurityRequest("r6", phelixgrpc.HostSecurityCommandInventory, &pb.HostSecurityCommandOptions{Limit: 10})); got.GetHostSecurityInventory().GetTotal() != 1 || got.GetHostSecurityInventory().GetScanId() != "scan-fixture-0001" {
		t.Fatalf("inventory with consent: %+v", got)
	}
	if got := RemoteHostSecurityCommand(hostSecurityRequest("r7", phelixgrpc.HostSecurityCommandDetail, &pb.HostSecurityCommandOptions{ScanId: "scan-fixture-0001"})); got.GetHostSecurityScanRecord().GetScanId() != "scan-fixture-0001" {
		t.Fatalf("detail with consent: %+v", got)
	}
	if got := RemoteHostSecurityCommand(hostSecurityRequest("r8", phelixgrpc.HostSecurityCommandRebootStatus, nil)); got.GetStatus() != "success" || got.GetHostSecurityReboot().GetRequired() {
		t.Fatalf("reboot status without recorded state must be false with no fabricated reasons: %+v", got)
	}
	if got := RemoteHostSecurityCommand(hostSecurityRequest("r9", phelixgrpc.HostSecurityCommandDetail, &pb.HostSecurityCommandOptions{ScanId: "scan-missing"})); got.GetErrorCode() != "HOST_SECURITY_NOT_FOUND" {
		t.Fatalf("missing scan: %+v", got)
	}
}

type fixtureRunnerFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

func (f fixtureRunnerFunc) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f(ctx, name, args...)
}

// TestHostSecurityMonitorEndToEnd drives the host security command family
// against a fake backend over a real MonitorStream: scan_run with a fixture
// system and a local advisory server, the store queries, foreign-field
// rejection, the consent gate, and ledger replay across a stream drop.
func TestHostSecurityMonitorEndToEnd(t *testing.T) {
	if os.Getenv("PHELIX_HOST_SECURITY_MONITOR_TEST") != "1" {
		child := exec.Command(os.Args[0], "-test.run=^TestHostSecurityMonitorEndToEnd$", "-test.timeout=90s")
		child.Env = append(os.Environ(), "PHELIX_HOST_SECURITY_MONITOR_TEST=1")
		if output, err := child.CombinedOutput(); err != nil {
			t.Fatalf("HostSecurity MonitorStream integration: %v\n%s", err, output)
		}
		return
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".phelix"), 0o755); err != nil {
		t.Fatal(err)
	}
	hostSecuritySetConsent(t, true)
	intelligence := hostSecurityIntelligence(t)
	prevSystem := newServerHostSystem
	newServerHostSystem = func() hostsecurity.System { return hostSecurityFixtureSystem(t) }
	t.Cleanup(func() { newServerHostSystem = prevSystem })
	prevIntelClient := newServerIntelClient
	newServerIntelClient = func() *http.Client { return nil }
	t.Cleanup(func() { newServerIntelClient = prevIntelClient })
	prevIntel := serverIntelligence
	serverIntelligence = func(dataDir string) *hostsecurity.Intelligence { return intelligence }
	t.Cleanup(func() { serverIntelligence = prevIntel })

	session, err := json.Marshal(map[string]any{"sessionID": "integration", "token": "integration", "scope": "agent", "expiresAt": time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(securityTestDataDir(), "agent-session.json"), session, 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	backend := &securityMonitorBackend{controls: make(chan *pb.MonitorControl, 16), events: make(chan *pb.MonitorEvent, 128), drop: make(chan struct{}, 1)}
	grpcServer := grpc.NewServer()
	pb.RegisterPhelixServiceServer(grpcServer, backend)
	go func() { _ = grpcServer.Serve(listener) }()
	defer grpcServer.Stop()
	if err := config.Load(); err != nil {
		t.Fatal(err)
	}
	previous := *config.Get()
	config.Get().App.Mode = "dev"
	config.Get().App.GRPCUrl = listener.Addr().String()
	defer func() { *config.Get() = previous }()
	if err := phelixgrpc.InitializeHostSecurityLedger(); err != nil {
		t.Fatal(err)
	}

	blocked, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	var runs int
	phelixgrpc.SetHostSecurityHandler(func(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
		result := RemoteHostSecurityCommand(req)
		if req.RequestId == "drop-scan" {
			runs++
			close(blocked)
			<-release
		}
		return result
	})
	defer phelixgrpc.SetHostSecurityHandler(nil)
	client := phelixgrpc.NewClient()
	if err := client.Connect(); err != nil {
		t.Fatal(err)
	}
	client.StartMonitorStream()
	defer func() {
		releaseOnce.Do(func() { close(release) })
		phelixgrpc.WaitPendingHostSecurityCommands(3 * time.Second)
		client.Close()
	}()

	await := func(match func(*pb.MonitorEvent) bool) *pb.MonitorEvent {
		t.Helper()
		deadline := time.NewTimer(15 * time.Second)
		defer deadline.Stop()
		for {
			select {
			case event := <-backend.events:
				if match(event) {
					return event
				}
			case <-deadline.C:
				t.Fatal("MonitorStream event timed out")
				return nil
			}
		}
	}
	await(func(ev *pb.MonitorEvent) bool { return ev.GetAgentStatus() != nil })
	request := func(id, command string, opts *pb.HostSecurityCommandOptions) *pb.MonitorCommandResult {
		t.Helper()
		backend.controls <- &pb.MonitorControl{Payload: &pb.MonitorControl_Command{Command: hostSecurityRequest(id, command, opts)}}
		result := await(func(ev *pb.MonitorEvent) bool { return ev.GetCommandResult().GetRequestId() == id }).GetCommandResult()
		if result.GetCommand() != command {
			t.Fatalf("correlation: %v", result)
		}
		return result
	}

	if got := request("empty-report", phelixgrpc.HostSecurityCommandReport, nil); got.GetErrorCode() != "HOST_SECURITY_NOT_FOUND" {
		t.Fatalf("report without any scan: %v", got)
	}

	run := request("run-1", phelixgrpc.HostSecurityCommandScanRun, nil)
	if run.GetStatus() != "success" || run.GetHostSecurityScanRecord().GetScanId() == "" {
		t.Fatalf("scan_run: %v", run)
	}
	record := run.GetHostSecurityScanRecord()
	if record.GetPackageCount() != 1 || record.GetStatus() != "completed" || record.GetCoverage() != "complete" || len(record.GetFindings()) != 1 {
		t.Fatalf("scan record: %v", record)
	}
	finding := record.GetFindings()[0]
	if finding.GetPackage() != "libssl3" || finding.GetCve() != "CVE-2024-3001" || finding.GetSeverity() != "high" || finding.GetState() != "confirmed" {
		t.Fatalf("finding: %v", finding)
	}
	if finding.GetRisk() == nil || !finding.GetRisk().GetHasEpss() {
		t.Fatalf("finding risk enrichment: %v", finding.GetRisk())
	}

	report := request("report-1", phelixgrpc.HostSecurityCommandReport, &pb.HostSecurityCommandOptions{Severity: "high"})
	if report.GetHostSecurityScanRecord().GetScanId() != record.GetScanId() || report.GetHostSecurityScanRecord().GetFindingsTotal() != 1 {
		t.Fatalf("report: %v", report)
	}
	if got := request("report-page", phelixgrpc.HostSecurityCommandReport, &pb.HostSecurityCommandOptions{Severity: "low"}); got.GetHostSecurityScanRecord().GetFindingsTotal() != 0 {
		t.Fatalf("filtered report: %v", got)
	}
	if got := request("detail-1", phelixgrpc.HostSecurityCommandDetail, &pb.HostSecurityCommandOptions{ScanId: record.GetScanId()}); got.GetHostSecurityScanRecord().GetScanId() != record.GetScanId() {
		t.Fatalf("detail: %v", got)
	}
	if got := request("history-1", phelixgrpc.HostSecurityCommandHistory, &pb.HostSecurityCommandOptions{Limit: 10}); got.GetHostSecurityScanList().GetTotal() != 1 || len(got.GetHostSecurityScanList().GetRecords()) != 1 {
		t.Fatalf("history: %v", got)
	}
	inventory := request("inventory-1", phelixgrpc.HostSecurityCommandInventory, &pb.HostSecurityCommandOptions{Limit: 10})
	if inventory.GetHostSecurityInventory().GetTotal() != 1 || inventory.GetHostSecurityInventory().GetScanId() != record.GetScanId() {
		t.Fatalf("inventory: %v", inventory)
	}
	if got := request("config-1", phelixgrpc.HostSecurityCommandConfig, nil); !got.GetHostSecurityConfig().GetReportingEnabled() || !got.GetHostSecurityConfig().GetSchedulerActive() {
		t.Fatalf("config: %v", got)
	}
	if got := request("reboot-1", phelixgrpc.HostSecurityCommandRebootStatus, nil); got.GetStatus() != "success" || got.GetHostSecurityReboot().GetRequired() || len(got.GetHostSecurityReboot().GetReasons()) != 0 {
		t.Fatalf("reboot status: %v", got)
	}

	// Foreign-field rejection travels through the wire validation before
	// the handler runs.
	backend.controls <- &pb.MonitorControl{Payload: &pb.MonitorControl_Command{Command: &pb.MonitorCommandRequest{RequestId: "foreign-app", Type: phelixgrpc.HostSecurityCommandScanRun, AppName: "remoteapp"}}}
	if got := await(func(ev *pb.MonitorEvent) bool { return ev.GetCommandResult().GetRequestId() == "foreign-app" }).GetCommandResult(); got.GetErrorCode() != "INVALID_ARGUMENT" {
		t.Fatalf("foreign app_name rejection: %v", got)
	}
	backend.controls <- &pb.MonitorControl{Payload: &pb.MonitorControl_Command{Command: &pb.MonitorCommandRequest{RequestId: "foreign-security", Type: phelixgrpc.HostSecurityCommandScanRun, Security: &pb.SecurityCommandOptions{}}}}
	if got := await(func(ev *pb.MonitorEvent) bool { return ev.GetCommandResult().GetRequestId() == "foreign-security" }).GetCommandResult(); got.GetErrorCode() != "INVALID_ARGUMENT" {
		t.Fatalf("foreign family rejection: %v", got)
	}

	// Ledger replay across a stream drop: the mutation completes and is
	// durably recorded before the send, so the reconnect replays it.
	backend.controls <- &pb.MonitorControl{Payload: &pb.MonitorControl_Command{Command: hostSecurityRequest("drop-scan", phelixgrpc.HostSecurityCommandScanRun, nil)}}
	select {
	case <-blocked:
	case <-time.After(15 * time.Second):
		t.Fatal("scan did not reach completion")
	}
	backend.drop <- struct{}{}
	releaseOnce.Do(func() { close(release) })
	await(func(ev *pb.MonitorEvent) bool { return ev.GetAgentStatus() != nil })
	replayed := await(func(ev *pb.MonitorEvent) bool { return ev.GetCommandResult().GetRequestId() == "drop-scan" }).GetCommandResult()
	if replayed.GetStatus() != "success" || runs != 1 {
		t.Fatalf("replay after reconnect: %v runs=%d", replayed, runs)
	}
	backend.controls <- &pb.MonitorControl{Payload: &pb.MonitorControl_SecurityResultAck{SecurityResultAck: &pb.SecurityResultAck{RequestId: replayed.GetRequestId(), Command: replayed.GetCommand()}}}

	// The consent gate refuses everything but the config query once
	// consent is revoked, and consent stays local-only: there is no
	// remote toggle.
	hostSecuritySetConsent(t, false)
	if got := request("consent-report", phelixgrpc.HostSecurityCommandReport, nil); got.GetErrorCode() != "HOST_SECURITY_CONSENT_DISABLED" || got.GetHostSecurityScanRecord() != nil {
		t.Fatalf("consent refusal: %v", got)
	}
	if got := request("consent-run", phelixgrpc.HostSecurityCommandScanRun, nil); got.GetErrorCode() != "HOST_SECURITY_CONSENT_DISABLED" {
		t.Fatalf("consent refusal for scan_run: %v", got)
	}
	if got := request("consent-config", phelixgrpc.HostSecurityCommandConfig, nil); got.GetStatus() != "success" || got.GetHostSecurityConfig().GetReportingEnabled() {
		t.Fatalf("config without consent: %v", got)
	}
}
