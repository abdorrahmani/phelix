package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runPhelixSecurity runs the binary in an isolated HOME with an OSV
// endpoint override and a working directory.
func runPhelixSecurity(t *testing.T, home, dir, osvURL string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(phelixBin(t), args...)
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"PHELIX_OSV_URL="+osvURL,
	)
	cmd.Dir = dir
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("failed to run phelix %v: %v", args, err)
		}
	}
	return code, stdout.String(), stderr.String()
}

// securityOSV serves a minimal OSV.dev: one advisory for one package with
// a configurable severity.
func securityOSV(t *testing.T, severity string) *httptest.Server {
	advisory := fmt.Sprintf(`{
		"id": "GO-2024-3001",
		"aliases": ["CVE-2024-3001"],
		"summary": "test advisory",
		"database_specific": {"severity": %q},
		"affected": [{
			"package": {"ecosystem": "Go", "name": "example.com/vuln"},
			"ranges": [{"type": "ECOSYSTEM", "events": [{"introduced": "0"}, {"fixed": "2.0.0"}]}]
		}]
	}`, severity)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/querybatch":
			_, _ = w.Write([]byte(`{"results":[{"vulns":[{"id":"GO-2024-3001"}]}]}`))
		case r.URL.Path == "/v1/vulns/GO-2024-3001":
			_, _ = w.Write([]byte(advisory))
		default:
			http.NotFound(w, r)
		}
	}))
}

// writeSecurityProject writes a Go project with one vulnerable dependency.
func writeSecurityProject(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	content := fmt.Sprintf("module example.com/%s\n\ngo 1.24\n\nrequire example.com/vuln v1.0.0\n", name)
	phelix := fmt.Sprintf("name: %s\n", name)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "phelix.yaml"), []byte(phelix), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func decodeSecurityEnvelope(t *testing.T, stdout string) map[string]any {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("stdout is not a JSON envelope: %v\n%s", err, stdout)
	}
	if envelope["schema_version"] != "1" {
		t.Fatalf("envelope schema_version wrong: %v", envelope["schema_version"])
	}
	return envelope
}

func TestCLISecurityScan_BelowThreshold_Exit0(t *testing.T) {
	server := securityOSV(t, "low")
	defer server.Close()
	home := t.TempDir()
	project := writeSecurityProject(t, "lowapp")

	code, stdout, stderr := runPhelixSecurity(t, home, project, server.URL, "security", "scan", "--json")
	if code != 0 {
		t.Fatalf("low severity must stay below the default high threshold: exit=%d stderr=%s", code, stderr)
	}
	envelope := decodeSecurityEnvelope(t, stdout)
	if envelope["status"] != "succeeded" {
		t.Fatalf("scan below threshold must succeed: %v", envelope["status"])
	}
	result := envelope["result"].(map[string]any)
	if result["threshold_tripped"] != false {
		t.Fatalf("threshold_tripped must be false: %v", result)
	}
	if result["fail_threshold"] != "high" {
		t.Fatalf("threshold must be reported: %v", result["fail_threshold"])
	}
}

func TestCLISecurityScan_AtThreshold_Exit25(t *testing.T) {
	server := securityOSV(t, "high")
	defer server.Close()
	home := t.TempDir()
	project := writeSecurityProject(t, "highapp")

	code, stdout, stderr := runPhelixSecurity(t, home, project, server.URL, "security", "scan", "--json")
	if code != 25 {
		t.Fatalf("high severity at the high threshold must exit 25: exit=%d stderr=%s", code, stderr)
	}
	envelope := decodeSecurityEnvelope(t, stdout)
	if envelope["status"] != "succeeded" {
		t.Fatalf("the scan itself succeeded; one envelope with results is required: %v", envelope)
	}
	result := envelope["result"].(map[string]any)
	if result["threshold_tripped"] != true {
		t.Fatalf("threshold_tripped must be true: %v", result)
	}
	findings := result["findings"].([]any)
	if len(findings) != 1 {
		t.Fatalf("one finding expected: %v", findings)
	}
	finding := findings[0].(map[string]any)
	if finding["state"] != "potentially_affected" || finding["severity"] != "high" {
		t.Fatalf("finding wrong: %v", finding)
	}
	if finding["reachability"] != "unknown" {
		t.Fatalf("reachability must be unknown: %v", finding)
	}
	if strings.Contains(stdout, "\nScan ") {
		t.Fatal("human output must not leak into machine stdout")
	}
}

func TestCLISecurityScan_ConnectionFailure_Exit30(t *testing.T) {
	home := t.TempDir()
	project := writeSecurityProject(t, "offlineapp")

	code, _, stderr := runPhelixSecurity(t, home, project, "http://127.0.0.1:1", "security", "scan")
	if code != 30 {
		t.Fatalf("unreachable source with no cache must exit 30: exit=%d stderr=%s", code, stderr)
	}
}

