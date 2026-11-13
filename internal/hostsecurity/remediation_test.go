package hostsecurity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// primeTracker seeds the debian advisory tracker cache with a fixture body.
func primeTracker(t *testing.T, intel *Intelligence, tracker map[string]any) {
	t.Helper()
	raw, err := json.Marshal(tracker)
	if err != nil {
		t.Fatal(err)
	}
	primeCache(t, intel, "https://security-tracker.debian.org/tracker/data/json", raw)
}

// aptFixture drives one apt host through preview, apply and verify with a
// deterministic command log.
type aptFixture struct {
	applied  bool
	commands []string
}

func (f *aptFixture) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.commands = append(f.commands, strings.Join(append([]string{name}, args...), " "))
	switch {
	case name == "dpkg-query" && len(args) >= 3 && args[len(args)-1] == "linux-image*":
		return []byte("linux-image-6.1.0-18-amd64\tii \t6.1.0-18\n"), nil
	case name == "dpkg-query" && len(args) == 2:
		ssl := "1.0-1"
		if f.applied {
			ssl = "2.0-1"
		}
		return []byte("ii \tssl\t" + ssl + "\tamd64\tssl\t" + ssl + "\t\t\t\nii \tneon\t1.0-1\tamd64\tneon\t1.0-1\t\t\t\n"), nil
	case name == "dpkg-query" && len(args) >= 3:
		var b strings.Builder
		for _, spec := range args[2:] {
			if spec != "ssl" && spec != "neon" {
				continue
			}
			version := "1.0-1"
			if f.applied && spec == "ssl" {
				version = "2.0-1"
			}
			b.WriteString(spec + "\tii\t" + version + "\n")
		}
		return []byte(b.String()), nil
	case name == "uname" && len(args) == 1 && args[0] == "-r":
		return []byte("6.1.0-18-amd64\n"), nil
	case name == "apt-get" && len(args) >= 2 && args[0] == "-s":
		return []byte("Reading package lists...\nBuilding dependency tree...\nInst ssl [1.0-1] (2.0-1 Debian:12/bookworm)\nConf ssl (2.0-1 Debian:12/bookworm)\nInst neon [1.0-1] (2.0-2 Debian:12/bookworm)\nConf neon (2.0-2 Debian:12/bookworm)\n"), nil
	case name == "env" && len(args) >= 3 && args[1] == "apt-get":
		f.applied = true
		return []byte(""), nil
	}
	return []byte("1.0\n"), nil
}

func remediationHost(manager string) Host {
	return Host{ID: "debian", Version: "12", Manager: manager, Status: "supported"}
}

func runAsRoot(t *testing.T, uid int) {
	t.Helper()
	previous := euidOf
	euidOf = func() int { return uid }
	t.Cleanup(func() { euidOf = previous })
}

func errorCode(t *testing.T, err error) phelixerr.Code {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	return phelixerr.CodeOf(err)
}

func TestPreviewAPTWorkflow(t *testing.T) {
	runAsRoot(t, 0)
	fixture := &aptFixture{}
	system := fixtureSystem(t, "debian")
	system.Runner = fakeRunner(fixture.run)
	dataDir := t.TempDir()
	preview, err := PreviewUpdates(context.Background(), system, remediationHost("apt"), []string{"ssl", "neon"}, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Items) != 2 {
		t.Fatalf("items: %+v", preview.Items)
	}
	if preview.Items[0].Name != "ssl" || preview.Items[0].CurrentVersion != "1.0-1" || preview.Items[0].TargetVersion != "2.0-1" {
		t.Fatalf("ssl item: %+v", preview.Items[0])
	}
	if len(preview.Items[0].DependencyChanges) != 1 || !strings.HasPrefix(preview.Items[0].DependencyChanges[0], "configure ssl (") {
		t.Fatalf("ssl dependency changes: %+v", preview.Items[0].DependencyChanges)
	}
	tokenPattern := regexp.MustCompile(`^[0-9a-f]{32}$`)
	if !tokenPattern.MatchString(preview.Token) {
		t.Fatalf("token shape: %q", preview.Token)
	}
	if preview.ExpiresAt.Sub(preview.CreatedAt) != PreviewTTL {
		t.Fatalf("ttl: %v", preview.ExpiresAt.Sub(preview.CreatedAt))
	}
	if len(preview.Limitations) != 1 {
		t.Fatalf("apt limitations: %+v", preview.Limitations)
	}
	state, err := loadOperations(dataDir)
	if err != nil || len(state.Previews) != 1 || state.Previews[0].Token != preview.Token || state.Previews[0].StateHash == "" {
		t.Fatalf("persisted previews: %+v %v", state.Previews, err)
	}
}

