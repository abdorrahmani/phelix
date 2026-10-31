package logs

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *LogStore {
	t.Helper()
	home := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", filepath.Join(home, ".phelix"))
	s, err := OpenLogStore()
	if err != nil {
		t.Fatalf("OpenLogStore returned error: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func storeRecord(ts time.Time, app string, level LogLevel, version string) LogRecord {
	return LogRecord{
		TS:      ts.UTC().Format(RFC3339Milli),
		Level:   level,
		Source:  SourceApp,
		App:     app,
		AppID:   "apps_" + app,
		Version: version,
		Msg:     "hello from " + app,
		Fields:  map[string]string{"path": "/x"},
	}
}

func TestLogStore_InsertAndQuery(t *testing.T) {
	s := openTestStore(t)
	base := time.Now().Add(-time.Hour)

	err := s.Insert([]LogRecord{
		storeRecord(base, "orders", LevelError, "v18"),
		storeRecord(base.Add(time.Minute), "orders", LevelInfo, "v19"),
		storeRecord(base.Add(2*time.Minute), "gateway", LevelWarning, "v19"),
	})
	if err != nil {
		t.Fatalf("Insert returned error: %v", err)
	}

	got, err := s.Query("orders", "", "", "", time.Time{}, 0)
	if err != nil {
		t.Fatalf("Query returned error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d records, want 2", len(got))
	}
	if got[0].Version != "v19" {
		t.Errorf("newest-first ordering violated: %v", got[0].Version)
	}
	if got[0].Fields["path"] != "/x" {
		t.Errorf("fields did not round-trip: %v", got[0].Fields)
	}

	got, err = s.Query("orders", LevelError, "", "", time.Time{}, 0)
	if err != nil {
		t.Fatalf("Query returned error: %v", err)
	}
	if len(got) != 1 || got[0].Level != LevelError {
		t.Errorf("level filter: %+v", got)
	}

	got, err = s.Query("", "", "v18", "", time.Time{}, 0)
	if err != nil {
		t.Fatalf("Query returned error: %v", err)
	}
	if len(got) != 1 || got[0].App != "orders" {
		t.Errorf("version filter: %+v", got)
	}

	got, err = s.Query("", "", "", "", base.Add(30*time.Second), 0)
	if err != nil {
		t.Fatalf("Query returned error: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("since filter: got %d records, want 2", len(got))
	}
	got, err = s.Query("", "", "", "", base.Add(90*time.Second), 0)
	if err != nil {
		t.Fatalf("Query returned error: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("late since filter: got %d records, want 1", len(got))
	}
}

func TestLogStore_QueryLevelAliases(t *testing.T) {
	s := openTestStore(t)
	if err := s.Insert([]LogRecord{storeRecord(time.Now(), "orders", LevelError, "v1")}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Query("", "ERROR", "", "", time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("uppercase legacy level token must match canonical records, got %d", len(got))
	}
}

func TestLogStore_Retain(t *testing.T) {
	s := openTestStore(t)
	old := storeRecord(time.Now().Add(-31*24*time.Hour), "orders", LevelInfo, "v1")
	fresh := storeRecord(time.Now(), "orders", LevelInfo, "v2")
	if err := s.Insert([]LogRecord{old, fresh}); err != nil {
		t.Fatal(err)
	}

	n, err := s.Retain()
	if err != nil {
		t.Fatalf("Retain returned error: %v", err)
	}
	if n != 1 {
		t.Fatalf("retention removed %d records, want 1", n)
	}
	got, _ := s.Query("", "", "", "", time.Time{}, 0)
	if len(got) != 1 || got[0].Version != "v2" {
		t.Errorf("fresh record must survive retention: %+v", got)
	}
}

func TestStoreRecords_NoopWhenDisabled(t *testing.T) {
	t.Setenv("PHELIX_LOG_STORE", "")
	home := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", filepath.Join(home, ".phelix"))

	StoreRecords([]LogRecord{storeRecord(time.Now(), "orders", LevelInfo, "v1")})

	if LogStoreEnabled() {
		t.Fatalf("opt-in reported enabled with the variable unset")
	}
	if _, err := os.Stat(filepath.Join(home, ".phelix", "logs", "records.db")); !os.IsNotExist(err) {
		t.Errorf("records.db created while the store was disabled")
	}
}
