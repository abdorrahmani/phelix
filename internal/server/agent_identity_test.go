package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// callLoadInDir points the agent-ID filename at a fresh temp dir, runs
// loadOrGenerateAgentID, and returns (dir, id).
func callLoadInDir(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	originalFile, originalID := agentIDFile, agentID
	t.Cleanup(func() {
		agentIDFile, agentID = originalFile, originalID
	})
	agentIDFile = filepath.Join(dir, "agent-id")
	agentID = ""
	id, err := loadOrGenerateAgentID()
	if err != nil {
		t.Fatalf("loadOrGenerateAgentID: %v", err)
	}
	return dir, id
}

func TestLoadOrGenerateAgentIDPersistsRandomUUID(t *testing.T) {
	tempDir := t.TempDir()
	originalFile, originalID := agentIDFile, agentID
	t.Cleanup(func() {
		agentIDFile, agentID = originalFile, originalID
	})

	agentIDFile = filepath.Join(tempDir, "agent-id")
	agentID = ""
	first, err := loadOrGenerateAgentID()
	if err != nil {
		t.Fatalf("first loadOrGenerateAgentID: %v", err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(first) {
		t.Fatalf("agent ID %q is not a version 4 UUID", first)
	}

	agentID = ""
	second, err := loadOrGenerateAgentID()
	if err != nil {
		t.Fatalf("second loadOrGenerateAgentID: %v", err)
	}
	if second != first {
		t.Fatalf("agent ID changed across restart: got %q, want %q", second, first)
	}

	data, err := os.ReadFile(agentIDFile)
	if err != nil {
		t.Fatalf("read persisted agent ID: %v", err)
	}
	if string(data) != first+"\n" {
		t.Fatalf("persisted agent ID = %q, want %q", string(data), first+"\n")
	}
}

// Two independent Phelix data directories on the same host must mint two
// different agent IDs: this is the core fix for the Docker machine-id
// collision.
func TestLoadOrGenerateAgentIDIndependentDirectories(t *testing.T) {
	dirA, idA := callLoadInDir(t)
	dirB, idB := callLoadInDir(t)

	if dirA == dirB {
		t.Fatalf("test bug: temp dirs must differ")
	}
	if idA == idB {
		t.Fatalf("two independent data dirs produced the same agent ID %q", idA)
	}
}

// The agent ID must never be derived from host data. Here we create two data
// dirs that share identical host-shaped inputs (machine-id file, settings);
// the only thing that differs is the random UUID.
func TestAgentIDNotDerivedFromMachineID(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	originalFile, originalID := agentIDFile, agentID
	t.Cleanup(func() { agentIDFile, agentID = originalFile, originalID })

	machineID := "f5e4c3b2a190481990aabbccddeeff00"
	for _, dir := range []string{dirA, dirB} {
		if err := os.WriteFile(filepath.Join(dir, "machine-id"), []byte(machineID), 0600); err != nil {
			t.Fatal(err)
		}
	}

	agentID = ""
	agentIDFile = filepath.Join(dirA, "agent-id")
	idA, err := loadOrGenerateAgentID()
	if err != nil {
		t.Fatal(err)
	}

	agentID = ""
	agentIDFile = filepath.Join(dirB, "agent-id")
	idB, err := loadOrGenerateAgentID()
	if err != nil {
		t.Fatal(err)
	}

	if idA == idB {
		t.Fatalf("agent IDs derived from shared host state: %q == %q", idA, idB)
	}
}

// Concurrent initialization (e.g. daemon and a CLI command racing on boot)
// must yield exactly one persisted ID.
func TestLoadOrGenerateAgentIDConcurrentSafe(t *testing.T) {
	dir := t.TempDir()
	originalFile, originalID := agentIDFile, agentID
	t.Cleanup(func() { agentIDFile, agentID = originalFile, originalID })
	agentIDFile = filepath.Join(dir, "agent-id")

	var wg sync.WaitGroup
	ids := make([]string, 8)
	errs := make([]error, 8)
	for i := 0; i < len(ids); i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i], errs[i] = loadOrGenerateAgentID()
		}(i)
	}
	wg.Wait()

	id := ids[0]
	if id == "" {
		t.Fatal("no ID produced by concurrent init")
	}
	for i, got := range ids[1:] {
		if errs[i+1] != nil {
			t.Fatalf("concurrent loadOrGenerateAgentID[%d]: %v", i+1, errs[i+1])
		}
		if got != id {
			t.Fatalf("concurrent init produced divergent IDs: got %q, want %q", got, id)
		}
	}
	// and the on-disk value matches
	data, err := os.ReadFile(agentIDFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != id {
		t.Fatalf("on-disk ID %q does not match in-memory %q", strings.TrimSpace(string(data)), id)
	}
}

// A corrupted/partial agent-id from a crashed or interrupted write must not
// be silently trusted. The runtime surfaces a configuration error rather
// than inventing a new identity for an existing runtime.
func TestLoadOrGenerateAgentIDCorruptedFile(t *testing.T) {
	dir := t.TempDir()
	originalFile, originalID := agentIDFile, agentID
	t.Cleanup(func() { agentIDFile, agentID = originalFile, originalID })
	agentIDFile = filepath.Join(dir, "agent-id")
	agentID = ""

	if err := os.WriteFile(agentIDFile, []byte("partial-wr\xff"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := loadOrGenerateAgentID()
	if err == nil {
		t.Fatal("expected an error for a corrupted agent-id file")
	}
	perr := phelixerr.AsError(err)
	if perr == nil {
		t.Fatalf("expected phelix error, got %T: %v", err, err)
	}
	if perr.Code != phelixerr.CodeConfiguration {
		t.Fatalf("expected CodeConfiguration for corrupted agent-id, got %q", perr.Code)
	}
}

// An empty agent-id file must be treated as corruption, not as a blank
// identity.
func TestLoadOrGenerateAgentIDEmptyFile(t *testing.T) {
	dir := t.TempDir()
	originalFile, originalID := agentIDFile, agentID
	t.Cleanup(func() { agentIDFile, agentID = originalFile, originalID })
	agentIDFile = filepath.Join(dir, "agent-id")
	agentID = ""

	if err := os.WriteFile(agentIDFile, []byte("  \n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, loadErr := loadOrGenerateAgentID()
	if loadErr == nil {
		t.Fatal("expected an error for a blank agent-id file")
	}
	perr := phelixerr.AsError(loadErr)
	if perr == nil || perr.Code != phelixerr.CodeConfiguration {
		t.Fatalf("expected CodeConfiguration error, got %v", loadErr)
	}
}

// A surviving .tmp file from a crash must not affect the final ID, and a
// normal new ID must still be written.
func TestLoadOrGenerateAgentIDIgnoresStaleTemp(t *testing.T) {
	dir := t.TempDir()
	originalFile, originalID := agentIDFile, agentID
	t.Cleanup(func() { agentIDFile, agentID = originalFile, originalID })
	agentIDFile = filepath.Join(dir, "agent-id")
	agentID = ""

	if err := os.WriteFile(agentIDFile+".tmp", []byte("stale-tmp\n"), 0600); err != nil {
		t.Fatal(err)
	}
	id, err := loadOrGenerateAgentID()
	if err != nil {
		t.Fatalf("loadOrGenerateAgentID with stale tmp: %v", err)
	}
	data, err := os.ReadFile(agentIDFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != id {
		t.Fatalf("final agent-id %q does not match %q", strings.TrimSpace(string(data)), id)
	}
}