func TestPreviewParsingPerManager(t *testing.T) {
	versions := map[string]string{"ssl": "1.0-1", "openssl": "3.0.7-r0", "kernel-core": "6.1.0-20.fc38", "linux": "6.6.1-1"}
	cases := []struct {
		manager string
		raw     string
		targets map[string]string
	}{
		{"apt", "Inst ssl [1.0-1] (2.0-1)\nConf ssl (2.0-1)\nRemv olddep [0.9]\n", map[string]string{"ssl": "2.0-1"}},
		{"dnf", "Upgrading:\n ssl x86_64 1.0-2.fc38 updates 1.2 M\n kernel-core.x86_64 6.1.0-20.fc38 -> 6.1.0-21.fc38 updates\n\nTransaction Summary\n", map[string]string{"ssl": "1.0-2.fc38", "kernel-core": "6.1.0-21.fc38"}},
		{"zypper", "The following 2 packages are going to be upgraded:\n  ssl  1.0-1 -> 1.0-2\n  openssl  3.0.7 -> 3.0.9\n", map[string]string{"ssl": "1.0-2", "openssl": "3.0.9"}},
		{"apk", "Upgrading ssl (1.0-1 -> 1.0-2)\nUpgrading openssl (3.0.7-r0 -> 3.0.9-r0)\n", map[string]string{"ssl": "1.0-2", "openssl": "3.0.9-r0"}},
		{"pacman", "ssl 1.0-2\nopenssl 3.0.9-1\n", map[string]string{"ssl": "1.0-2", "openssl": "3.0.9-1"}},
	}
	for _, tc := range cases {
		items, warnings := parsePreview(tc.manager, []string{"ssl", "openssl", "kernel-core", "linux"}, versions, []byte(tc.raw))
		targets := map[string]string{}
		for _, item := range items {
			targets[item.Name] = item.TargetVersion
		}
		for name, want := range tc.targets {
			if targets[name] != want {
				t.Fatalf("%s target for %s = %q, want %q", tc.manager, name, targets[name], want)
			}
		}
		if len(warnings) != 4-len(tc.targets) {
			t.Fatalf("%s warnings: %+v", tc.manager, warnings)
		}
	}
	aptRemovals, _ := parsePreview("apt", []string{"ssl"}, versions, []byte("Inst ssl [1.0-1] (2.0-1)\nConf ssl (2.0-1)\nRemv olddep [0.9]\n"))
	if len(aptRemovals[0].Removals) != 1 || aptRemovals[0].Removals[0] != "remove olddep (0.9)" {
		t.Fatalf("apt removals: %+v", aptRemovals[0].Removals)
	}
	noCandidate, _ := parsePreview("apt", []string{"ssl"}, map[string]string{"ssl": "1.0-1"}, []byte("Inst nothing [1] (2)\n"))
	if noCandidate[0].TargetVersion != "1.0-1" || len(noCandidate) != 1 {
		t.Fatalf("no-candidate fallback: %+v", noCandidate)
	}
}

