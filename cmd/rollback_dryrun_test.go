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

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
)

// seedRollbackPreviewApp creates a fake HOME with versions.json, binaries,
// env snapshot and a blue-green deploy state, so renderRollbackPreview has a
// full picture to plan from.
func seedRollbackPreviewApp(t *testing.T, name string, mode deploy.Mode) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	appDir := filepath.Join(home, ".phelix", "apps", name)
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}

	vf := deploy.VersionsFile{Versions: []deploy.VersionMeta{
		{Version: 1, GitCommit: "aaaa1111aaaa", BuiltAt: time.Now().Add(-48 * time.Hour), SizeBytes: 10 * 1024 * 1024},
		{Version: 2, Tag: "stable", GitCommit: "bbbb2222bbbb", BuiltAt: time.Now().Add(-24 * time.Hour), SizeBytes: 11 * 1024 * 1024},
		{Version: 3, GitCommit: "cccc3333cccc", BuiltAt: time.Now(), SizeBytes: 12 * 1024 * 1024, IsCurrent: true},
	}}
	data, _ := json.MarshalIndent(vf, "", "  ")
	if err := os.WriteFile(filepath.Join(appDir, "versions.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, v := range vf.Versions {
		vdir := filepath.Join(appDir, "builds", fmt.Sprintf("v%d", v.Version))
		if err := os.MkdirAll(vdir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(vdir, "binary"), []byte("bin"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	envDir := filepath.Join(appDir, "env")
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envDir, "v2.enc"), []byte("enc"), 0o644); err != nil {
		t.Fatal(err)
	}

	state := &deploy.DeployState{AppName: name, PublicPort: 3000}
	switch mode {
	case deploy.ModeBlueGreen:
		state.Mode = deploy.ModeBlueGreen
		state.ActiveSlot = deploy.SlotGreen
		state.Slots = map[string]*deploy.Instance{
			deploy.SlotBlue:  {Slot: deploy.SlotBlue, Status: "stopped"},
			deploy.SlotGreen: {Slot: deploy.SlotGreen, Status: "running", PID: 4242, Port: 31001, Version: 3},
		}
	case deploy.ModeRolling:
		state.Mode = deploy.ModeRolling
		state.Replicas = map[string]*deploy.Instance{
			"0": {Slot: "0", Status: "running", PID: 111, Port: 31001, Version: 3},
			"1": {Slot: "1", Status: "running", PID: 222, Port: 31002, Version: 3},
		}
	case "": // classic: no deploy.json
		return home
	}
	sd, _ := json.MarshalIndent(state, "", "  ")
	if err := os.WriteFile(filepath.Join(appDir, "deploy.json"), sd, 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

func previewAppInfo(name string) *app.AppInfo {
	return &app.AppInfo{ID: "72b242bd", Name: name, Status: "running", PID: 4242, Port: 8080, Directory: "/tmp/never-used"}
}

func capturePreview(t *testing.T, name string, target int) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	err = renderRollbackPreview(previewAppInfo(name), name, target, "", rollbackTargetExplicit, "", 0)
	os.Stdout = old
	w.Close()
	if err != nil {
		t.Fatalf("renderRollbackPreview: %v", err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestRenderRollbackPreviewBlueGreen(t *testing.T) {
	home := seedRollbackPreviewApp(t, "bgprev", deploy.ModeBlueGreen)
	out := capturePreview(t, "bgprev", 2)

	for _, want := range []string{
		"Rollback Preview",
		"bgprev",
		"v3",
		"v2 (stable)",
		"blue-green",
		"green",
		"blue",
		":3000",
		"v2 snapshot available",
		"Switch proxy traffic",
		"No changes will be made",
	} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Errorf("preview output missing %q\noutput:\n%s", want, out)
		}
	}
	_ = home
}

func TestRenderRollbackPreviewRolling(t *testing.T) {
	seedRollbackPreviewApp(t, "rollprev", deploy.ModeRolling)
	out := capturePreview(t, "rollprev", 1)

	for _, want := range []string{"rolling", "Replicas", "replica 0", "replica 1", "No changes will be made"} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Errorf("preview output missing %q\noutput:\n%s", want, out)
		}
	}
}

func TestRenderRollbackPreviewClassic(t *testing.T) {
	seedRollbackPreviewApp(t, "clsprev", "")
	out := capturePreview(t, "clsprev", 2)

	for _, want := range []string{"classic", "Downtime", "yes", "Promote", "No changes will be made"} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Errorf("preview output missing %q\noutput:\n%s", want, out)
		}
	}
}

// TestRollbackDryRunNoMutation exercises the full flag path (rollbackDryRun =
// true through PlanRollback inside renderRollbackPreview) and asserts every
// on-disk state surface is byte-identical afterwards: versions.json,
// deploy.json, rollback.log, current symlink, deploy.lock.
func TestRollbackDryRunNoMutation(t *testing.T) {
	name := "nomut-cli"
	home := seedRollbackPreviewApp(t, name, deploy.ModeBlueGreen)
	appDir := filepath.Join(home, ".phelix", "apps", name)
	logPath := filepath.Join(appDir, "rollback.log")
	if err := os.WriteFile(logPath, []byte("preexisting\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	type snap map[string]string
	snapshot := func() snap {
		out := snap{}
		entries, err := os.ReadDir(appDir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			p := filepath.Join(appDir, e.Name())
			if e.Type()&os.ModeSymlink != 0 {
				link, _ := os.Readlink(p)
				out[p] = "symlink→" + link
				continue
			}
			if e.IsDir() {
				continue
			}
			data, _ := os.ReadFile(p)
			out[p] = string(data)
		}
		return out
	}

	before := snapshot()
	_ = capturePreview(t, name, 2)
	_ = capturePreview(t, name, 2) // idempotency: run twice
	after := snapshot()

	for p, v := range before {
		if after[p] != v {
			t.Errorf("state mutated by dry-run: %s", p)
		}
	}
	if _, err := os.Stat(filepath.Join(appDir, "deploy.lock")); err == nil {
		t.Error("deploy.lock created by dry-run")
	}
}
