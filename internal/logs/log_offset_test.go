package logs

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAppLogWriterFile verifies that a writer wrapping an *os.File exposes the
// same handle, which lets process starters attach the child's stdout/stderr
// straight to the file descriptor (no OS pipe in between).
func TestAppLogWriterFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	w := NewAppLogWriter(f, StreamStdout)
	if got := w.File(); got != f {
		t.Fatalf("File() = %v, want the wrapped handle", got)
	}

	nw := NewAppLogWriter(nonFileWriter{}, StreamStdout)
	if got := nw.File(); got != nil {
		t.Fatalf("File() for non-file dst = %v, want nil", got)
	}
}

type nonFileWriter struct{}

func (nonFileWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestFindOffsetForLastNLinesChunked checks the chunked backward scan against
// files of several sizes, including sizes that straddle the 8 KiB chunk
// boundary and a file without a trailing newline.
func TestFindOffsetForLastNLinesChunked(t *testing.T) {
	cases := []struct {
		name    string
		lines   []string
		wantIdx int // index of first line expected in the last-N window
		n       int
	}{
		{"small", []string{"a", "b", "c"}, 1, 2},
		{"exact", []string{"a", "b", "c"}, 0, 3},
		{"more-than-have", []string{"a", "b"}, 0, 10},
		{"chunk-boundary", makeLineSlice(1000, "line"), 990, 10},
		{"multi-chunk", makeLineSlice(5000, "some longer line to fill"), 4985, 15},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "app.log")
			content := ""
			for _, l := range tc.lines {
				content += l + "\n"
			}
			if err := os.WriteFile(path, []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()

			off, err := findOffsetForLastNLines(f, tc.n)
			if err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 64)
			nread, _ := f.ReadAt(buf, off)
			first := string(buf[:nread])
			if idx := indexByte(first, '\n'); idx >= 0 {
				first = first[:idx]
			}
			if first != tc.lines[tc.wantIdx] {
				t.Fatalf("offset %d: first line %q, want %q", off, first, tc.lines[tc.wantIdx])
			}
		})
	}
}

func makeLineSlice(n int, prefix string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = prefix
	}
	return out
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// TestFindOffsetNoTrailingNewline covers a file whose last line is unterminated.
func TestFindOffsetNoTrailingNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree"), 0644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	off, err := findOffsetForLastNLines(f, 2)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	nread, _ := f.ReadAt(buf, off)
	if got := string(buf[:nread]); got != "two\nthree" {
		t.Fatalf("got %q, want %q", got, "two\nthree")
	}
}
