package controller

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdapterRejectsForgedOwnershipDescriptor(t *testing.T) {
	if expected := os.Getenv("PHELIX_ADAPTER_OWNERSHIP_TEST"); expected != "" {
		_, _, err := ReadAdapter()
		if err == nil || !strings.Contains(err.Error(), expected) {
			t.Fatal("forged ownership descriptor accepted", err)
		}
		return
	}
	directory := t.TempDir()
	owner, err := Acquire(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	forged, err := os.CreateTemp(directory, "forged-owner-")
	if err != nil {
		t.Fatal(err)
	}
	defer forged.Close()
	independent, err := os.OpenFile(filepath.Join(directory, "controller.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer independent.Close()
	for _, test := range []struct {
		file    *os.File
		message string
	}{{forged, "does not match persistent state"}, {independent, "not the active ownership lease"}} {
		child := exec.Command(os.Args[0], "-test.run=^TestAdapterRejectsForgedOwnershipDescriptor$")
		child.Env = append(os.Environ(), "PHELIX_DATA_DIR="+directory, "PHELIX_ADAPTER_OWNERSHIP_TEST="+test.message)
		child.ExtraFiles = []*os.File{forged, test.file}
		if output, err := child.CombinedOutput(); err != nil {
			t.Fatalf("ownership check failed: %v\n%s", err, output)
		}
	}
}
