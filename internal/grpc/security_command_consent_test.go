package grpc

import (
	"os"
	"path/filepath"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/security"
	"github.com/abdorrahmani/phelix/internal/server"
	"google.golang.org/protobuf/proto"
)

func TestSendSecurityResultRevokedConsentPreservesOutcome(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	previous := monitorStream.stream
	stream := &capturingHostSecurityStream{}
	monitorStream.stream = stream
	t.Cleanup(func() { monitorStream.stream = previous })

	for _, tc := range []struct {
		name    string
		command string
		legacy  bool
		failed  bool
		empty   bool
	}{
		{"artifact failure", SecurityArtifactVerify, false, true, false},
		{"artifact success", SecurityArtifactVerify, false, false, false},
		{"scan failure", SecurityCommandRun, true, true, false},
		{"scan success", SecurityCommandRun, true, false, false},
		{"artifact failure without payload", SecurityArtifactVerify, false, true, true},
		{"scan failure without payload", SecurityCommandRun, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := security.SetRemoteSecurity(server.DataDir(), true); err != nil {
				t.Fatal(err)
			}
			result := &pb.MonitorCommandResult{RequestId: "durable-result", AppName: "app", Command: tc.command, Status: "success"}
			if tc.legacy {
				result.SecurityScanRecord = &pb.SecurityScanRecord{ScanId: "private-scan"}
			} else {
				result.SupplyChainJson = []byte(`{"license":"GPL-3.0-only","package":"private-package"}`)
			}
			if tc.empty {
				result.SecurityScanRecord = nil
				result.SupplyChainJson = nil
			}
			if tc.failed {
				result.Status = "error"
				result.ErrorCode = "SECURITY_VERIFICATION_FAILED"
				result.Error = "security policy rejected private-package: GPL-3.0-only and CVE-2026-1234"
			}
			original := proto.Clone(result).(*pb.MonitorCommandResult)
			if err := sendSecurityResult(result); err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(stream.events[len(stream.events)-1].GetCommandResult(), original) {
				t.Fatal("consented result changed before transmission")
			}
			if _, err := security.SetRemoteSecurity(server.DataDir(), false); err != nil {
				t.Fatal(err)
			}
			if err := sendSecurityResult(result); err != nil {
				t.Fatal(err)
			}
			got := stream.events[len(stream.events)-1].GetCommandResult()
			if len(got.GetSupplyChainJson()) != 0 || got.GetSecurityScanRecord() != nil || !got.GetSecurityResultWithheld() {
				t.Fatalf("security payload was not withheld: %v", got)
			}
			if got.GetSecurityConsentErrorCode() != string(phelixerr.CodeSecurityReportingDisabled) || got.GetStatus() != original.GetStatus() || got.GetErrorCode() != original.GetErrorCode() || got.GetRequestId() != original.GetRequestId() {
				t.Fatalf("durable outcome or consent marker changed: %v", got)
			}
			if tc.failed && got.GetError() != "security operation failed; details are withheld because remote security reporting is disabled" {
				t.Fatalf("sensitive error was not withheld: %q", got.GetError())
			}
			if !tc.failed && got.GetError() != "" {
				t.Fatalf("successful result acquired an error: %q", got.GetError())
			}
			if !proto.Equal(result, original) {
				t.Fatal("withholding mutated the durable original")
			}
		})
	}
}

func TestWithholdSecurityResultPreservesControlAndMonitoring(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	for _, command := range []string{SecurityRemoteStatus, SecurityRemoteEnable, SecurityRemoteDisable, SecurityAppStatus, SecurityAppLogs, SecurityAppBuild, SecurityCommandConfig} {
		result := &pb.MonitorCommandResult{Command: command, Status: "success", SupplyChainJson: []byte(`{"remote":{"enabled":false}}`)}
		if command == SecurityCommandConfig {
			result.SupplyChainJson = nil
			result.SecurityConfig = &pb.SecurityConfigState{}
		}
		if got := withholdSecurityResult(result); !proto.Equal(got, result) || got.GetSecurityResultWithheld() {
			t.Fatalf("control or monitoring result %s was withheld: %v", command, got)
		}
	}
}

type revokingSecuritySnapshotStream struct {
	capturingHostSecurityStream
	afterSend func()
}

func (s *revokingSecuritySnapshotStream) Send(event *pb.MonitorEvent) error {
	if err := s.capturingHostSecurityStream.Send(event); err != nil {
		return err
	}
	if s.afterSend != nil {
		s.afterSend()
	}
	return nil
}

func TestSendSecuritySnapshotsRechecksConsentForEachEvent(t *testing.T) {
	for _, state := range []string{"absent", "malformed", "disabled", "enabled", "revoked"} {
		t.Run(state, func(t *testing.T) {
			t.Setenv("PHELIX_DATA_DIR", t.TempDir())
			if state != "absent" {
				if _, err := security.SetRemoteSecurity(server.DataDir(), state != "disabled"); err != nil {
					t.Fatal(err)
				}
			}
			if state == "malformed" {
				if err := os.WriteFile(filepath.Join(server.DataDir(), "security", "remote.json"), []byte(`{"enabled":true`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			previousProvider := SecuritySnapshotProvider
			SecuritySnapshotProvider = func() []*pb.SecurityScanSnapshot {
				return []*pb.SecurityScanSnapshot{{AppName: "first"}, {AppName: "second"}}
			}
			t.Cleanup(func() { SecuritySnapshotProvider = previousProvider })
			previous := monitorStream.stream
			stream := &revokingSecuritySnapshotStream{}
			monitorStream.stream = stream
			t.Cleanup(func() { monitorStream.stream = previous })
			if state == "revoked" {
				stream.afterSend = func() {
					if _, err := security.SetRemoteSecurity(server.DataDir(), false); err != nil {
						t.Fatal(err)
					}
				}
			}
			(&Client{}).sendSecuritySnapshots()
			want := 0
			if state == "enabled" {
				want = 2
			} else if state == "revoked" {
				want = 1
			}
			if len(stream.events) != want {
				t.Fatalf("state %s sent %d snapshots, want %d", state, len(stream.events), want)
			}
		})
	}
}
