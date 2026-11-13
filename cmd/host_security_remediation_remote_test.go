package cmd

import (
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
