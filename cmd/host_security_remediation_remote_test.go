package cmd

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/hostsecurity"
)

// TestRemoteHostSecurityRemediation pins the remote preview, apply and
// verify wiring: consent gates the mutations, the engine's error codes
// travel verbatim, a successful apply yields an hso_ operation, and verify
// reports per-package findings from a fresh scan.
func TestRemoteHostSecurityRemediation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".phelix"), 0o755); err != nil {
		t.Fatal(err)
	}
	hostSecuritySetConsent(t, false)
	previewOff := RemoteHostSecurityCommand(hostSecurityRequest("off-preview", phelixgrpc.HostSecurityCommandUpdatePreview,
		&pb.HostSecurityCommandOptions{Packages: []string{"ssl"}, ProtocolVersion: 1}))
	if previewOff.GetErrorCode() != "HOST_SECURITY_CONSENT_DISABLED" {
		t.Fatalf("preview must be refused without consent: %+v", previewOff)
	}
	hostSecuritySetConsent(t, true)

	server := remediationAdvisoryServer(t, "2.0-1")
	_, system := remediationFixtureSystem(t)
	prevSystem := newServerHostSystem
	newServerHostSystem = func() hostsecurity.System { return system }
	t.Cleanup(func() { newServerHostSystem = prevSystem })
	prevIntel := serverIntelligence
	serverIntelligence = func(dataDir string) *hostsecurity.Intelligence {
		return &hostsecurity.Intelligence{
			CacheDir: filepath.Join(securityTestDataDir(), "host-security", "intel-cache"),
			HTTP:     &http.Client{Transport: serverRedirectTransport{server}},
		}
	}
	t.Cleanup(func() { serverIntelligence = prevIntel })
	prevProbe := hostsecurity.PrivilegeProbe
	hostsecurity.PrivilegeProbe = func() int { return 0 }
	t.Cleanup(func() { hostsecurity.PrivilegeProbe = prevProbe })

	preview := RemoteHostSecurityCommand(hostSecurityRequest("p1", phelixgrpc.HostSecurityCommandUpdatePreview,
		&pb.HostSecurityCommandOptions{Packages: []string{"ssl"}, ProtocolVersion: 1}))
	if preview.GetStatus() != "success" || preview.GetHostSecurityPreview().GetPreviewToken() == "" {
		t.Fatalf("preview: %+v", preview)
	}
	wire := preview.GetHostSecurityPreview()
	if wire.GetManager() != "apt" || len(wire.GetItems()) != 1 || wire.GetItems()[0].GetName() != "ssl" ||
		wire.GetItems()[0].GetCurrentVersion() != "1.0-1" || wire.GetItems()[0].GetTargetVersion() != "2.0-1" {
		t.Fatalf("preview wire: %+v", wire)
	}
	if wire.GetExpiresAtMs() <= wire.GetCreatedAtMs() || wire.GetRebootRequired() {
		t.Fatalf("preview expiry: %+v", wire)
	}
	token := wire.GetPreviewToken()

	badToken := RemoteHostSecurityCommand(hostSecurityRequest("a0", phelixgrpc.HostSecurityCommandUpdateApply,
		&pb.HostSecurityCommandOptions{Packages: []string{"ssl"}, PreviewToken: "nope", ProtocolVersion: 1}))
	if badToken.GetErrorCode() != "HOST_SECURITY_NOT_FOUND" {
		t.Fatalf("unknown token: %+v", badToken)
	}
	mismatch := RemoteHostSecurityCommand(hostSecurityRequest("a1", phelixgrpc.HostSecurityCommandUpdateApply,
		&pb.HostSecurityCommandOptions{Packages: []string{"other"}, PreviewToken: token, ProtocolVersion: 1}))
	if mismatch.GetErrorCode() != "HOST_SECURITY_INVALID" {
		t.Fatalf("package mismatch: %+v", mismatch)
	}
	hostsecurity.PrivilegeProbe = func() int { return 1000 }
	privilege := RemoteHostSecurityCommand(hostSecurityRequest("a2", phelixgrpc.HostSecurityCommandUpdateApply,
		&pb.HostSecurityCommandOptions{Packages: []string{"ssl"}, PreviewToken: token, ProtocolVersion: 1}))
	if privilege.GetErrorCode() != "HOST_SECURITY_PRIVILEGE" {
		t.Fatalf("privilege gate: %+v", privilege)
	}
	hostsecurity.PrivilegeProbe = func() int { return 0 }

	apply := RemoteHostSecurityCommand(hostSecurityRequest("a3", phelixgrpc.HostSecurityCommandUpdateApply,
		&pb.HostSecurityCommandOptions{Packages: []string{"ssl"}, PreviewToken: token, ProtocolVersion: 1}))
	if apply.GetStatus() != "success" {
		t.Fatalf("apply: %+v", apply)
	}
	applyWire := apply.GetHostSecurityUpdate()
	if !strings.HasPrefix(applyWire.GetOperationId(), "hso_") || applyWire.GetStatus() != "succeeded" || applyWire.GetManager() != "apt" {
		t.Fatalf("apply wire: %+v", applyWire)
	}
	operationID := applyWire.GetOperationId()

	verify := RemoteHostSecurityCommand(hostSecurityRequest("v1", phelixgrpc.HostSecurityCommandUpdateVerify,
		&pb.HostSecurityCommandOptions{OperationId: operationID, ProtocolVersion: 1}))
	if verify.GetStatus() != "success" {
		t.Fatalf("verify: %+v", verify)
	}
	if verify.GetHostSecurityScanRecord().GetScanId() == "" || verify.GetHostSecurityScanRecord().GetCoverage() != "complete" {
		t.Fatalf("verify scan record: %+v", verify.GetHostSecurityScanRecord())
	}
	verifyWire := verify.GetHostSecurityUpdate()
	if verifyWire.GetStatus() != "verified" || len(verifyWire.GetVerification()) != 1 {
		t.Fatalf("verify wire: %+v", verifyWire)
	}
	entry := verifyWire.GetVerification()[0]
	if entry.GetPackage() != "ssl" || !entry.GetResolved() || entry.GetDetail() == "" {
		t.Fatalf("verification entry: %+v", entry)
	}
	unknown := RemoteHostSecurityCommand(hostSecurityRequest("v2", phelixgrpc.HostSecurityCommandUpdateVerify,
		&pb.HostSecurityCommandOptions{OperationId: "hso_missing", ProtocolVersion: 1}))
	if unknown.GetErrorCode() != "HOST_SECURITY_NOT_FOUND" {
		t.Fatalf("unknown operation: %+v", unknown)
	}

	reboot := RemoteHostSecurityCommand(hostSecurityRequest("rb", phelixgrpc.HostSecurityCommandRebootStatus, nil))
	if reboot.GetStatus() != "success" || reboot.GetHostSecurityReboot().GetRequired() {
		t.Fatalf("reboot status: %+v", reboot)
	}
}

