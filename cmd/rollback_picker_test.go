package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/deploy"
)

// seedPickerApp creates a fake HOME with versions.json and binaries for the
// given versions so rollbackCandidates / promptRollbackVersion have data.
// Versions are listed newest-first in versions.json order-independent; use
// the version numbers given.
func seedPickerApp(t *testing.T, versions ...int) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	appDir := filepath.Join(home, ".phelix", "apps", "pickapp")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}

	highest := 0
	for _, v := range versions {
		if v > highest {
			highest = v
		}
	}
	metas := make([]deploy.VersionMeta, 0, len(versions))
	for _, v := range versions {
		metas = append(metas, deploy.VersionMeta{
			Version:   v,
			BuiltAt:   time.Now().Add(-time.Duration(highest-v) * time.Hour),
			SizeBytes: 1024,
			IsCurrent: v == highest,
		})
	}
	data, _ := json.Marshal(deploy.VersionsFile{Versions: metas})
	if err := os.WriteFile(filepath.Join(appDir, "versions.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, v := range versions {
		vdir := filepath.Join(appDir, "builds", fmt.Sprintf("v%d", v))
		if err := os.MkdirAll(vdir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(vdir, "binary"), []byte("bin"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func TestRollbackCandidatesExcludesCurrentAndOrdersNewestFirst(t *testing.T) {
	seedPickerApp(t, 9, 10, 11)
	cands, cur, err := rollbackCandidates("pickapp")
	if err != nil {
		t.Fatal(err)
	}
	if cur != 11 {
		t.Errorf("current = %d, want 11", cur)
	}
	got := []int{}
	for _, c := range cands {
		got = append(got, c.Version)
	}
	// v10 must sort after v9 numerically, not lexicographically.
	want := []int{10, 9}
	if len(got) != len(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidates = %v, want %v (newest-first, numeric order)", got, want)
		}
	}
}

func TestRollbackCandidatesFiltersMissingBinary(t *testing.T) {
	home := seedPickerApp(t, 1, 2, 3)
	// v1's binary artifact vanished — stale versions.json entry.
	if err := os.Remove(filepath.Join(home, ".phelix", "apps", "pickapp", "builds", "v1", "binary")); err != nil {
		t.Fatal(err)
	}
	cands, _, err := rollbackCandidates("pickapp")
	if err != nil {
		t.Fatal(err)
	}
	got := []int{}
	for _, c := range cands {
		got = append(got, c.Version)
	}
	if len(got) != 1 || got[0] != 2 {
		t.Errorf("candidates = %v, want [2] (stale v1 filtered)", got)
	}
}

func TestRollbackCandidatesEmptyWhenOnlyCurrent(t *testing.T) {
	seedPickerApp(t, 1)
	cands, cur, err := rollbackCandidates("pickapp")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Errorf("candidates = %v, want empty", cands)
	}
	if cur != 1 {
		t.Errorf("current = %d, want 1", cur)
	}
}

func TestPromptRollbackVersionEmptyMessage(t *testing.T) {
	seedPickerApp(t, 1)
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	target, picked, err := promptRollbackVersion("pickapp")
	os.Stdout = old
	w.Close()
	if err != nil || picked || target != 0 {
		t.Fatalf("picked=%v target=%d err=%v, want clean no-pick", picked, target, err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"No previous versions are available for rollback", "Current version: v1"} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Errorf("empty-candidates output missing %q\noutput:\n%s", want, out)
		}
	}
}

func TestRollbackPickerLabelFormat(t *testing.T) {
	now := time.Now()
	metas := []deploy.VersionMeta{
		{Version: 11, Tag: "hotfix-auth", BuiltAt: now.Add(-2 * time.Minute)},
		{Version: 10, Tag: "", BuiltAt: now.Add(-3 * 24 * time.Hour)},
	}
	// Label formatting mirrors promptRollbackVersion's option construction.
	labels := make([]string, len(metas))
	for i, v := range metas {
		tag := v.Tag
		if tag == "" {
			tag = "—"
		}
		labels[i] = fmt.Sprintf("v%-4d %-20.20s %s", v.Version, tag, relTime(v.BuiltAt))
	}
	if labels[0] != fmt.Sprintf("v%-4d %-20.20s %s", 11, "hotfix-auth", "2 min ago") {
		t.Errorf("label[0] = %q", labels[0])
	}
	// Long tag truncated, missing tag renders as —.
	if labels[1][:6] != "v10   " {
		t.Errorf("label[1] = %q", labels[1])
	}
}

func TestRelTime(t *testing.T) {
	now := time.Now()
	cases := []struct {
		ago  time.Duration
		want string
	}{
		{30 * time.Second, "just now"},
		{2 * time.Minute, "2 min ago"},
		{3 * time.Hour, "3 hours ago"},
		{1 * time.Hour, "1 hour ago"},
		{3 * 24 * time.Hour, "3 days ago"},
	}
	for _, c := range cases {
		if got := relTime(now.Add(-c.ago)); got != c.want {
			t.Errorf("relTime(-%s) = %q, want %q", c.ago, got, c.want)
		}
	}
	if got := relTime(now.Add(-30 * 24 * time.Hour)); got != now.Add(-30*24*time.Hour).Format("Jan 2, 2006") {
		t.Errorf("relTime(30d) = %q, want date fallback", got)
	}
	if got := relTime(time.Time{}); got != "—" {
		t.Errorf("relTime(zero) = %q, want —", got)
	}
}
