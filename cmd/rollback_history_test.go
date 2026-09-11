package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
)

// --- history command --------------------------------------------------------

// seedHistoryApp creates a fake HOME with a minimal app registry entry and a
// rollback_history.jsonl containing the given records.
func seedHistoryApp(t *testing.T, app string, recs ...deploy.RollbackHistoryRecord) string {
	t.Helper()
	return seedHistoryAppHome(t, t.TempDir(), app, recs...)
}

// seedHistoryAppHome seeds one app into an existing fake HOME so multiple
// apps can share the same registry.
func seedHistoryAppHome(t *testing.T, home, app string, recs ...deploy.RollbackHistoryRecord) string {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("PHELIX_DATA_DIR", filepath.Join(home, ".phelix"))
	appDir := filepath.Join(home, ".phelix", "apps", app)
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := make([]string, 0, len(recs))
	for _, r := range recs {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(b))
	}
	content := strings.Join(lines, "\n")
	if len(lines) > 0 {
		content += "\n"
	}
	if err := os.WriteFile(filepath.Join(appDir, "rollback_history.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	// Register the app in the apps.json registry so GetAppInfo finds it.
	// LoadState parses a map keyed by app ID.
	if err := os.WriteFile(filepath.Join(home, ".phelix", "apps.json"), []byte(
		`{"`+app+`":{"id":"`+app+`","name":"`+app+`","status":"stopped","port":8080}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

func runHistoryCmd(t *testing.T, args ...string) string {
	t.Helper()
	var runErr error
	out := captureStdout(t, func() {
		appName := ""
		for i := 0; i < len(args); i++ {
			if args[i] == "--limit" && i+1 < len(args) {
				old := rollbackHistoryLimit
				rollbackHistoryLimit = mustAtoi(t, args[i+1])
				defer func() { rollbackHistoryLimit = old }()
				i++
				continue
			}
			appName = args[i]
		}
		// Construct the record directly: app.Manager binds its stateFile
		// path at package init (before t.Setenv("HOME")), so the seeded
		// temp-HOME registry is invisible to LoadState in-process.
		// runRollbackHistory only reads ID/Name from the record.
		runErr = runRollbackHistory(&app.AppInfo{ID: appName, Name: appName})
	})
	if runErr != nil {
		t.Errorf("rollback history %v: %v", args, runErr)
	}
	return out
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			t.Fatalf("bad limit %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func TestRollbackHistoryTableColumns(t *testing.T) {
	seedHistoryApp(t, "histapp",
		deploy.RollbackHistoryRecord{
			Time: time.Date(2026, 9, 6, 14, 20, 31, 0, time.Local), App: "histapp",
			From: "v12", To: "v7", Status: deploy.RollbackStatusSuccess, Mode: "blue-green",
		},
		deploy.RollbackHistoryRecord{
			Time: time.Date(2026, 9, 2, 9, 13, 12, 0, time.Local), App: "histapp",
			From: "v9", To: "v8", Status: deploy.RollbackStatusFailed, Mode: "rolling",
			Error: "health check failed",
		},
	)
	out := runHistoryCmd(t, "histapp")

	for _, want := range []string{"Rollback History — histapp", "TIME", "FROM", "TO", "STATUS", "MODE"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\noutput:\n%s", want, out)
		}
	}
	for _, want := range []string{"2026-09-06 14:20:31", "v12", "v7", "SUCCESS", "blue-green",
		"2026-09-02 09:13:12", "v9", "v8", "FAILED", "rolling"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\noutput:\n%s", want, out)
		}
	}
}

func TestRollbackHistoryNewestFirstAndLimit(t *testing.T) {
	base := time.Now()
	recs := []deploy.RollbackHistoryRecord{}
	// 30 records, oldest first as the log appends them.
	for i := 0; i < 30; i++ {
		recs = append(recs, deploy.RollbackHistoryRecord{
			Time: base.Add(-time.Duration(30-i) * time.Minute), App: "limapp",
			From: "v31", To: fmt.Sprintf("v%d", 30-i), Status: deploy.RollbackStatusSuccess, Mode: "rolling",
		})
	}
	seedHistoryApp(t, "limapp", recs...)

	out := runHistoryCmd(t, "limapp", "--limit", "5")
	rows := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "v31") {
			rows++
		}
	}
	if rows != 5 {
		t.Errorf("rows shown = %d, want 5 (--limit 5)\noutput:\n%s", rows, out)
	}
	// Newest record carries the most recent timestamp (base - 1min; the loop
	// makes the newest record i=29 → To v1).
	if !strings.Contains(out, base.Add(-time.Minute).Local().Format("2006-01-02 15:04")) {
		t.Errorf("newest record missing from output:\n%s", out)
	}
}

func TestRollbackHistoryAppIsolation(t *testing.T) {
	ts := time.Now().Add(-time.Hour)
	home := seedHistoryApp(t, "app-a",
		deploy.RollbackHistoryRecord{Time: ts, App: "app-a", From: "v3", To: "v2",
			Status: deploy.RollbackStatusSuccess, Mode: "classic"})
	seedHistoryAppHome(t, home, "app-b",
		deploy.RollbackHistoryRecord{Time: ts, App: "app-b", From: "v9", To: "v5",
			Status: deploy.RollbackStatusFailed, Mode: "rolling"})

	out := runHistoryCmd(t, "app-a")
	if strings.Contains(out, "v9") || strings.Contains(out, "app-b") {
		t.Errorf("app-a history leaked app-b records:\n%s", out)
	}
	if !strings.Contains(out, "v3") {
		t.Errorf("app-a record missing:\n%s", out)
	}
}

func TestRollbackHistoryEmpty(t *testing.T) {
	seedHistoryApp(t, "emptyapp")
	out := runHistoryCmd(t, "emptyapp")
	if !strings.Contains(out, "No rollback history found.") {
		t.Errorf("empty history message missing:\n%s", out)
	}
	if strings.Contains(out, "TIME") {
		t.Errorf("empty history must not render a table:\n%s", out)
	}
}

func TestRollbackHistoryMalformedSkippedWithWarning(t *testing.T) {
	home := seedHistoryApp(t, "malapp")
	path := filepath.Join(home, ".phelix", "apps", "malapp", "rollback_history.jsonl")
	good, _ := json.Marshal(deploy.RollbackHistoryRecord{
		Time: time.Now().Add(-time.Hour), App: "malapp", From: "v5", To: "v4",
		Status: deploy.RollbackStatusSuccess, Mode: "classic",
	})
	if err := os.WriteFile(path, []byte("garbage line\n"+string(good)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := runHistoryCmd(t, "malapp")
	if !strings.Contains(out, "malformed") {
		t.Errorf("skipped-record warning missing:\n%s", out)
	}
	if !strings.Contains(out, "v5") {
		t.Errorf("valid record must still render:\n%s", out)
	}
}

func TestRollbackHistoryInvalidLimit(t *testing.T) {
	seedHistoryApp(t, "limval")
	for _, bad := range []string{"0", "-1"} {
		cmd := RollbackCmd
		cmd.SetArgs([]string{"history", "limval", "--limit", bad})
		err := cmd.Execute()
		if err == nil {
			t.Errorf("--limit %s must produce a validation error", bad)
		}
	}
}

// --- picker details ---------------------------------------------------------

func TestPickerVersionDetailsFields(t *testing.T) {
	now := time.Now()
	got := pickerVersionDetails(deploy.VersionMeta{
		Version: 14, Tag: "v3.9", GitCommit: "8f31c2aabbccddeeff", BuiltAt: now.Add(-time.Hour),
		SizeBytes: 15518944, DeployMode: "blue-green",
	})
	want := "v14\n├── Tag: v3.9\n├── Commit: 8f31c2a\n├── Built: 1 hour ago\n├── Binary: 14.8 MB\n├── Health: —\n└── Deploy: blue-green"
	if got != want {
		t.Errorf("details =\n%s\nwant\n%s", got, want)
	}
}

func TestPickerVersionDetailsMissingMetadata(t *testing.T) {
	got := pickerVersionDetails(deploy.VersionMeta{Version: 11})
	for _, want := range []string{"Tag: —", "Commit: —", "Binary: —", "Deploy: —", "Health: —"} {
		if !strings.Contains(got, want) {
			t.Errorf("details missing %q:\n%s", want, got)
		}
	}
}

// TestPickerCandidatesCarryDetailsData: the candidate list used by the picker
// is the same authoritative source as --list, and each candidate carries the
// stored metadata rendered in the details pane.
func TestPickerCandidatesCarryDetailsData(t *testing.T) {
	home := seedPickerApp(t, 9, 10, 11)
	// Enrich v10 with full metadata.
	vdirData := map[string]deploy.VersionMeta{
		"pickapp": {},
	}
	_ = vdirData
	path := filepath.Join(home, ".phelix", "apps", "pickapp", "versions.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var vf deploy.VersionsFile
	if err := json.Unmarshal(data, &vf); err != nil {
		t.Fatal(err)
	}
	for i := range vf.Versions {
		if vf.Versions[i].Version == 10 {
			vf.Versions[i].Tag = "v3.1"
			vf.Versions[i].GitCommit = "abc1234def"
			vf.Versions[i].SizeBytes = 1024 * 1024
			vf.Versions[i].DeployMode = "rolling"
		}
	}
	enriched, _ := json.Marshal(vf)
	if err := os.WriteFile(path, enriched, 0o644); err != nil {
		t.Fatal(err)
	}

	cands, _, err := rollbackCandidates("pickapp")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Fatalf("candidates = %d, want 2", len(cands))
	}
	var v10 *deploy.VersionMeta
	for i := range cands {
		if cands[i].Version == 10 {
			v10 = &cands[i]
		}
	}
	if v10 == nil {
		t.Fatal("v10 missing from candidates")
	}
	details := pickerVersionDetails(*v10)
	for _, want := range []string{"v10", "Tag: v3.1", "Commit: abc1234", "Binary: 1.0 MB", "Deploy: rolling"} {
		if !strings.Contains(details, want) {
			t.Errorf("details missing %q:\n%s", want, details)
		}
	}
}
