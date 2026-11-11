package job

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/processex"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	return NewStore(t.TempDir())
}

func testRecord(app, id string, status Status, started time.Time) *Record {
	return &Record{
		ID:        id,
		AppName:   app,
		Trigger:   TriggerScheduled,
		Status:    status,
		Command:   []string{"/bin/true"},
		StartedAt: started,
	}
}

func TestStoreSaveLoadAndList(t *testing.T) {
	store := testStore(t)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	first := testRecord("api", NewID(), StatusSucceeded, base)
	second := testRecord("api", NewID(), StatusFailed, base.Add(time.Minute))
	for _, record := range []*Record{first, second} {
		if err := store.Save(record); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}
	got, err := store.Load("api", first.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ID != first.ID || got.Status != first.Status {
		t.Errorf("loaded = %+v", got)
	}
	list, err := store.List("api", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].ID != second.ID || list[1].ID != first.ID {
		t.Fatalf("list order = %+v", list)
	}
	limited, err := store.List("api", 1)
	if err != nil || len(limited) != 1 || limited[0].ID != second.ID {
		t.Fatalf("limited = %+v, %v", limited, err)
	}
}

func TestStoreListSkipsCorruptRecords(t *testing.T) {
	store := testStore(t)
	valid := testRecord("api", NewID(), StatusSucceeded, time.Now())
	if err := store.Save(valid); err != nil {
		t.Fatalf("Save: %v", err)
	}
	dir, err := store.executionsDir("api")
	if err != nil {
		t.Fatalf("executionsDir: %v", err)
	}
	corruptID := NewID()
	if err := os.WriteFile(filepath.Join(dir, corruptID+".json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}
	list, err := store.List("api", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ID != valid.ID {
		t.Fatalf("list = %+v", list)
	}
	if _, err := store.Load("api", corruptID); err == nil {
		t.Fatal("Load must report the corrupt record")
	}
}

func TestStoreRetainKeepsRunningAndNewest(t *testing.T) {
	store := testStore(t)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		record := testRecord("api", NewID(), StatusSucceeded, base.Add(time.Duration(i)*time.Minute))
		record.FinishedAt = &record.StartedAt
		if err := store.Save(record); err != nil {
			t.Fatalf("Save: %v", err)
		}
		logPath, _ := store.LogPath("api", record.ID)
		if err := os.WriteFile(logPath, []byte("x"), 0o600); err != nil {
			t.Fatalf("write log: %v", err)
		}
	}
	running := testRecord("api", NewID(), StatusRunning, base.Add(-time.Hour))
	if err := store.Save(running); err != nil {
		t.Fatalf("Save running: %v", err)
	}
	if err := store.Retain("api", 2); err != nil {
		t.Fatalf("Retain: %v", err)
	}
	list, err := store.List("api", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("retained %d records, want 2 finished plus the running one", len(list))
	}
	foundRunning := false
	for _, record := range list {
		if record.ID == running.ID {
			foundRunning = true
		}
		if record.ID != running.ID {
			logPath, _ := store.LogPath("api", record.ID)
			if _, err := os.Stat(logPath); err != nil {
				t.Errorf("log for retained record %s missing: %v", record.ID, err)
			}
		}
	}
	if !foundRunning {
		t.Error("Retain deleted a running record")
	}
}

func TestStoreHasScheduled(t *testing.T) {
	store := testStore(t)
	when := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	record := testRecord("api", NewID(), StatusSkipped, when)
	record.ScheduledFor = &when
	if err := store.Save(record); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !store.HasScheduled("api", when) {
		t.Error("HasScheduled = false for a recorded occurrence")
	}
	if store.HasScheduled("api", when.Add(time.Minute)) {
		t.Error("HasScheduled = true for an unrecorded occurrence")
	}
}

func TestStoreRecoverInterrupted(t *testing.T) {
	store := testStore(t)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	dead := testRecord("api", NewID(), StatusRunning, base)
	dead.PID = 999999
	if err := store.Save(dead); err != nil {
		t.Fatalf("Save dead: %v", err)
	}
	live := testRecord("api", NewID(), StatusRunning, base)
	identity, err := processex.Identity(os.Getpid())
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	live.PID = identity.PID
	live.ProcessStart = identity.StartTime
	live.Executable = identity.ExePath
	if err := store.Save(live); err != nil {
		t.Fatalf("Save live: %v", err)
	}
	recovered, err := store.RecoverInterrupted()
	if err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("recovered = %d, want 1", recovered)
	}
	got, err := store.Load("api", dead.ID)
	if err != nil {
		t.Fatalf("Load dead: %v", err)
	}
	if got.Status != StatusInterrupted || got.FinishedAt == nil {
		t.Errorf("dead record = %+v", got)
	}
	gotLive, err := store.Load("api", live.ID)
	if err != nil {
		t.Fatalf("Load live: %v", err)
	}
	if gotLive.Status != StatusRunning {
		t.Errorf("live record status = %q", gotLive.Status)
	}
}

func TestStoreRejectsUnsafeIDs(t *testing.T) {
	store := testStore(t)
	if _, err := store.Load("api", "../../etc/passwd"); err == nil {
		t.Fatal("Load must reject an unsafe id")
	}
	if _, err := store.LogPath("api", "not-an-id"); err == nil {
		t.Fatal("LogPath must reject an unsafe id")
	}
	if _, err := store.List("../api", 0); err == nil {
		t.Fatal("List must reject an unsafe app name")
	}
}

func TestStoreLogIsAppendOnly(t *testing.T) {
	store := testStore(t)
	id := NewID()
	f, err := store.OpenLog("api", id)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	if _, err := f.WriteString("first\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()
	f, err = store.OpenLog("api", id)
	if err != nil {
		t.Fatalf("OpenLog again: %v", err)
	}
	if _, err := f.WriteString("second\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()
	path, _ := store.LogPath("api", id)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(raw), "first") || !strings.Contains(string(raw), "second") {
		t.Errorf("log = %q", raw)
	}
}
