package security

import (
	"os"
	"sync"
	"testing"
)

func TestRemoteSecurityState(t *testing.T) {
	dir := t.TempDir()
	state, err := LoadRemoteState(dir)
	if err != nil || state.Enabled {
		t.Fatalf("default must disable reporting: %+v %v", state, err)
	}
	state, err = SetRemoteSecurity(dir, true)
	if err != nil || !state.Enabled || state.Revision != 1 || state.SyncStatus != "pending" {
		t.Fatalf("enable: %+v %v", state, err)
	}
	if err := AcknowledgeRemoteSecurity(dir, state.Revision, ""); err != nil {
		t.Fatal(err)
	}
	state, err = LoadRemoteState(dir)
	if err != nil || state.SyncStatus != "synchronized" {
		t.Fatalf("restart: %+v %v", state, err)
	}
	state, err = SetRemoteSecurity(dir, false)
	if err != nil || state.Revision != 2 {
		t.Fatalf("disable: %+v %v", state, err)
	}
	if err := AcknowledgeRemoteSecurity(dir, 1, ""); err != nil {
		t.Fatal(err)
	}
	state, err = LoadRemoteState(dir)
	if err != nil || state.Enabled || state.SyncStatus != "pending" {
		t.Fatalf("stale acknowledgment changed state: %+v %v", state, err)
	}
	if err := AcknowledgeRemoteSecurity(dir, 2, "backend unavailable"); err != nil {
		t.Fatal(err)
	}
	state, err = LoadRemoteState(dir)
	if err != nil || state.SyncStatus != "failed" || state.SyncError == "" {
		t.Fatalf("failure: %+v %v", state, err)
	}
	info, err := os.Stat(remoteStatePath(dir))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("permissions: %v %v", info, err)
	}
}

func TestRemoteSecurityCorruptionFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if _, err := SetRemoteSecurity(dir, true); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(remoteStatePath(dir), []byte(`{"enabled":true`), 0600); err != nil {
		t.Fatal(err)
	}
	if RemoteSecurityEnabled(dir) {
		t.Fatal("corrupt activation must disable reporting")
	}
}

func TestConcurrentRemoteStateChanges(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := SetRemoteSecurity(dir, true); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	state, err := LoadRemoteState(dir)
	if err != nil || state.Revision != 1 || !state.Enabled {
		t.Fatalf("concurrent enables should be idempotent: %+v %v", state, err)
	}
}