func TestCLISecurityScan_NonTTYAmbiguous_Exit2(t *testing.T) {
	home := t.TempDir()
	empty := t.TempDir()

	code, _, stderr := runPhelixSecurity(t, home, empty, "http://127.0.0.1:1", "security", "scan")
	if code != 2 {
		t.Fatalf("ambiguous non-interactive scan must be a usage error: exit=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "phelix.yaml") {
		t.Fatalf("error must explain the resolution rule: %s", stderr)
	}
}

func TestCLISecurityScan_PartialCoverage_Exit30(t *testing.T) {
	server := securityOSV(t, "low")
	defer server.Close()
	home := t.TempDir()
	project := writeSecurityProject(t, "coverageapp")

	if code, _, stderr := runPhelixSecurity(t, home, project, server.URL, "security", "scan"); code != 0 {
		t.Fatalf("fully covered online scan: exit=%d stderr=%s", code, stderr)
	}
	// A second dependency the cache knows nothing about, scanned offline.
	content, err := os.ReadFile(filepath.Join(project, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	grown := strings.Replace(string(content), "require example.com/vuln v1.0.0",
		"require (\n\texample.com/vuln v1.0.0\n\texample.com/uncached v1.0.0\n)", 1)
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte(grown), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runPhelixSecurity(t, home, project, "http://127.0.0.1:1", "security", "scan", "--json")
	if code != 30 {
		t.Fatalf("partial offline coverage with no findings must fail closed: exit=%d stderr=%s", code, stderr)
	}
	envelope := decodeSecurityEnvelope(t, stdout)
	result := envelope["result"].(map[string]any)
	if result["advisory_coverage"] != "partial" {
		t.Fatalf("coverage must be explicit in JSON: %v", result["advisory_coverage"])
	}
}

func TestCLISecurityReport_RequiresCompletedScan_Exit12(t *testing.T) {
	home := t.TempDir()
	project := writeSecurityProject(t, "reportapp")

	code, _, stderr := runPhelixSecurity(t, home, project, "http://127.0.0.1:1", "security", "report")
	if code != 12 {
		t.Fatalf("report without scans must be NOT_FOUND: exit=%d stderr=%s", code, stderr)
	}
}

func TestCLISecurityReport_AndDiff(t *testing.T) {
	server := securityOSV(t, "low")
	defer server.Close()
	home := t.TempDir()
	project := writeSecurityProject(t, "diffapp")

	if code, _, stderr := runPhelixSecurity(t, home, project, server.URL, "security", "scan"); code != 0 {
		t.Fatalf("first scan: exit=%d stderr=%s", code, stderr)
	}
	if code, _, stderr := runPhelixSecurity(t, home, project, server.URL, "security", "scan"); code != 0 {
		t.Fatalf("second scan: exit=%d stderr=%s", code, stderr)
	}

	code, stdout, _ := runPhelixSecurity(t, home, project, server.URL, "security", "report", "--json", "--previous")
	if code != 0 {
		t.Fatalf("report: unexpected exit %d", code)
	}
	envelope := decodeSecurityEnvelope(t, stdout)
	result := envelope["result"].(map[string]any)
	scan := result["scan"].(map[string]any)
	if scan["status"] != "completed" {
		t.Fatalf("report must show the completed scan: %v", scan["status"])
	}
}

func TestCLISecurityConfig_ShowsDefaults(t *testing.T) {
	home := t.TempDir()
	project := writeSecurityProject(t, "cfgapp")

	code, stdout, stderr := runPhelixSecurity(t, home, project, "http://127.0.0.1:1", "security", "config", "--json")
	if code != 0 {
		t.Fatalf("config: exit=%d stderr=%s", code, stderr)
	}
	envelope := decodeSecurityEnvelope(t, stdout)
	result := envelope["result"].(map[string]any)
	if result["enabled"] != false || result["scan_interval"] != "24h0m0s" || result["fail_threshold"] != "high" {
		t.Fatalf("defaults wrong: %v", result)
	}
}

func TestCLISecurityShow_FetchesAndCaches(t *testing.T) {
	server := securityOSV(t, "high")
	defer server.Close()
	home := t.TempDir()
	project := t.TempDir()

	code, stdout, stderr := runPhelixSecurity(t, home, project, server.URL, "security", "show", "GO-2024-3001", "--json")
	if code != 0 {
		t.Fatalf("show: exit=%d stderr=%s", code, stderr)
	}
	envelope := decodeSecurityEnvelope(t, stdout)
	result := envelope["result"].(map[string]any)
	if result["cached"] != false {
		t.Fatalf("first fetch must miss the cache: %v", result)
	}

	// A second show with the endpoint down must serve the cache.
	code, _, stderr = runPhelixSecurity(t, home, project, "http://127.0.0.1:1", "security", "show", "GO-2024-3001", "--json")
	if code != 0 {
		t.Fatalf("cached show must survive an unreachable endpoint: exit=%d stderr=%s", code, stderr)
	}
}