func TestPreviewAndApplyCommandsPerManager(t *testing.T) {
	items := []UpdateItem{{Name: "ssl", CurrentVersion: "1.0-1", TargetVersion: "2.0-1"}, {Name: "held", CurrentVersion: "2.0-1", TargetVersion: "2.0-1"}}
	if name, args := previewCommand("apt", []string{"ssl"}); name != "apt-get" || strings.Join(args, " ") != "-s install ssl" {
		t.Fatalf("apt preview: %s %v", name, args)
	}
	if name, args := previewCommand("dnf", []string{"ssl"}); name != "dnf" || strings.Join(args, " ") != "-q --assumeno upgrade ssl" {
		t.Fatalf("dnf preview: %s %v", name, args)
	}
	if name, args := previewCommand("zypper", []string{"ssl"}); name != "zypper" || strings.Join(args, " ") != "-q --non-interactive update --dry-run ssl" {
		t.Fatalf("zypper preview: %s %v", name, args)
	}
	if name, args := previewCommand("apk", []string{"ssl"}); name != "apk" || strings.Join(args, " ") != "-s upgrade ssl" {
		t.Fatalf("apk preview: %s %v", name, args)
	}
	if name, args := previewCommand("pacman", []string{"ssl"}); name != "pacman" || strings.Join(args, " ") != "--print-format %n %v -Su ssl" {
		t.Fatalf("pacman preview: %s %v", name, args)
	}
	if name, args := applyCommand("apt", items); name != "env" || strings.Join(args, " ") != "DEBIAN_FRONTEND=noninteractive apt-get -y install ssl=2.0-1 held" {
		t.Fatalf("apt apply: %s %v", name, args)
	}
	if name, args := applyCommand("dnf", items); name != "dnf" || strings.Join(args, " ") != "-y upgrade ssl-2.0-1 held" {
		t.Fatalf("dnf apply: %s %v", name, args)
	}
	if name, args := applyCommand("zypper", items); name != "zypper" || strings.Join(args, " ") != "-n in ssl=2.0-1 held" {
		t.Fatalf("zypper apply: %s %v", name, args)
	}
	if name, args := applyCommand("apk", items); name != "apk" || strings.Join(args, " ") != "add ssl=2.0-1 held" {
		t.Fatalf("apk apply: %s %v", name, args)
	}
	if name, args := applyCommand("pacman", items); name != "pacman" || strings.Join(args, " ") != "-S --noconfirm ssl held" {
		t.Fatalf("pacman apply: %s %v", name, args)
	}
	limitations := previewLimitations("pacman")
	if len(limitations) != 1 || limitations[0] != "pacman cannot simulate dependency conflicts or package removals, and cannot install a pinned older version; apply installs latest available" {
		t.Fatalf("pacman limitation: %+v", limitations)
	}
}

func TestPreviewValidationAndUnsupportedManager(t *testing.T) {
	runAsRoot(t, 0)
	system := fixtureSystem(t, "debian")
	dataDir := t.TempDir()
	host := remediationHost("apt")
	for _, bad := range [][]string{nil, {}, {"../escape"}, {"ssl", "ssl"}, {"ssl="}, {"=1.0"}} {
		if _, err := PreviewUpdates(context.Background(), system, host, bad, dataDir); errorCode(t, err) != phelixerr.CodeHostSecurityInvalid {
			t.Fatalf("accepted %v", bad)
		}
	}
	many := make([]string, MaxUpdatePackages+1)
	for i := range many {
		many[i] = fmt.Sprintf("pkg%02d", i)
	}
	if _, err := PreviewUpdates(context.Background(), system, host, many, dataDir); errorCode(t, err) != phelixerr.CodeHostSecurityInvalid {
		t.Fatal("accepted oversized package list")
	}
	if _, err := PreviewUpdates(context.Background(), system, remediationHost("portage"), []string{"ssl"}, dataDir); errorCode(t, err) != phelixerr.CodeHostSecurityUnsupported {
		t.Fatal("accepted unsupported manager")
	}
}

