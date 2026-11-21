package grpc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/security"
	"github.com/abdorrahmani/phelix/internal/server"
)

func TestSupplyChainCommandValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		opts    *pb.SecurityCommandOptions
		valid   bool
	}{
		{"status", SecurityRemoteStatus, nil, true},
		{"detail", SecurityArtifactDetail, &pb.SecurityCommandOptions{Version: 3, ArtifactDigest: strings.Repeat("a", 64)}, true},
		{"matrix detail", SecurityArtifactDetail, &pb.SecurityCommandOptions{Version: 3, ArtifactDigest: strings.Repeat("a", 64), BuildId: "go1.27-linux-amd64"}, true},
		{"matrix traversal", SecurityArtifactDetail, &pb.SecurityCommandOptions{Version: 3, BuildId: "../artifact"}, false},
		{"matrix without version", SecurityArtifacts, &pb.SecurityCommandOptions{BuildId: "go1.27-linux-amd64"}, false},
		{"missing version", SecurityArtifactDetail, nil, false},
		{"wrong digest", SecurityArtifactDetail, &pb.SecurityCommandOptions{Version: 3, ArtifactDigest: "../file"}, false},
		{"foreign scan fields", SecurityAppStatus, &pb.SecurityCommandOptions{SetEnabled: true}, false},
		{"negative offset", SecurityArtifacts, &pb.SecurityCommandOptions{Offset: -1}, false},
		{"format", SecuritySBOM, &pb.SecurityCommandOptions{Version: 3, Format: "spdx"}, true},
		{"unknown format", SecuritySBOM, &pb.SecurityCommandOptions{Version: 3, Format: "text"}, false},
		{"cancel", SecurityOperationCancel, &pb.SecurityCommandOptions{ScanId: "operation123"}, true},
		{"cancel traversal", SecurityOperationCancel, &pb.SecurityCommandOptions{ScanId: "../operation"}, false},
		{"policy unknown", SecurityPolicySet, &pb.SecurityCommandOptions{PolicyJson: `{"mode":"audit","unknown":true}`}, false},
		{"policy trailing object", SecurityPolicySet, &pb.SecurityCommandOptions{PolicyJson: `{"mode":"audit"} {"mode":"disabled"}`}, false},
		{"policy trailing garbage", SecurityPolicySet, &pb.SecurityCommandOptions{PolicyJson: `{"mode":"audit"} invalid`}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &pb.MonitorCommandRequest{RequestId: "request", Type: tc.command, AppName: "app", Security: tc.opts}
			err := validateSecurityCommand(req)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
}

func TestSupplyChainQueueStopsWithClient(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", dir)
	done := make(chan struct{})
	close(done)
	client := &Client{done: done}
	client.queueSupplyChainSnapshots()
	if !client.supplyChainMu.TryLock() {
		t.Fatal("closed client queued a synchronization worker")
	}
	defer client.supplyChainMu.Unlock()
	if _, err := os.Stat(filepath.Join(dir, "security", "remote.json")); !os.IsNotExist(err) {
		t.Fatalf("closed client initialized reporting state: %v", err)
	}
}

func TestSupplyChainSmallUploadRequiresCurrentConsent(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	previous := monitorStream.stream
	stream := &capturingHostSecurityStream{}
	monitorStream.stream = stream
	t.Cleanup(func() { monitorStream.stream = previous })
	client := &Client{}
	for _, state := range []string{"absent", "enabled", "disabled"} {
		t.Run(state, func(t *testing.T) {
			if state != "absent" {
				if _, err := security.SetRemoteSecurity(server.DataDir(), state == "enabled"); err != nil {
					t.Fatal(err)
				}
			}
			stream.events = nil
			raw := []byte(`{"report":{"app_id":"app"}}`)
			err := client.sendSupplyChainJSON(raw, "app")
			if state == "enabled" {
				if err != nil || len(stream.events) != 1 || string(stream.events[0].GetSupplyChainJson()) != string(raw) {
					t.Fatalf("consented report was not delivered: %v", err)
				}
			} else if phelixerr.CodeOf(err) != phelixerr.CodeSecurityReportingDisabled || len(stream.events) != 0 {
				t.Fatalf("report sent without current consent: events=%d error=%v", len(stream.events), err)
			}
		})
	}
}
