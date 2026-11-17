package logs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestLogCollectorPreservesBacklogAndPartialLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	var content strings.Builder
	for i := range 450 {
		fmt.Fprintf(&content, "line-%03d\n", i)
	}
	content.WriteString("partial")
	if err := os.WriteFile(path, []byte(content.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewGenericLogCollector(200, 1000)
	defer c.Prune(func(string) bool { return false })
	var entries []LogEntry
	for range 3 {
		out, err := c.Collect("app", path, identityMapper)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, out...)
	}
	if len(entries) != 450 {
		t.Fatalf("collected %d of 450 complete lines", len(entries))
	}
	for i, entry := range entries {
		if want := fmt.Sprintf("line-%03d", i); entry.Log != want {
			t.Fatalf("line %d = %q, want %q", i, entry.Log, want)
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(" finished\n"); err != nil {
		t.Fatal(err)
	}
	out, err := c.Collect("app", path, identityMapper)
	if err != nil || len(out) != 1 || out[0].Log != "partial finished" {
		t.Fatalf("completed partial line: %v, error: %v", out, err)
	}
}

func TestLogCollectorReadsRewrittenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("old long line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewGenericLogCollector(200, 10)
	defer c.Prune(func(string) bool { return false })
	if _, err := c.Collect("app", path, identityMapper); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := c.Collect("app", path, identityMapper)
	if err != nil || len(out) != 1 || out[0].Log != "new" {
		t.Fatalf("rewritten file: %v, error: %v", out, err)
	}
}

func TestLogCollectorConcurrentReadsAndPrune(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("line\n", 400)), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewGenericLogCollector(200, 400)
	defer c.Prune(func(string) bool { return false })
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if _, err := c.Collect("app", path, identityMapper); err != nil {
				t.Error(err)
			}
		})
		wg.Go(func() { c.Prune(func(string) bool { return false }) })
	}
	wg.Wait()
}
