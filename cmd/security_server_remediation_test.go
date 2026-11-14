package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abdorrahmani/phelix/internal/hostsecurity"
)

// remediationFixture drives the preview, apply and verify CLI commands
// against one Debian 12 fixture host whose ssl package upgrades to 2.0-1
// once the apply command runs.
type remediationFixture struct {
	applied bool
	root    string
}

func (f *remediationFixture) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	switch {
	case name == "apt":
		return []byte("apt 2.6.1 (fixture)\n"), nil
	case name == "uname" && len(args) == 1 && args[0] == "-m":
		return []byte("x86_64\n"), nil
	case name == "uname" && len(args) == 1 && args[0] == "-r":
		return []byte("6.1.0-18-amd64\n"), nil
	case name == "dpkg-query" && len(args) == 3 && args[len(args)-1] == "linux-image*":
		return []byte("linux-image-6.1.0-18-amd64\tii \t6.1.0-18\n"), nil
	case name == "dpkg-query" && len(args) == 2:
		ssl := "1.0-1"
		if f.applied {
			ssl = "2.0-1"
		}
		return []byte("ii \tssl\t" + ssl + "\tamd64\tssl\t" + ssl + "\tDebian\t\t\n"), nil
	case name == "dpkg-query" && len(args) >= 3:
		var b strings.Builder
		for _, spec := range args[2:] {
			if spec != "ssl" {
				continue
			}
			version := "1.0-1"
			if f.applied {
				version = "2.0-1"
			}
			b.WriteString(spec + "\tii\t" + version + "\n")
		}
		return []byte(b.String()), nil
	case name == "apt-get" && len(args) >= 2 && args[0] == "-s":
		return []byte("Reading package lists...\nBuilding dependency tree...\nInst ssl [1.0-1] (2.0-1 Debian:12/bookworm)\nConf ssl (2.0-1 Debian:12/bookworm)\n"), nil
	case name == "env" && len(args) >= 3 && args[1] == "apt-get":
		f.applied = true
		return []byte(""), nil
	}
	return nil, fmt.Errorf("unexpected command %s %v", name, args)
}

// remediationFixtureSystem builds the fixture host and the mutable fixture
// state.
func remediationFixtureSystem(t *testing.T) (*remediationFixture, hostsecurity.System) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/os-release"),
		[]byte("ID=debian\nVERSION_ID=12\nVERSION_CODENAME=bookworm\nPRETTY_NAME=\"Debian GNU/Linux 12\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := &remediationFixture{root: root}
	return fixture, hostsecurity.System{
		Root:   root,
		Lookup: func(name string) (string, error) { return name, nil },
		Runner: fixtureRunnerFunc(fixture.run),
	}
}

// remediationAdvisoryServer serves the tracker plus enrichment endpoints
// for one CVE whose fix is the given version.
func remediationAdvisoryServer(t *testing.T, fixed string) *httptest.Server {
	t.Helper()
	tracker := map[string]any{
		"ssl": map[string]any{
			"CVE-2024-10001": map[string]any{
				"description": "fixture advisory",
				"releases": map[string]any{
					"bookworm": map[string]any{"status": "open", "fixed_version": fixed, "urgency": "high**"},
				},
			},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/tracker/data/json", func(w http.ResponseWriter, r *http.Request) { writeJSONBody(t, w, tracker) })
	mux.HandleFunc("/v1/vulns/CVE-2024-10001", func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(t, w, map[string]any{"id": "CVE-2024-10001", "published": "2024-01-01T00:00:00Z", "modified": "2024-01-02T00:00:00Z", "references": []any{}})
	})
	mux.HandleFunc("/advisories", func(w http.ResponseWriter, r *http.Request) { writeJSONBody(t, w, []any{}) })
	mux.HandleFunc("/rest/json/cves/2.0", func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(t, w, map[string]any{"vulnerabilities": []any{}})
	})
	mux.HandleFunc("/sites/default/files/feeds/known_exploited_vulnerabilities.json", func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(t, w, map[string]any{"vulnerabilities": []any{}})
	})
	mux.HandleFunc("/data/v1/epss", func(w http.ResponseWriter, r *http.Request) { writeJSONBody(t, w, map[string]any{"data": []any{}}) })
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// installRemediationSeams points the host system and intelligence seams at
// the remediation fixture for the duration of one test.
func installRemediationSeams(t *testing.T, fixture *remediationFixture, system hostsecurity.System, server *httptest.Server) {
	t.Helper()
	prevSystem := newServerHostSystem
	newServerHostSystem = func() hostsecurity.System { return system }
	t.Cleanup(func() { newServerHostSystem = prevSystem })
	prevClient := newServerIntelClient
	newServerIntelClient = func() *http.Client { return &http.Client{Transport: serverRedirectTransport{server}} }
	t.Cleanup(func() { newServerIntelClient = prevClient })
	prevProbe := hostsecurity.PrivilegeProbe
	hostsecurity.PrivilegeProbe = func() int { return 0 }
	t.Cleanup(func() { hostsecurity.PrivilegeProbe = prevProbe })
	_ = fixture
}

