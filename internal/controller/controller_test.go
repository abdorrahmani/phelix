package controller

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/abdorrahmani/phelix/internal/app"
)

func TestOwnerReleaseAndDuplicate(t *testing.T) {
	dir := t.TempDir()
	first, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := Acquire(dir); err == nil {
		other.Close()
		t.Fatal("duplicate owner accepted")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	second.Close()
	if _, err := os.Stat(filepath.Join(dir, "controller.lock")); err != nil {
		t.Fatal("lock inode removed", err)
	}
}

func TestOwnershipSurvivesParentReleaseUntilAdapterExits(t *testing.T) {
	if os.Getenv("PHELIX_OWNER_TEST_HELPER") == "1" {
		file := os.NewFile(3, "inherited-owner")
		defer file.Close()
		if _, err := file.Stat(); err != nil {
			t.Fatal(err)
		}
		os.Stdout.Write([]byte{1})
		io.Copy(io.Discard, os.Stdin)
		return
	}
	directory := t.TempDir()
	owner, err := Acquire(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	child := exec.Command(os.Args[0], "-test.run=^TestOwnershipSurvivesParentReleaseUntilAdapterExits$")
	child.Env = append(os.Environ(), "PHELIX_OWNER_TEST_HELPER=1")
	child.ExtraFiles = []*os.File{owner.file}
	input, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Process.Kill()
	if _, err := io.ReadFull(output, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	owner.Close()
	if duplicate, err := Acquire(directory); err == nil {
		duplicate.Close()
		t.Fatal("ownership released while adapter still running")
	}
	input.Close()
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
	restarted, err := Acquire(directory)
	if err != nil {
		t.Fatal("ownership not released after adapter exits", err)
	}
	restarted.Close()
}

func TestMigrationRepeatAndConflict(t *testing.T) {
	old, next := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(old, "master.key"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := MigrateStorage(old, next); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(filepath.Join(next, "master.key"))
	if string(data) != "original" {
		t.Fatal("key changed")
	}
	os.WriteFile(filepath.Join(next, "master.key"), []byte("other"), 0600)
	if err := MigrateStorage(old, next); err == nil {
		t.Fatal("conflict ignored")
	}
	data, _ = os.ReadFile(filepath.Join(old, "master.key"))
	if string(data) != "original" {
		t.Fatal("legacy key changed")
	}
}

func TestSourceMountTraversal(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	c := Config{Mode: "container", Sources: []SourceMount{{Host: "/host/source", Controller: root}}}
	if path, err := c.SourcePath("/host/source"); err != nil || path != root {
		t.Fatal(path, err)
	}
	os.Symlink(outside, filepath.Join(root, "escape"))
	if _, err := c.SourcePath("/host/source/escape"); err == nil {
		t.Fatal("symlink escape allowed")
	}
	if _, err := c.SourcePath("/host/source/../other"); err == nil {
		t.Fatal("traversal allowed")
	}
}

func TestControllerCancelledBeforeOwnership(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New(ctx, Config{}); err == nil {
		t.Fatal("cancelled initialization succeeded")
	}
}

func TestPersistentRegistryNotReset(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", dir)
	original := app.StateDirForTesting()
	t.Cleanup(func() { app.SetStateDirForTesting(original) })
	data := []byte(`{"app_123":{"id":"app_123","name":"sample","status":"stopped"}}`)
	os.WriteFile(filepath.Join(dir, "apps.json"), data, 0600)
	if err := app.SetStateDirForTesting(dir); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		owner, err := Acquire(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := app.Manager.LoadState(); err != nil {
			t.Fatal(err)
		}
		apps := app.Manager.ListApplications()
		if len(apps) != 1 || apps[0].ID != "app_123" {
			t.Fatal("registry reset", apps)
		}
		owner.Close()
	}
}

func TestControllerIdentityRestart(t *testing.T) {
	if os.Getenv("PHELIX_CONTROLLER_TEST_HELPER") == "1" {
		c, err := New(context.Background(), Config{})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(os.Getenv("PHELIX_DATA_DIR"), "observed-id"), []byte(c.Identity), 0600); err != nil {
			t.Fatal(err)
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		return
	}
	dir, home := t.TempDir(), t.TempDir()
	var first string
	for range 2 {
		child := exec.Command(os.Args[0], "-test.run=^TestControllerIdentityRestart$")
		child.Env = append(os.Environ(), "PHELIX_CONTROLLER_TEST_HELPER=1", "PHELIX_DATA_DIR="+dir, "HOME="+home)
		if output, err := child.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, output)
		}
		id, err := os.ReadFile(filepath.Join(dir, "observed-id"))
		if err != nil {
			t.Fatal(err)
		}
		if first != "" && first != string(id) {
			t.Fatal("identity changed on restart")
		}
		first = string(id)
	}
	if first == "" {
		t.Fatal("identity absent")
	}
}