// TestRemoteHostSecurityUpdateVerifyUnresolved pins the wire verification
// status: when the verification scan still finds advisories for a requested
// package, the wire status stays "succeeded" (the apply completed) instead
// of fabricating "verified", and the entries carry the remaining findings.
func TestRemoteHostSecurityUpdateVerifyUnresolved(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".phelix"), 0o755); err != nil {
		t.Fatal(err)
	}
	hostSecuritySetConsent(t, true)
	server := remediationAdvisoryServer(t, "3.0-1")
	_, system := remediationFixtureSystem(t)
	prevSystem := newServerHostSystem
	newServerHostSystem = func() hostsecurity.System { return system }
	t.Cleanup(func() { newServerHostSystem = prevSystem })
	prevIntel := serverIntelligence
	serverIntelligence = func(dataDir string) *hostsecurity.Intelligence {
		return &hostsecurity.Intelligence{
			CacheDir: filepath.Join(securityTestDataDir(), "host-security", "intel-cache"),
			HTTP:     &http.Client{Transport: serverRedirectTransport{server}},
		}
	}
	t.Cleanup(func() { serverIntelligence = prevIntel })
	prevProbe := hostsecurity.PrivilegeProbe
	hostsecurity.PrivilegeProbe = func() int { return 0 }
	t.Cleanup(func() { hostsecurity.PrivilegeProbe = prevProbe })

	preview := RemoteHostSecurityCommand(hostSecurityRequest("up1", phelixgrpc.HostSecurityCommandUpdatePreview,
		&pb.HostSecurityCommandOptions{Packages: []string{"ssl"}, ProtocolVersion: 1}))
	if preview.GetStatus() != "success" {
		t.Fatalf("preview: %+v", preview)
	}
	apply := RemoteHostSecurityCommand(hostSecurityRequest("ua1", phelixgrpc.HostSecurityCommandUpdateApply,
		&pb.HostSecurityCommandOptions{Packages: []string{"ssl"}, PreviewToken: preview.GetHostSecurityPreview().GetPreviewToken(), ProtocolVersion: 1}))
	if apply.GetStatus() != "success" {
		t.Fatalf("apply: %+v", apply)
	}
	verify := RemoteHostSecurityCommand(hostSecurityRequest("uv1", phelixgrpc.HostSecurityCommandUpdateVerify,
		&pb.HostSecurityCommandOptions{OperationId: apply.GetHostSecurityUpdate().GetOperationId(), ProtocolVersion: 1}))
	if verify.GetStatus() != "success" {
		t.Fatalf("verify: %+v", verify)
	}
	verifyWire := verify.GetHostSecurityUpdate()
	if verifyWire.GetStatus() != "succeeded" {
		t.Fatalf("an unresolved entry must keep the wire status at succeeded: %+v", verifyWire)
	}
	if len(verifyWire.GetVerification()) != 1 || verifyWire.GetVerification()[0].GetResolved() ||
		!strings.Contains(verifyWire.GetVerification()[0].GetDetail(), "CVE-2024-10001") {
		t.Fatalf("the remaining findings must travel in the entries: %+v", verifyWire.GetVerification())
	}
}

