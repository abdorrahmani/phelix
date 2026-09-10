package cmd

import (
	"bytes"
	"io"
	"os"
	"testing"
)

// captureStdout runs fn with os.Stdout replaced by a pipe and returns what
// it printed. Shared helper for command tests that verify terminal output.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	os.Stdout = old
	w.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}
