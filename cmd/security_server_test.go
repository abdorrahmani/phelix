package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abdorrahmani/phelix/internal/hostsecurity"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/securityscore"
)

func TestServerSecurityCommands(t *testing.T) {
	for _, name := range []string{"scan", "status", "report", "inventory", "history"} {
		cmd := newServerSecurityCommand()
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetErr(&output)
		cmd.SetArgs([]string{name, "--help"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), "--json") {
			t.Fatalf("%s lacks JSON help", name)
		}
	}
	cmd := newServerSecurityCommand()
	cmd.SetArgs([]string{"inventory", "--limit", "501"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("accepted unbounded page")
	}
	cmd = newServerSecurityCommand()
	cmd.SetArgs([]string{"report", "--offset", "-1"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("accepted negative offset")
	}
}

// serverFixtureRunner answers package manager invocations with canned output
// so a scan runs against a fixture root without native utilities.
type serverFixtureRunner struct{ responses map[string][]byte }

func (r serverFixtureRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if out, ok := r.responses[name]; ok {
		return out, nil
	}
	return []byte("1.0\n"), nil
}

// serverFixtureSystem builds a Debian 12 fixture root with one installed
// package whose source is "ssl".
func serverFixtureSystem(t *testing.T) hostsecurity.System {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/os-release"), []byte("ID=debian\nVERSION_ID=12\nPRETTY_NAME=\"Fixture Linux\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return hostsecurity.System{
		Root:   root,
		Lookup: func(n string) (string, error) { return n, nil },
		Runner: serverFixtureRunner{responses: map[string][]byte{
			"apt":        []byte("apt 2.6 (fixture)\n"),
			"dpkg-query": []byte("ii \tssl\t1.0-1\tamd64\tssl\t1.0-1\tDebian\t\t\n"),
			"uname":      []byte("x86_64\n"),
		}},
	}
}

// serverRedirectTransport rewrites every advisory request to the local test
// server, standing in for the distribution and enrichment endpoints.
type serverRedirectTransport struct{ target *httptest.Server }

func (t serverRedirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rewritten := req.Clone(req.Context())
	rewritten.URL.Scheme = "http"
	rewritten.URL.Host = strings.TrimPrefix(t.target.URL, "http://")
	return http.DefaultTransport.RoundTrip(rewritten)
}

// newAdvisoryServer serves minimal tracker, OSV, GitHub, NVD, KEV, and EPSS
// responses for one CVE so a scan completes with provenance intact.
func newAdvisoryServer(t *testing.T, cve string) *httptest.Server {
	t.Helper()
	tracker := map[string]any{
		"ssl": map[string]any{
			cve: map[string]any{
				"description": "fixture advisory",
				"releases": map[string]any{
					"bookworm": map[string]any{"status": "open", "fixed_version": "", "urgency": "high**"},
				},
			},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/tracker/data/json", func(w http.ResponseWriter, r *http.Request) { writeJSONBody(t, w, tracker) })
	mux.HandleFunc("/v1/vulns/"+cve, func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(t, w, map[string]any{"id": cve, "published": "2024-01-01T00:00:00Z", "modified": "2024-01-02T00:00:00Z", "references": []any{map[string]any{"type": "WEB", "url": "https://example.com/advisory"}}})
	})
	mux.HandleFunc("/advisories", func(w http.ResponseWriter, r *http.Request) { writeJSONBody(t, w, []any{}) })
	mux.HandleFunc("/rest/json/cves/2.0", func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(t, w, map[string]any{"vulnerabilities": []any{map[string]any{"cve": map[string]any{"id": cve, "published": "2024-01-01T00:00:00Z", "lastModified": "2024-01-02T00:00:00Z", "metrics": map[string]any{"cvssMetricV31": []any{map[string]any{"cvssData": map[string]any{"baseScore": 7.5, "baseSeverity": "HIGH"}}}}}}}})
	})
	mux.HandleFunc("/sites/default/files/feeds/known_exploited_vulnerabilities.json", func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(t, w, map[string]any{"vulnerabilities": []any{}})
	})
	mux.HandleFunc("/data/v1/epss", func(w http.ResponseWriter, r *http.Request) { writeJSONBody(t, w, map[string]any{"data": []any{}}) })
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// writeJSONBody marshals the fixture body and writes it on the response.
func writeJSONBody(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Error(err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(raw); err != nil {
		t.Error(err)
	}
}

// installServerSeams points the server security seams at the fixture system
// and the local advisory server for the duration of the test.
func installServerSeams(t *testing.T, server *httptest.Server) {
	t.Helper()
	systemPrev, clientPrev := newServerHostSystem, newServerIntelClient
	newServerHostSystem = func() hostsecurity.System { return serverFixtureSystem(t) }
	newServerIntelClient = func() *http.Client { return &http.Client{Transport: serverRedirectTransport{server}} }
	t.Cleanup(func() { newServerHostSystem = systemPrev; newServerIntelClient = clientPrev })
}

// runServerSecurity executes a server security subcommand and returns the
// captured stdout (the JSON envelope in machine mode).
func runServerSecurity(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newServerSecurityCommand()
	cmd.SetArgs(args)
	defer machine.LeaveJSON()
	var out string
	var execErr error
	out = captureStdout(t, func() {
		execErr = cmd.Execute()
	})
	return out, execErr
}

// fixtureServerScan runs one fully wired scan against the fixture and
// returns the data directory holding its store and intelligence cache.
func fixtureServerScan(t *testing.T, cve string) string {
	t.Helper()
	server := newAdvisoryServer(t, cve)
	dataDir := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", dataDir)
	installServerSeams(t, server)
	out, err := runServerSecurity(t, "scan", "--json")
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	env := decodeEnvelope(t, out)
	if env.Status != machine.StatusSucceeded {
		t.Fatalf("scan envelope status %q", env.Status)
	}
	raw, err := json.Marshal(env.Result)
	if err != nil {
		t.Fatal(err)
	}
	var scan hostsecurity.Scan
	if err := json.Unmarshal(raw, &scan); err != nil {
		t.Fatalf("result is not a host scan: %v (%s)", err, raw)
	}
	if scan.Status != "completed" || scan.Coverage != "complete" {
		t.Fatalf("scan status %q coverage %q: %+v", scan.Status, scan.Coverage, scan)
	}
	if scan.Score.Value == nil || *scan.Score.Value != 89 || scan.Score.Status != "needs_attention" {
		t.Fatalf("unexpected score: %+v", scan.Score)
	}
	if scan.Counts["high"] != 1 || len(scan.Findings) != 1 {
		t.Fatalf("unexpected counts/findings: %+v", scan.Counts)
	}
	finding := scan.Findings[0]
	if finding.Severity != "high" || finding.State != "confirmed" || finding.CVE != cve || finding.Package != "ssl" {
		t.Fatalf("unexpected finding: %+v", finding)
	}
	if finding.Risk.CVSS == nil || *finding.Risk.CVSS != 7.5 || finding.Risk.KnownExploited == nil || *finding.Risk.KnownExploited {
		t.Fatalf("finding risk not enriched: %+v", finding.Risk)
	}
	if len(scan.Sources) == 0 {
		t.Fatal("scan carries no provenance sources")
	}
	for _, source := range scan.Sources {
		if source.Error != "" || source.Stale {
			t.Fatalf("source not clean: %+v", source)
		}
	}
	if len(scan.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", scan.Warnings)
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "host-security", "intel-cache"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("intelligence cache missing under the data directory: %v", err)
	}
	return dataDir
}

func TestServerSecurityScanWiring(t *testing.T) {
	fixtureServerScan(t, "CVE-2024-10001")
}

func TestServerSecurityStatusShowsScore(t *testing.T) {
	fixtureServerScan(t, "CVE-2024-10001")
	out, err := runServerSecurity(t, "status", "--json")
	if err != nil {
		t.Fatalf("status failed: %v", err)
	}
	env := decodeEnvelope(t, out)
	raw, _ := json.Marshal(env.Result)
	var scan hostsecurity.Scan
	if err := json.Unmarshal(raw, &scan); err != nil {
		t.Fatalf("status result is not a host scan: %v (%s)", err, raw)
	}
	if scan.Score.Value == nil || scan.Score.Status != "needs_attention" || scan.Counts["high"] != 1 || len(scan.Sources) == 0 {
		t.Fatalf("status scan missing score, counts, or sources: %+v", scan)
	}

	cmd := newServerSecurityCommand()
	var human bytes.Buffer
	cmd.SetOut(&human)
	cmd.SetErr(&human)
	cmd.SetArgs([]string{"status"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	humanOut := human.String()
	if !strings.Contains(humanOut, "89 (needs_attention)") || !strings.Contains(humanOut, "complete") || !strings.Contains(humanOut, "high 1") {
		t.Fatalf("status output lacks score, coverage, or counts: %q", humanOut)
	}
}

func TestServerSecurityHistoryRowsCarryScore(t *testing.T) {
	fixtureServerScan(t, "CVE-2024-10001")
	out, err := runServerSecurity(t, "history", "--json")
	if err != nil {
		t.Fatalf("history failed: %v", err)
	}
	env := decodeEnvelope(t, out)
	raw, _ := json.Marshal(env.Result)
	var page struct {
		Items []hostsecurity.Scan `json:"items"`
		Total int                 `json:"total"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("history result is not a page: %v (%s)", err, raw)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Score.Value == nil || page.Items[0].Score.Status != "needs_attention" || page.Items[0].Coverage != "complete" {
		t.Fatalf("history row lacks score, status, or coverage: %+v", page)
	}

	cmd := newServerSecurityCommand()
	var human bytes.Buffer
	cmd.SetOut(&human)
	cmd.SetErr(&human)
	cmd.SetArgs([]string{"history"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(human.String(), "SCORE") || !strings.Contains(human.String(), "COVERAGE") {
		t.Fatalf("history table lacks score and coverage columns: %q", human.String())
	}
}

func TestServerSecurityReportFilters(t *testing.T) {
	fixtureServerScan(t, "CVE-2024-10001")

	decodeReport := func(t *testing.T, out string) serverReportResult {
		t.Helper()
		env := decodeEnvelope(t, out)
		raw, _ := json.Marshal(env.Result)
		var report serverReportResult
		if err := json.Unmarshal(raw, &report); err != nil {
			t.Fatalf("report result is not a report: %v (%s)", err, raw)
		}
		return report
	}

	base, err := runServerSecurity(t, "report", "--json")
	if err != nil {
		t.Fatalf("report failed: %v", err)
	}
	report := decodeReport(t, base)
	if report.Total != 1 || len(report.Findings) != 1 || len(report.Sources) == 0 {
		t.Fatalf("report lacks findings or provenance: %+v", report)
	}
	if report.Score.Version != securityscore.Version || report.Status != "completed" || report.Coverage != "complete" {
		t.Fatalf("report scan fields incomplete: %+v", report)
	}

	for _, tc := range []struct {
		args            []string
		total, findings int
	}{
		{[]string{"--severity", "high"}, 1, 1},
		{[]string{"--severity", "HIGH"}, 1, 1},
		{[]string{"--severity", "low"}, 0, 0},
		{[]string{"--state", "confirmed"}, 1, 1},
		{[]string{"--state", "potential"}, 0, 0},
		{[]string{"--package", "ssl"}, 1, 1},
		{[]string{"--package", "absent"}, 0, 0},
		{[]string{"--limit", "1", "--offset", "1"}, 1, 0},
		{[]string{"--offset", "2"}, 1, 0},
	} {
		out, err := runServerSecurity(t, append([]string{"report", "--json"}, tc.args...)...)
		if err != nil {
			t.Fatalf("report %v failed: %v", tc.args, err)
		}
		got := decodeReport(t, out)
		if got.Total != tc.total || len(got.Findings) != tc.findings {
			t.Fatalf("report %v: total %d findings %d, want %d/%d", tc.args, got.Total, len(got.Findings), tc.total, tc.findings)
		}
	}

	cmd := newServerSecurityCommand()
	var human bytes.Buffer
	cmd.SetOut(&human)
	cmd.SetErr(&human)
	cmd.SetArgs([]string{"report", "--severity", "high"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	humanOut := human.String()
	if !strings.Contains(humanOut, "SEVERITY") || !strings.Contains(humanOut, "CVE-2024-10001") || !strings.Contains(humanOut, "89 (needs_attention)") {
		t.Fatalf("report table lacks findings or score: %q", humanOut)
	}
}

func TestServerSecurityReportWithoutScan(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	_, err := runServerSecurity(t, "report", "--json")
	if err == nil || !strings.Contains(err.Error(), "no host snapshot found") {
		t.Fatalf("expected not-found guidance, got %v", err)
	}
}

func TestServerSecurityConfigCommand(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", dataDir)

	out, err := runServerSecurity(t, "config", "--json")
	if err != nil {
		t.Fatalf("config show failed: %v", err)
	}
	env := decodeEnvelope(t, out)
	raw, _ := json.Marshal(env.Result)
	var result serverHostConfigResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("config result is not a config: %v (%s)", err, raw)
	}
	if result.Reporting || result.ScanInterval != "24h" || result.Updated {
		t.Fatalf("unexpected defaults: %+v", result)
	}

	out, err = runServerSecurity(t, "config", "--json", "--reporting", "--interval", "6h")
	if err != nil {
		t.Fatalf("config update failed: %v", err)
	}
	env = decodeEnvelope(t, out)
	raw, _ = json.Marshal(env.Result)
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Reporting || result.ScanInterval != "6h" || !result.Updated {
		t.Fatalf("update not applied: %+v", result)
	}
	if !hostsecurity.HostReportingEnabled(dataDir) {
		t.Fatal("consent not persisted under the data directory")
	}

	if _, err := runServerSecurity(t, "config", "--json", "--no-reporting"); err != nil {
		t.Fatalf("revoke failed: %v", err)
	}
	if hostsecurity.HostReportingEnabled(dataDir) {
		t.Fatal("consent not revoked")
	}

	_, err = runServerSecurity(t, "config", "--json", "--interval", "30m")
	if err == nil || !strings.Contains(err.Error(), "at least 1h") {
		t.Fatalf("expected interval validation error, got %v", err)
	}
	_, err = runServerSecurity(t, "config", "--json", "--reporting", "--no-reporting")
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected exclusive flags error, got %v", err)
	}

	cmd := newServerSecurityCommand()
	var human bytes.Buffer
	cmd.SetOut(&human)
	cmd.SetErr(&human)
	cmd.SetArgs([]string{"config"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(human.String(), "Host reporting") || !strings.Contains(human.String(), "false") || !strings.Contains(human.String(), "6h") {
		t.Fatalf("human config view incomplete: %q", human.String())
	}
}