// TestRemoteHostSecurityUpdateUnsupported pins the unsupported-host path for
// the update commands: a host without a supported package manager answers
// HOST_SECURITY_UNSUPPORTED instead of executing anything.
func TestRemoteHostSecurityUpdateUnsupported(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".phelix"), 0o755); err != nil {
		t.Fatal(err)
	}
	hostSecuritySetConsent(t, true)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/os-release"), []byte("ID=custom\nVERSION_ID=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prevSystem := newServerHostSystem
	newServerHostSystem = func() hostsecurity.System {
		return hostsecurity.System{Root: root, Lookup: func(string) (string, error) { return "", nil }}
	}
	t.Cleanup(func() { newServerHostSystem = prevSystem })
	got := RemoteHostSecurityCommand(hostSecurityRequest("u1", phelixgrpc.HostSecurityCommandUpdatePreview,
		&pb.HostSecurityCommandOptions{Packages: []string{"ssl"}, ProtocolVersion: 1}))
	if got.GetErrorCode() != "HOST_SECURITY_UNSUPPORTED" {
		t.Fatalf("unsupported host preview: %+v", got)
	}
}

// TestHostSecurityCappedRecordTruncatesFindings pins the wire cap: a scan
// beyond the cap keeps its first findings in order, marks the record
// truncated, and reports the true total; a scan under the cap stays intact.
func TestHostSecurityCappedRecordTruncatesFindings(t *testing.T) {
	scan := hostsecurity.Scan{ID: "s1", ServerID: "srv", Status: "completed", Coverage: "complete", Warnings: []string{}, Counts: map[string]int{}, Sources: []hostsecurity.Source{}, Findings: []hostsecurity.Finding{}}
	for i := 0; i < hostSecurityFindingsWireCap+50; i++ {
		scan.Findings = append(scan.Findings, hostsecurity.Finding{ID: fmt.Sprintf("f%03d", i), CVE: fmt.Sprintf("CVE-2026-%04d", i), Package: "ssl", Severity: "high", State: "confirmed"})
	}
	record := hostSecurityCappedRecord(scan)
	if len(record.Findings) != hostSecurityFindingsWireCap {
		t.Fatalf("the wire record must cap findings at %d, got %d", hostSecurityFindingsWireCap, len(record.Findings))
	}
	if !record.FindingsTruncated {
		t.Fatal("the capped record must carry the truncation flag")
	}
	if record.FindingsTotal != int32(len(scan.Findings)) {
		t.Fatalf("the capped record must report the true total %d, got %d", len(scan.Findings), record.FindingsTotal)
	}
	if record.Findings[0].GetId() != scan.Findings[0].ID || record.Findings[hostSecurityFindingsWireCap-1].GetId() != scan.Findings[hostSecurityFindingsWireCap-1].ID {
		t.Fatal("the kept findings must preserve the scan's order")
	}
	small := scan
	small.Findings = scan.Findings[:10]
	uncapped := hostSecurityCappedRecord(small)
	if uncapped.FindingsTruncated || len(uncapped.Findings) != 10 || uncapped.FindingsTotal != 10 {
		t.Fatalf("a scan under the cap must stay intact: %+v", uncapped.FindingsTotal)
	}
}
