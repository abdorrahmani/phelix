package cmd

import (
	"github.com/abdorrahmani/phelix/internal/secrets"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadSecretValueFromStdin(t *testing.T) {
	original := os.Stdin
	defer func() { os.Stdin = original }()

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"plain value with trailing newline", "hunter2\n", "hunter2"},
		{"windows newline", "hunter2\r\n", "hunter2"},
		{"multi-line body keeps inner newlines", "-----BEGIN-----\nMIIE\n-----END-----\n", "-----BEGIN-----\nMIIE\n-----END-----"},
		{"no trailing newline stays untouched", "hunter2", "hunter2"},
		{"only one trailing newline is removed", "hunter2\n\n", "hunter2\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			os.Stdin = r
			if _, err := w.WriteString(tc.input); err != nil {
				t.Fatal(err)
			}
			w.Close()
			got, err := readSecretValue("")
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestReadSecretValueFromFileIsVerbatim(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.txt")
	content := "line1\nline2\n\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readSecretValue(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != content {
		t.Fatalf("file input must be stored byte for byte: got %q want %q", got, content)
	}
	if _, err := readSecretValue(filepath.Join(dir, "missing.txt")); err == nil {
		t.Fatal("a missing file must fail")
	}
}

func TestReadSecretValueRejectsInvalidInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "value")
	for _, value := range []string{"", "invalid\x00value", strings.Repeat("x", secrets.MaxValueBytes+1)} {
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readSecretValue(path); err == nil {
			t.Fatal("invalid input accepted")
		}
	}
}
