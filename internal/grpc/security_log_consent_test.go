package grpc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/security"
	"github.com/abdorrahmani/phelix/internal/server"
)

func TestSecuritySelfLogsRequireCurrentReportingConsent(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	stream := &capturingHostSecurityStream{}
	manager := &monitorStreamManager{stream: stream}
	entries := []*pb.MonitorLogEntry{
		{Source: pb.LogSource_LOG_SOURCE_SELF, Component: "grpc", Log: "ordinary monitoring"},
		{Source: pb.LogSource_LOG_SOURCE_SELF, Component: "security", Log: "artifact policy audit"},
		{Source: pb.LogSource_LOG_SOURCE_APPLICATION, Component: "security", Log: "application monitoring"},
	}
	for _, state := range []string{"absent", "enabled", "disabled", "corrupt"} {
		t.Run(state, func(t *testing.T) {
			batcher := newLogBatcher()
			batcher.add(entries)
			if state != "absent" {
				if _, err := security.SetRemoteSecurity(server.DataDir(), state == "enabled"); err != nil {
					t.Fatal(err)
				}
			}
			if state == "corrupt" {
				if err := os.WriteFile(filepath.Join(server.DataDir(), "security", "remote.json"), []byte("invalid"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			stream.events = nil
			for _, entry := range entries {
				if err := manager.send(&pb.MonitorEvent{Payload: &pb.MonitorEvent_LogEntry{LogEntry: entry}}); err != nil {
					t.Fatal(err)
				}
			}
			batcher.drain(manager.send)
			want := 2
			if state == "enabled" {
				want = 3
			}
			if len(stream.events) != want+1 {
				t.Fatalf("sent %d events, want %d", len(stream.events), want+1)
			}
			batch := stream.events[want].GetLogBatch()
			if batch == nil || len(batch.Entries) != want {
				t.Fatalf("batch did not apply current consent: %v", batch)
			}
			for _, event := range stream.events {
				sent := event.GetLogBatch().GetEntries()
				if entry := event.GetLogEntry(); entry != nil {
					sent = []*pb.MonitorLogEntry{entry}
				}
				for _, entry := range sent {
					if state != "enabled" && entry.GetSource() == pb.LogSource_LOG_SOURCE_SELF && entry.GetComponent() == "security" {
						t.Fatal("security self log sent without consent")
					}
				}
			}
		})
	}
}

func TestRedactedSecuritySelfLogRequiresConsent(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	for _, secret := range []string{"security", "ecur"} {
		t.Run(secret, func(t *testing.T) {
			phelixerr.RegisterSecretValue(secret)
			t.Cleanup(func() { phelixerr.UnregisterSecretValue(secret) })
			record := logs.NewRecord(logs.SourceSelf, logs.LevelWarning, "artifact policy audit: dependency denied")
			record.Component = "security"
			raw, err := record.Encode()
			if err != nil {
				t.Fatal(err)
			}
			restored, ok := logs.RecordFromLine(string(raw))
			if !ok {
				t.Fatal("local audit record was not readable")
			}
			entry := toProtoLogEntry(logs.LogEntry{Component: restored.Component, Log: restored.Msg}, pb.LogSource_LOG_SOURCE_SELF)
			if entry.Component == "security" || entry.Log == "" {
				t.Fatal("fixture did not mask the component and retain audit details")
			}
			stream := &capturingHostSecurityStream{}
			manager := &monitorStreamManager{stream: stream}
			for _, event := range []*pb.MonitorEvent{
				{Payload: &pb.MonitorEvent_LogEntry{LogEntry: entry}},
				{Payload: &pb.MonitorEvent_LogBatch{LogBatch: &pb.MonitorLogBatch{Entries: []*pb.MonitorLogEntry{entry}}}},
			} {
				if err := manager.send(event); err != nil {
					t.Fatal(err)
				}
			}
			if len(stream.events) != 0 {
				t.Fatalf("redacted security classification bypassed reporting consent: component=%q source=%v enabled=%v events=%v", entry.Component, entry.Source, supplyChainReadEnabled(), stream.events)
			}
		})
	}
}

func TestQueuedSecurityEventsRecheckConsentUnderStreamLock(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	result := &pb.MonitorCommandResult{
		Command: SecurityArtifactVerify, Status: "error", ErrorCode: "SECURITY_POLICY_REJECTED",
		Error: "private-package has a prohibited license", SupplyChainJson: []byte(`{"report":{"app_id":"app"}}`),
	}
	for _, tc := range []struct {
		name  string
		event *pb.MonitorEvent
	}{
		{"report", &pb.MonitorEvent{Payload: &pb.MonitorEvent_SupplyChainJson{SupplyChainJson: []byte(`{"report":{"app_id":"app"}}`)}}},
		{"transfer", &pb.MonitorEvent{Payload: &pb.MonitorEvent_SupplyChainJson{SupplyChainJson: []byte(`{"transfer":{"payload":"private"}}`)}}},
		{"scan", &pb.MonitorEvent{Payload: &pb.MonitorEvent_SecurityScan{SecurityScan: &pb.SecurityScanSnapshot{}}}},
		{"result", &pb.MonitorEvent{Payload: &pb.MonitorEvent_CommandResult{CommandResult: result}}},
		{"state", &pb.MonitorEvent{Payload: &pb.MonitorEvent_SupplyChainJson{SupplyChainJson: []byte(`{"remote":{"enabled":false,"revision":2}}`)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := security.SetRemoteSecurity(server.DataDir(), true); err != nil {
				t.Fatal(err)
			}
			stream := &capturingHostSecurityStream{}
			manager := &monitorStreamManager{stream: stream}
			manager.mu.Lock()
			started := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				close(started)
				done <- manager.send(tc.event)
			}()
			<-started
			_, err := security.SetRemoteSecurity(server.DataDir(), false)
			manager.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			err = <-done
			if tc.name == "state" {
				if err != nil || len(stream.events) != 1 {
					t.Fatalf("disabled state did not synchronize: %v", err)
				}
			} else if tc.name == "result" {
				if err != nil || len(stream.events) != 1 {
					t.Fatalf("operation outcome was not delivered: %v", err)
				}
				sent := stream.events[0].GetCommandResult()
				if !sent.SecurityResultWithheld || len(sent.SupplyChainJson) != 0 || strings.Contains(sent.Error, "private-package") || sent.Status != result.Status || sent.ErrorCode != result.ErrorCode {
					t.Fatalf("queued operation did not withhold sensitive details: %v", sent)
				}
				if !strings.Contains(result.Error, "private-package") || len(result.SupplyChainJson) == 0 {
					t.Fatal("withholding destroyed the original durable outcome")
				}
			} else if phelixerr.CodeOf(err) != phelixerr.CodeSecurityReportingDisabled || len(stream.events) != 0 {
				t.Fatalf("queued security data sent after disable: events=%d error=%v", len(stream.events), err)
			}
		})
	}
}
