package matrix

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"

	"github.com/abdorrahmani/phelix/internal/builder"
)

// --- SHA256File ----------------------------------------------------------------

func TestSHA256File_KnownVectors(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name, content, want string
	}{
		{"empty", "", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"abc", "abc", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
		{"hello", "hello world", "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"},
		{"binary", "\x00\x01\x02\xff\xfe", "3fbdad0f6c6a3e8e8b1e0a2b1a0d5f4bb1c9f8e6d3a1b7c4e2f0d9a8b7c6e5f4"},
	}
	// Recompute the binary case through the standard library so the vector
	// itself is not hand-derived: streaming must agree with Sum256.
	sum := sha256.Sum256([]byte(cases[3].content))
	cases[3].want = hex.EncodeToString(sum[:])

	for _, tc := range cases {
		path := filepath.Join(dir, tc.name)
		if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := SHA256File(path)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Fatalf("%s: SHA256File = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestSHA256File_LargeArtifactStreams(t *testing.T) {
	// A buffer several times larger than the io.Copy internal buffer proves
	// the checksum streams the whole file rather than a first chunk.
	path := filepath.Join(t.TempDir(), "large.bin")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 1<<20)
	for i := range chunk {
		chunk[i] = byte(i % 251)
	}
	for i := 0; i < 8; i++ { // 8 MiB
		if _, err := f.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()

	want := sha256.New()
	for i := 0; i < 8; i++ {
		want.Write(chunk)
	}
	got, err := SHA256File(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != hex.EncodeToString(want.Sum(nil)) {
		t.Fatal("large-artifact checksum mismatch — streaming dropped bytes")
	}
}

func TestSHA256File_Deterministic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artifact")
	if err := os.WriteFile(path, []byte("deterministic bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	first, err := SHA256File(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := SHA256File(path)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("same bytes hashed twice: %s != %s", first, second)
	}
}

func TestSHA256File_ReadError(t *testing.T) {
	_, err := SHA256File(filepath.Join(t.TempDir(), "missing"))
	if err == nil {
		t.Fatal("expected an error for a missing artifact")
	}
	if phelixerr.CodeOf(err) != phelixerr.CodeFilesystem {
		t.Fatalf("error code = %s, want FILESYSTEM", phelixerr.CodeOf(err))
	}
}

// --- ShortSHA256 -----------------------------------------------------------------

func TestShortSHA256(t *testing.T) {
	full := strings.Repeat("a", SHA256HexLen)
	got := ShortSHA256(full)
	if got != "aaaaaaaaaaaa…" {
		t.Fatalf("short form = %q", got)
	}
	if runes := len([]rune(got)); runes != 13 { // 12 hex chars + ellipsis
		t.Fatalf("short form has %d runes, want 13: %q", runes, got)
	}
	if got := ShortSHA256("short"); got != "short" {
		t.Fatalf("short input must pass through unchanged, got %q", got)
	}
	if got := ShortSHA256(""); got != "" {
		t.Fatalf("empty input must stay empty, got %q", got)
	}
}

// --- finalizeArtifactChecksum -----------------------------------------------------

func TestFinalizeArtifactChecksum_AttachesAndFailsClosed(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.WriteFile(bin, []byte("artifact bytes"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Success with a real artifact: the checksum is attached and matches the
	// bytes on disk.
	res := &Result{
		Combination: Combination{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
		Status:      "success",
		Artifact:    bin,
	}
	finalizeArtifactChecksum(res)
	if res.Status != "success" || res.SHA256 == "" {
		t.Fatalf("checksum not attached: %+v", res)
	}
	if want, _ := SHA256File(bin); res.SHA256 != want {
		t.Fatalf("checksum = %s, want %s", res.SHA256, want)
	}

	// A checksum failure converts the success into an integrity failure with
	// a non-transient code (never a valid release artifact, never retried).
	bad := &Result{
		Combination: res.Combination,
		Status:      "success",
		Artifact:    filepath.Join(dir, "gone"),
	}
	finalizeArtifactChecksum(bad)
	if bad.Status != "failed" || bad.SHA256 != "" || bad.Error == nil {
		t.Fatalf("checksum failure must fail the combination: %+v", bad)
	}
	if phelixerr.CodeOf(bad.Error) != phelixerr.CodeBuildFailed {
		t.Fatalf("integrity failure code = %s, want BUILD_FAILED", phelixerr.CodeOf(bad.Error))
	}
	if DefaultRetryClassifier(bad.Error) {
		t.Fatal("an integrity failure must not be classified as retryable")
	}

	// Failed results and successes without an artifact are left untouched.
	failed := &Result{Status: "failed", Artifact: bin, Error: phelixerr.New(phelixerr.CodeBuildFailed, "boom")}
	finalizeArtifactChecksum(failed)
	if failed.SHA256 != "" || failed.Status != "failed" {
		t.Fatalf("failed result must not gain a checksum: %+v", failed)
	}
	noArtifact := &Result{Status: "success"}
	finalizeArtifactChecksum(noArtifact)
	if noArtifact.Status != "success" || noArtifact.SHA256 != "" {
		t.Fatalf("artifact-less success must be untouched: %+v", noArtifact)
	}
}

// --- builders produce checksums end to end ---------------------------------------

func TestRustBuilderResultCarriesChecksum(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Cargo.toml"), []byte("[package]\nname='demo'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "main.rs"), []byte("fn main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &RustMatrixBuilder{
		ProjectRoot: root, AppName: "demo",
		LookPath: func(string) (string, error) { return "/fake/cross", nil },
		CommandContext: func(_ context.Context, _ string, args ...string) *exec.Cmd {
			triple := "x86_64-unknown-linux-gnu"
			bin := filepath.Join(root, "target", "rust1.77.2-linux-amd64", triple, "release", "demo")
			_ = os.MkdirAll(filepath.Dir(bin), 0o755)
			_ = os.WriteFile(bin, []byte("rust binary bytes"), 0o755)
			return exec.Command("true")
		},
	}
	c := Combination{Lang: builder.Rust, Version: "1.77.2", OS: "linux", Arch: "amd64", Platform: "linux/amd64"}
	result := r.Build(context.Background(), c)
	if result.Status != "success" {
		t.Fatalf("build failed: %+v", result)
	}
	// The checksummed bytes are the COPIED artifact (builds/matrix/…), not
	// the intermediate target/… binary.
	want, err := SHA256File(result.Artifact)
	if err != nil {
		t.Fatal(err)
	}
	if result.SHA256 != want {
		t.Fatalf("builder checksum = %s, want %s (artifact %s)", result.SHA256, want, result.Artifact)
	}
	if !strings.HasPrefix(result.Artifact, filepath.Join(root, "builds", "matrix")) {
		t.Fatalf("artifact not in the matrix output dir: %s", result.Artifact)
	}
}
