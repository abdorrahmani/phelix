package logs

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func rotationPadding(t *testing.T, path string) {
	t.Helper()
	var sb bytes.Buffer
	for sb.Len() < 1024*1024+1024 {
		sb.WriteString("padding line ::::::::::::::::::::::::::::::::::::::::::::::::\n")
	}
	if err := os.WriteFile(path, sb.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRotateLogFile_RenamesActive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	rotationPadding(t, path)

	rotated, err := RotateLogFile(path, 1, 3, true)
	if err != nil {
		t.Fatalf("RotateLogFile returned error: %v", err)
	}
	if !rotated {
		t.Fatalf("oversized file was not rotated")
	}

	info, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatalf("rotated file missing: %v", err)
	}
	if info.Size() <= 1024*1024 {
		t.Errorf("rotated file truncated: %d bytes", info.Size())
	}
	fresh, err := os.Stat(path)
	if err != nil {
		t.Fatalf("active file missing after rotation: %v", err)
	}
	if fresh.Size() != 0 {
		t.Errorf("fresh active file holds %d bytes, want 0", fresh.Size())
	}
}

func TestRotateLogFile_SmallFileUntouched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("tiny\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rotated, err := RotateLogFile(path, 1, 3, true)
	if err != nil || rotated {
		t.Fatalf("rotated=%v err=%v, want false/nil", rotated, err)
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Errorf("rotation archive created for a small file")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "tiny\n" {
		t.Errorf("small file was modified: %q err=%v", data, err)
	}
}

func TestRotateLogFile_ChainAndRetention(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	for i := 0; i < 3; i++ {
		rotationPadding(t, path)
		if _, err := RotateLogFile(path, 1, 2, false); err != nil {
			t.Fatalf("rotation %d failed: %v", i, err)
		}
	}

	for _, p := range []string{path + ".1", path + ".2"} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected rotation %s to exist: %v", p, err)
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Errorf("retention kept base.3 beyond maxFiles=2")
	}
}

func TestRotateLogFile_CompressesQuietRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	rotationPadding(t, path)
	if _, err := RotateLogFile(path, 1, 5, true); err != nil {
		t.Fatal(err)
	}
	plain := path + ".1"
	info, err := os.Stat(plain)
	if err != nil {
		t.Fatal(err)
	}

	rotatedMu.Lock()
	rotatedSizes[plain] = info.Size()
	rotatedWrites[plain] = time.Now().Add(-2 * time.Minute)
	rotatedMu.Unlock()

	compressQuietRotated(path, true, 5)

	if _, err := os.Stat(plain); !os.IsNotExist(err) {
		t.Fatalf("quiet rotation was not compressed to %s.gz", plain)
	}
	f, err := os.Open(plain + ".gz")
	if err != nil {
		t.Fatalf("gzip archive missing: %v", err)
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("archive is not gzip: %v", err)
	}
	var out bytes.Buffer
	if _, err := io.Copy(&out, gr); err != nil {
		t.Fatalf("archive does not decompress: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("padding line")) {
		t.Errorf("archive content mismatch")
	}
}

func TestRotateLogFile_LiveRotationStaysPlain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	rotationPadding(t, path)
	if _, err := RotateLogFile(path, 1, 5, true); err != nil {
		t.Fatal(err)
	}
	plain := path + ".1"
	info, _ := os.Stat(plain)

	rotatedMu.Lock()
	rotatedSizes[plain] = info.Size() + 512
	rotatedWrites[plain] = time.Now().Add(-2 * time.Minute)
	rotatedMu.Unlock()

	compressQuietRotated(path, true, 5)

	if _, err := os.Stat(plain); err != nil {
		t.Fatalf("a growing rotation must stay plain: %v", err)
	}
}

func TestLogHistoryPathsOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	rotationPadding(t, path)
	if _, err := RotateLogFile(path, 1, 3, true); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".1", path+".2"); err != nil {
		t.Fatal(err)
	}
	if err := gzipArchive(path + ".2"); err != nil {
		t.Fatal(err)
	}
	rotationPadding(t, path)
	if _, err := RotateLogFile(path, 1, 3, false); err != nil {
		t.Fatal(err)
	}

	paths := LogHistoryPaths(path, 3)
	if len(paths) != 3 {
		t.Fatalf("history paths = %v, want 3 entries", paths)
	}
	if filepath.Base(paths[0]) != "app.log.3.gz" || filepath.Base(paths[1]) != "app.log.1" || filepath.Base(paths[2]) != "app.log" {
		t.Errorf("history order is not oldest first: %v", paths)
	}
	if !IsGzipPath(paths[0]) || IsGzipPath(paths[1]) {
		t.Errorf("gzip classification wrong: %v", paths)
	}

	plain := PlainRotatedPaths(path, 3)
	if len(plain) != 1 || filepath.Base(plain[0]) != "app.log.1" {
		t.Errorf("plain rotations = %v, want only app.log.1", plain)
	}
}

func gzipArchive(path string) error {
	return gzipFile(path)
}