func TestServerSecurityPreviewCommand(t *testing.T) {
	server := remediationAdvisoryServer(t, "2.0-1")
	dataDir := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", dataDir)
	fixture, system := remediationFixtureSystem(t)
	installRemediationSeams(t, fixture, system, server)

	if _, err := runServerSecurity(t, "preview", "--json"); err == nil || !strings.Contains(err.Error(), "--package is required") {
		t.Fatalf("preview without packages: %v", err)
	}
	if _, err := runServerSecurity(t, "preview", "--json", "--package", "../escape"); err == nil {
		t.Fatal("preview accepted an invalid package spec")
	}
	if _, err := runServerSecurity(t, "preview", "--json", "--package", "absent"); err == nil {
		t.Fatal("preview accepted a request with no installed packages")
	}

	out, err := runServerSecurity(t, "preview", "--json", "--package", "ssl")
	if err != nil {
		t.Fatalf("preview failed: %v", err)
	}
	env := decodeEnvelope(t, out)
	raw, _ := json.Marshal(env.Result)
	var preview hostsecurity.Preview
	if err := json.Unmarshal(raw, &preview); err != nil {
		t.Fatalf("preview result is not a preview: %v (%s)", err, raw)
	}
	if len(preview.Token) != 32 || preview.Manager != "apt" || len(preview.Items) != 1 || preview.Items[0].TargetVersion != "2.0-1" || preview.Items[0].CurrentVersion != "1.0-1" {
		t.Fatalf("unexpected preview: %+v", preview)
	}
	if len(preview.Limitations) == 0 {
		t.Fatal("preview carries no limitations")
	}

	cmd := newServerSecurityCommand()
	var human strings.Builder
	cmd.SetOut(&human)
	cmd.SetErr(&human)
	cmd.SetArgs([]string{"preview", "--package", "ssl"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(human.String(), "Preview token") || !strings.Contains(human.String(), "PACKAGE") || !strings.Contains(human.String(), "ssl\t1.0-1\t2.0-1") {
		t.Fatalf("human preview incomplete: %q", human.String())
	}
}

func TestServerSecurityRemediationFlow(t *testing.T) {
	server := remediationAdvisoryServer(t, "2.0-1")
	dataDir := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", dataDir)
	fixture, system := remediationFixtureSystem(t)
	installRemediationSeams(t, fixture, system, server)

	out, err := runServerSecurity(t, "preview", "--json", "--package", "ssl")
	if err != nil {
		t.Fatalf("preview failed: %v", err)
	}
	env := decodeEnvelope(t, out)
	raw, _ := json.Marshal(env.Result)
	var preview hostsecurity.Preview
	if err := json.Unmarshal(raw, &preview); err != nil {
		t.Fatal(err)
	}

	// Apply mutates the package database: without --yes and without an
	// interactive confirmation the command refuses to run.
	_, err = runServerSecurity(t, "apply", "--json", "--preview-token", preview.Token)
	if err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Fatalf("apply without confirmation: %v", err)
	}
	if fixture.applied {
		t.Fatal("apply mutated the host without confirmation")
	}

	out, err = runServerSecurity(t, "apply", "--json", "--preview-token", preview.Token, "--yes")
	if err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	env = decodeEnvelope(t, out)
	raw, _ = json.Marshal(env.Result)
	var result hostsecurity.UpdateResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("apply result is not an update result: %v (%s)", err, raw)
	}
	if !strings.HasPrefix(result.OperationID, "hso_") || result.Status != "succeeded" || result.Manager != "apt" {
		t.Fatalf("unexpected apply result: %+v", result)
	}
	if !fixture.applied {
		t.Fatal("apply did not run the manager command")
	}

	out, err = runServerSecurity(t, "verify", "--json", "--operation-id", result.OperationID)
	if err != nil {
		t.Fatalf("verify failed: %v", err)
	}
	env = decodeEnvelope(t, out)
	raw, _ = json.Marshal(env.Result)
	var verified serverVerifyResult
	if err := json.Unmarshal(raw, &verified); err != nil {
		t.Fatalf("verify result is not a verification: %v (%s)", err, raw)
	}
	if verified.Status != "verified" || verified.ScanID == "" || len(verified.Verification) != 1 {
		t.Fatalf("unexpected verification: %+v", verified)
	}
	entry := verified.Verification[0]
	if entry.Package != "ssl" || !entry.Resolved || entry.Detail == "" {
		t.Fatalf("unexpected verification entry: %+v", entry)
	}

	if _, err := runServerSecurity(t, "verify", "--json"); err == nil || !strings.Contains(err.Error(), "--operation-id is required") {
		t.Fatalf("verify without operation id: %v", err)
	}
	if _, err := runServerSecurity(t, "verify", "--json", "--operation-id", "hso_missing"); err == nil {
		t.Fatal("verify accepted an unknown operation")
	}

	out, err = runServerSecurity(t, "reboot-status", "--json")
	if err != nil {
		t.Fatalf("reboot-status failed: %v", err)
	}
	env = decodeEnvelope(t, out)
	raw, _ = json.Marshal(env.Result)
	var state hostsecurity.RebootState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("reboot result is not a reboot state: %v (%s)", err, raw)
	}
	if state.Required || len(state.Reasons) != 0 || state.RunningKernel != "6.1.0-18-amd64" {
		t.Fatalf("unexpected reboot state: %+v", state)
	}

	// Human output for every remediation surface. Apply gets a fresh
	// preview: the first apply already moved ssl to 2.0-1, so reusing its
	// token would rightly fail as stale.
	out, err = runServerSecurity(t, "preview", "--json", "--package", "ssl")
	if err != nil {
		t.Fatalf("second preview failed: %v", err)
	}
	env = decodeEnvelope(t, out)
	raw, _ = json.Marshal(env.Result)
	var renewed hostsecurity.Preview
	if err := json.Unmarshal(raw, &renewed); err != nil {
		t.Fatal(err)
	}
	cmd := newServerSecurityCommand()
	var human strings.Builder
	cmd.SetOut(&human)
	cmd.SetErr(&human)
	cmd.SetArgs([]string{"apply", "--preview-token", renewed.Token, "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(human.String(), "Operation") || !strings.Contains(human.String(), "succeeded") {
		t.Fatalf("human apply incomplete: %q", human.String())
	}
	renewedOperation := ""
	for _, line := range strings.Split(human.String(), "\n") {
		if strings.HasPrefix(line, "Operation") {
			renewedOperation = strings.TrimSpace(strings.TrimPrefix(line, "Operation"))
		}
	}
	if !strings.HasPrefix(renewedOperation, "hso_") {
		t.Fatalf("human apply lacks the operation id: %q", human.String())
	}

	cmd = newServerSecurityCommand()
	human.Reset()
	cmd.SetOut(&human)
	cmd.SetErr(&human)
	cmd.SetArgs([]string{"verify", "--operation-id", renewedOperation})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(human.String(), "RESOLVED") || !strings.Contains(human.String(), "no remaining findings for this package") {
		t.Fatalf("human verify incomplete: %q", human.String())
	}

	cmd = newServerSecurityCommand()
	human.Reset()
	cmd.SetOut(&human)
	cmd.SetErr(&human)
	cmd.SetArgs([]string{"reboot-status"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(human.String(), "Reboot required") || !strings.Contains(human.String(), "false") || !strings.Contains(human.String(), "Running kernel") || !strings.Contains(human.String(), "6.1.0-18-amd64") {
		t.Fatalf("human reboot incomplete: %q", human.String())
	}
}

func TestServerSecurityApplyNeedsStoredPreview(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", dataDir)
	if _, err := runServerSecurity(t, "apply", "--json", "--yes"); err == nil || !strings.Contains(err.Error(), "no stored preview") {
		t.Fatalf("apply without a stored preview: %v", err)
	}
}

// TestServerSecurityVerifyUnresolvedFindings pins the verification status
// contract: when the verification scan still finds advisories for a
// requested package, the verify result must not fabricate a "verified"
// status. The status stays "succeeded" (the apply completed), the entries
// carry the remaining findings, and the human output names the unresolved
// packages clearly.
func TestServerSecurityVerifyUnresolvedFindings(t *testing.T) {
	server := remediationAdvisoryServer(t, "3.0-1")
	dataDir := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", dataDir)
	fixture, system := remediationFixtureSystem(t)
	installRemediationSeams(t, fixture, system, server)

	out, err := runServerSecurity(t, "preview", "--json", "--package", "ssl")
	if err != nil {
		t.Fatalf("preview failed: %v", err)
	}
	env := decodeEnvelope(t, out)
	raw, _ := json.Marshal(env.Result)
	var preview hostsecurity.Preview
	if err := json.Unmarshal(raw, &preview); err != nil {
		t.Fatal(err)
	}
	out, err = runServerSecurity(t, "apply", "--json", "--preview-token", preview.Token, "--yes")
	if err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	env = decodeEnvelope(t, out)
	raw, _ = json.Marshal(env.Result)
	var result hostsecurity.UpdateResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}

	out, err = runServerSecurity(t, "verify", "--json", "--operation-id", result.OperationID)
	if err != nil {
		t.Fatalf("verify failed: %v", err)
	}
	env = decodeEnvelope(t, out)
	raw, _ = json.Marshal(env.Result)
	var verified serverVerifyResult
	if err := json.Unmarshal(raw, &verified); err != nil {
		t.Fatal(err)
	}
	if verified.Status != "succeeded" {
		t.Fatalf("an unresolved entry must keep the status at succeeded: %+v", verified)
	}
	if len(verified.Verification) != 1 || verified.Verification[0].Resolved ||
		!strings.Contains(verified.Verification[0].Detail, "CVE-2024-10001") {
		t.Fatalf("the remaining findings must stay in the entries: %+v", verified.Verification)
	}

	// The human output needs its own operation: verifying twice is refused.
	out, err = runServerSecurity(t, "preview", "--json", "--package", "ssl")
	if err != nil {
		t.Fatalf("second preview failed: %v", err)
	}
	env = decodeEnvelope(t, out)
	raw, _ = json.Marshal(env.Result)
	var renewed hostsecurity.Preview
	if err := json.Unmarshal(raw, &renewed); err != nil {
		t.Fatal(err)
	}
	out, err = runServerSecurity(t, "apply", "--json", "--preview-token", renewed.Token, "--yes")
	if err != nil {
		t.Fatalf("second apply failed: %v", err)
	}
	env = decodeEnvelope(t, out)
	raw, _ = json.Marshal(env.Result)
	var renewedResult hostsecurity.UpdateResult
	if err := json.Unmarshal(raw, &renewedResult); err != nil {
		t.Fatal(err)
	}

	cmd := newServerSecurityCommand()
	var human strings.Builder
	cmd.SetOut(&human)
	cmd.SetErr(&human)
	cmd.SetArgs([]string{"verify", "--operation-id", renewedResult.OperationID})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(human.String(), "Unresolved") || !strings.Contains(human.String(), "1 of 1 requested package(s) still have findings") {
		t.Fatalf("human verify must name the unresolved packages: %q", human.String())
	}
	if !strings.Contains(human.String(), "remaining findings: CVE-2024-10001") {
		t.Fatalf("human verify must show the remaining findings: %q", human.String())
	}
}

// TestVerificationStatus pins the status resolver: "verified" only when
// every requested package resolved, otherwise the apply's "succeeded".
func TestVerificationStatus(t *testing.T) {
	allResolved := []hostsecurity.VerificationEntry{{Package: "ssl", Resolved: true, Detail: "no remaining findings for this package"}}
	if got := verificationStatus(allResolved); got != "verified" {
		t.Fatalf("a fully resolved verification must stay verified: %q", got)
	}
	partial := []hostsecurity.VerificationEntry{
		{Package: "ssl", Resolved: true, Detail: "no remaining findings for this package"},
		{Package: "neon", Resolved: false, Detail: "remaining findings: CVE-2024-10003"},
	}
	if got := verificationStatus(partial); got != "succeeded" {
		t.Fatalf("an unresolved entry must keep the status at succeeded: %q", got)
	}
	if got := verificationStatus(nil); got != "succeeded" {
		t.Fatalf("an entryless verification must not claim verified: %q", got)
	}
}