func TestPreviewWarnsAboutMissingPackages(t *testing.T) {
	runAsRoot(t, 0)
	fixture := &aptFixture{}
	system := fixtureSystem(t, "debian")
	system.Runner = fakeRunner(fixture.run)
	preview, err := PreviewUpdates(context.Background(), system, remediationHost("apt"), []string{"ssl", "absent"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Items) != 1 || preview.Items[0].Name != "ssl" {
		t.Fatalf("items: %+v", preview.Items)
	}
	found := false
	for _, warning := range preview.Warnings {
		if strings.Contains(warning, "absent is not installed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing-package warning: %+v", preview.Warnings)
	}
	if _, err := PreviewUpdates(context.Background(), system, remediationHost("apt"), []string{"absent"}, t.TempDir()); errorCode(t, err) != phelixerr.CodeHostSecurityInvalid {
		t.Fatal("accepted preview without installed packages")
	}
}

func TestApplyRequiresRoot(t *testing.T) {
	runAsRoot(t, 1000)
	fixture := &aptFixture{}
	system := fixtureSystem(t, "debian")
	system.Runner = fakeRunner(fixture.run)
	dataDir := t.TempDir()
	_, err := ApplyUpdates(context.Background(), system, remediationHost("apt"), []string{"ssl"}, "token", dataDir)
	if errorCode(t, err) != phelixerr.CodeHostSecurityPrivilege {
		t.Fatalf("privilege: %v", err)
	}
	if len(fixture.commands) != 0 {
		t.Fatalf("commands ran without privilege: %v", fixture.commands)
	}
}

func TestApplyRejectsUnknownAndExpiredToken(t *testing.T) {
	runAsRoot(t, 0)
	fixture := &aptFixture{}
	system := fixtureSystem(t, "debian")
	system.Runner = fakeRunner(fixture.run)
	dataDir := t.TempDir()
	if _, err := ApplyUpdates(context.Background(), system, remediationHost("apt"), []string{"ssl"}, "unknown", dataDir); errorCode(t, err) != phelixerr.CodeHostSecurityNotFound {
		t.Fatalf("unknown token: %v", err)
	}
	expired := storedPreview{Token: "expired", Manager: "apt", Packages: []string{"ssl"}, StateHash: stateHashOf([][2]string{{"ssl", "1.0-1"}}), ExpiresAt: time.Now().UTC().Add(-time.Minute), CreatedAt: time.Now().UTC().Add(-time.Hour), Items: []UpdateItem{{Name: "ssl", CurrentVersion: "1.0-1", TargetVersion: "1.0-1"}}}
	if err := mutateOperations(dataDir, func(data *operationsFile) error {
		data.Previews = append(data.Previews, expired)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyUpdates(context.Background(), system, remediationHost("apt"), []string{"ssl"}, "expired", dataDir); errorCode(t, err) != phelixerr.CodeHostSecurityPreviewExpired {
		t.Fatalf("expired token: %v", err)
	}
	if len(fixture.commands) != 0 {
		t.Fatalf("commands ran on a bad token: %v", fixture.commands)
	}
}

func TestApplySingleActiveOperation(t *testing.T) {
	runAsRoot(t, 0)
	fixture := &aptFixture{}
	system := fixtureSystem(t, "debian")
	system.Runner = fakeRunner(fixture.run)
	dataDir := t.TempDir()
	if err := mutateOperations(dataDir, func(data *operationsFile) error {
		data.Operations = append(data.Operations, storedOperation{OperationID: "hso_active", Type: "update_apply", Status: OperationStatusExecuting, Packages: []string{"other"}, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err := ApplyUpdates(context.Background(), system, remediationHost("apt"), []string{"ssl"}, "token", dataDir)
	if errorCode(t, err) != phelixerr.CodeHostSecurityInvalid {
		t.Fatalf("active operation: %v", err)
	}
}

func TestApplyBusyLock(t *testing.T) {
	runAsRoot(t, 0)
	fixture := &aptFixture{}
	system := fixtureSystem(t, "debian")
	system.Runner = fakeRunner(fixture.run)
	dataDir := t.TempDir()
	preview, err := PreviewUpdates(context.Background(), system, remediationHost("apt"), []string{"ssl"}, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	fixture.commands = nil
	lockPath := filepath.Join(system.Root, "var/lib/dpkg/lock-frontend")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := acquireHostLock(lockPath, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = releaseHostLock(held)
		_ = held.Close()
	})
	_, err = ApplyUpdates(context.Background(), system, remediationHost("apt"), []string{"ssl"}, preview.Token, dataDir)
	if errorCode(t, err) != phelixerr.CodeHostSecurityBusy {
		t.Fatalf("busy: %v", err)
	}
	for _, command := range fixture.commands {
		if strings.HasPrefix(command, "dpkg-query") || strings.HasPrefix(command, "env") {
			t.Fatalf("executed while busy: %v", fixture.commands)
		}
	}
	state, loadErr := loadOperations(dataDir)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(state.Operations) != 1 || state.Operations[0].Status != OperationStatusFailed {
		t.Fatalf("persisted operations: %+v", state.Operations)
	}
}

func TestApplyStalePreview(t *testing.T) {
	runAsRoot(t, 0)
	fixture := &aptFixture{}
	system := fixtureSystem(t, "debian")
	system.Runner = fakeRunner(fixture.run)
	dataDir := t.TempDir()
	preview, err := PreviewUpdates(context.Background(), system, remediationHost("apt"), []string{"ssl"}, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	fixture.applied = true
	_, err = ApplyUpdates(context.Background(), system, remediationHost("apt"), []string{"ssl"}, preview.Token, dataDir)
	if errorCode(t, err) != phelixerr.CodeHostSecurityPreviewStale {
		t.Fatalf("stale: %v", err)
	}
	for _, command := range fixture.commands {
		if strings.HasPrefix(command, "env") {
			t.Fatalf("executed on a stale preview: %v", fixture.commands)
		}
	}
	state, loadErr := loadOperations(dataDir)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(state.Operations) != 1 || state.Operations[0].Status != OperationStatusFailed {
		t.Fatalf("persisted operations: %+v", state.Operations)
	}
}

func TestApplySuccessAndVerifiedRemediation(t *testing.T) {
	runAsRoot(t, 0)
	fixture := &aptFixture{}
	system := fixtureSystem(t, "debian")
	system.Runner = fakeRunner(fixture.run)
	dataDir := t.TempDir()
	preview, err := PreviewUpdates(context.Background(), system, remediationHost("apt"), []string{"ssl", "neon"}, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	result, err := ApplyUpdates(context.Background(), system, remediationHost("apt"), []string{"ssl", "neon"}, preview.Token, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != OperationStatusSucceeded || result.Manager != "apt" || strings.HasPrefix(result.OperationID, "hso_") == false {
		t.Fatalf("result: %+v", result)
	}
	apply := "env DEBIAN_FRONTEND=noninteractive apt-get -y install ssl=2.0-1 neon=2.0-2"
	found := false
	for _, command := range fixture.commands {
		if command == apply {
			found = true
		}
	}
	if !found {
		t.Fatalf("apply command missing: %v", fixture.commands)
	}
	state, loadErr := loadOperations(dataDir)
	if loadErr != nil || len(state.Operations) != 1 || state.Operations[0].Status != OperationStatusSucceeded {
		t.Fatalf("persisted operations: %+v %v", state.Operations, loadErr)
	}

	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	intel := &Intelligence{CacheDir: t.TempDir()}
	tracker := map[string]any{
		"ssl":  map[string]any{"CVE-2024-10001": map[string]any{"releases": map[string]any{"bookworm": map[string]any{"status": "resolved", "fixed_version": "2.0-1", "urgency": "high"}}}},
		"neon": map[string]any{"CVE-2024-10003": map[string]any{"releases": map[string]any{"bookworm": map[string]any{"status": "open", "fixed_version": "", "urgency": "high"}}}},
	}
	primeTracker(t, intel, tracker)
	enrichment := httptest.NewServer(unavailableHandler{})
	defer enrichment.Close()
	intel.URLs = map[string]string{}
	for _, key := range []string{"osv-CVE-2024-10001", "github-CVE-2024-10001", "nvd-CVE-2024-10001", "osv-CVE-2024-10003", "github-CVE-2024-10003", "nvd-CVE-2024-10003", "cisa-kev", "epss"} {
		intel.URLs[key] = enrichment.URL
	}
	scan, entries, err := Verify(context.Background(), system, store, "test-server", result.OperationID, dataDir, intel)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries: %+v", entries)
	}
	byPackage := map[string]VerificationEntry{}
	for _, entry := range entries {
		byPackage[entry.Package] = entry
	}
	if !byPackage["ssl"].Resolved {
		t.Fatalf("ssl entry: %+v", byPackage["ssl"])
	}
	if byPackage["neon"].Resolved || !strings.Contains(byPackage["neon"].Detail, "CVE-2024-10003") {
		t.Fatalf("neon entry: %+v", byPackage["neon"])
	}
	if scan.Coverage != "complete" {
		t.Fatalf("scan: %+v", scan)
	}
	state, loadErr = loadOperations(dataDir)
	if loadErr != nil || state.Operations[0].Status != OperationStatusVerified || state.Operations[0].Result.Status != OperationStatusVerified {
		t.Fatalf("verified operation: %+v %v", state.Operations[0], loadErr)
	}
	if _, _, err := Verify(context.Background(), system, store, "test-server", "hso_missing", dataDir, intel); errorCode(t, err) != phelixerr.CodeHostSecurityNotFound {
		t.Fatalf("unknown operation: %v", err)
	}
}

func TestApplyLockOutputDetection(t *testing.T) {
	runAsRoot(t, 0)
	fixture := &aptFixture{}
	system := fixtureSystem(t, "debian")
	system.Runner = fakeRunner(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		fixture.commands = append(fixture.commands, name)
		if name == "dpkg-query" {
			return fixture.run(ctx, name, args...)
		}
		if name == "env" {
			return []byte("E: Could not get lock /var/lib/dpkg/lock-frontend - open (11: Resource temporarily unavailable)\n"), errors.New("apt-get failed")
		}
		return fixture.run(ctx, name, args...)
	})
	dataDir := t.TempDir()
	preview, err := PreviewUpdates(context.Background(), system, remediationHost("apt"), []string{"ssl"}, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ApplyUpdates(context.Background(), system, remediationHost("apt"), []string{"ssl"}, preview.Token, dataDir)
	if errorCode(t, err) != phelixerr.CodeHostSecurityBusy {
		t.Fatalf("lock output: %v", err)
	}
}
