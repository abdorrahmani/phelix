package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/monitor"
)

func TestClassifyRollbackDeployStateStrict(t *testing.T) {
	t.Run("missing selects classic", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		state, err := classifyRollbackDeployState("missing")
		if err != nil || state != nil {
			t.Fatalf("state=%+v err=%v, want classic nil state", state, err)
		}
	})

	for _, tc := range []struct {
		name string
		data string
		code phelixerr.Code
	}{
		{name: "corrupt", data: "{", code: phelixerr.CodeConfiguration},
		{name: "empty", data: `{}`, code: phelixerr.CodeConfiguration},
		{name: "unsupported", data: `{"app_name":"shop","mode":"canary"}`, code: phelixerr.CodeRollbackFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			dir := filepath.Join(home, ".phelix", "apps", "shop")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "deploy.json"), []byte(tc.data), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := classifyRollbackDeployState("shop")
			if !phelixerr.IsCode(err, tc.code) {
				t.Fatalf("error=%v, want code %s", err, tc.code)
			}
		})
	}
}

func TestCopyFileForRollbackFailurePreservesDestination(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "app")
	if err := os.WriteFile(dst, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFileForRollback(filepath.Join(dir, "missing"), dst); err == nil {
		t.Fatal("expected source open failure")
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old-binary" {
		t.Fatalf("destination changed on failed copy: %q", got)
	}
}

func TestReplaceBinaryBackupRestore(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "target")
	dst := filepath.Join(dir, "app")
	if err := os.WriteFile(src, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	backup, err := replaceBinaryForRollback(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "new" {
		t.Fatalf("replacement=%q, want new", got)
	}
	if err := backup.Restore(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "old" {
		t.Fatalf("restored=%q, want old", got)
	}
}

func TestRemoteRollbackVerifyBound(t *testing.T) {
	err := RemoteRollback(monitor.CommandPayload{
		AppName:        "shop",
		VerifyDuration: maxRemoteRollbackVerifyDuration + time.Millisecond,
	})
	if !phelixerr.IsCode(err, phelixerr.CodeInvalidArgument) {
		t.Fatalf("error=%v, want INVALID_ARGUMENT", err)
	}
}

func TestBuildRollbackPreviewMapsTargetSize(t *testing.T) {
	const size = int64(1234567)
	preview := buildRollbackPreview("shop", &deploy.RollbackPlan{TargetSize: size})
	if preview.GetTargetSize() != size {
		t.Fatalf("target_size=%d, want %d", preview.GetTargetSize(), size)
	}
}
