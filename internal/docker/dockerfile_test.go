package docker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerfileGenerator_HasExistingDockerfile(t *testing.T) {
	dir := t.TempDir()

	gen := NewDockerfileGenerator(dir, LanguageGo)
	if gen.HasExistingDockerfile() {
		t.Error("should not find Dockerfile in empty directory")
	}

	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !gen.HasExistingDockerfile() {
		t.Error("should find Dockerfile after creating one")
	}
}

// TestGoDockerfile_LayerOrdering verifies that dependency files are copied
// before source code in the generated Dockerfile. This is critical for
// Docker layer caching: if go.mod/go.sum are copied and `go mod download`
// runs before the rest of the source is copied, Docker caches the
// dependency download layer and only rebuilds it when dependencies change.
func TestGoDockerfile_LayerOrdering(t *testing.T) {
	gen := NewDockerfileGenerator("/tmp/test", LanguageGo)
	dockerfile, err := gen.Generate()
	if err != nil {
		t.Fatal(err)
	}

	// Find the positions of key instructions
	goModPos := strings.Index(dockerfile, "COPY go.mod go.sum")
	downloadPos := strings.Index(dockerfile, "go mod download")
	sourceCopyPos := strings.Index(dockerfile, "COPY . .")
	buildPos := strings.Index(dockerfile, "go build")

	if goModPos == -1 {
		t.Fatal("missing 'COPY go.mod go.sum' instruction")
	}
	if downloadPos == -1 {
		t.Fatal("missing 'go mod download' instruction")
	}
	if sourceCopyPos == -1 {
		t.Fatal("missing 'COPY . .' instruction")
	}
	if buildPos == -1 {
		t.Fatal("missing 'go build' instruction")
	}

	// Dependencies must be copied before source code
	if goModPos > sourceCopyPos {
		t.Error("go.mod/go.sum must be COPY'd before source code for layer caching")
	}
	// go mod download must run after copying dependency manifests
	if downloadPos < goModPos {
		t.Error("go mod download must run after COPY go.mod go.sum")
	}
	// go mod download must run before copying source
	if downloadPos > sourceCopyPos {
		t.Error("go mod download must run before COPY . .")
	}
	// Build must happen after source copy
	if buildPos < sourceCopyPos {
		t.Error("go build must run after COPY . .")
	}
}

// writeRustProject lays down a minimal Rust project (Cargo.toml + optional
// files) in a temp dir so generateRust can read the manifest, and returns a
// generator rooted there.
func writeRustProject(t *testing.T, cargoToml string, files map[string]string) *DockerfileGenerator {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte(cargoToml), 0o644); err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return NewDockerfileGenerator(dir, LanguageRust)
}

const plainCargo = "[package]\nname = \"myapp\"\nversion = \"0.1.0\"\nedition = \"2021\"\n"

// TestRustDockerfile_CacheMountBuild verifies the new caching contract: a
// single builder stage builds under BuildKit cache mounts (no dummy-main deps
// stage, no COPY --from=deps /app/target), errors are not swallowed, and the
// resolved binary is copied out of the (non-layer) target cache in the same RUN.
func TestRustDockerfile_CacheMountBuild(t *testing.T) {
	gen := writeRustProject(t, plainCargo, map[string]string{"src/main.rs": "fn main() {}"})
	dockerfile, err := gen.Generate()
	if err != nil {
		t.Fatal(err)
	}

	// The syntax directive enabling RUN --mount must be the very first line.
	if !strings.HasPrefix(dockerfile, "# syntax=docker/dockerfile:1\n") {
		t.Fatal("Rust Dockerfile must start with the docker/dockerfile:1 syntax directive")
	}
	// The broken deps-stage machinery must be gone.
	if strings.Contains(dockerfile, "fn main()") {
		t.Error("must not create a dummy main.rs (the primary bug)")
	}
	if strings.Contains(dockerfile, "COPY --from=deps") {
		t.Error("must not COPY /app/target from a deps stage (the primary bug)")
	}
	if strings.Contains(dockerfile, "|| true") {
		t.Error("must not swallow build errors with '|| true'")
	}
	// Cache mounts for the registry and the target dir.
	if !strings.Contains(dockerfile, "--mount=type=cache,target=/usr/local/cargo/registry") {
		t.Error("missing cargo registry cache mount")
	}
	if !strings.Contains(dockerfile, "--mount=type=cache,target=/app/target") {
		t.Error("missing target dir cache mount")
	}
	// The binary is copied out in the same RUN (cache mounts aren't layers).
	buildPos := strings.Index(dockerfile, "cargo build --release")
	cpPos := strings.Index(dockerfile, "cp target/release/myapp /app/server")
	if buildPos == -1 {
		t.Fatal("missing 'cargo build --release'")
	}
	if cpPos == -1 {
		t.Fatal("must copy the resolved package binary to /app/server")
	}
	if cpPos < buildPos {
		t.Error("binary copy must run after cargo build, in the same RUN")
	}
}

// TestRustBinaryResolution covers the Cargo.toml shapes the generator must
// handle deterministically, and the ones it must fail-safe on.
func TestRustBinaryResolution(t *testing.T) {
	cases := []struct {
		name    string
		cargo   string
		files   map[string]string
		wantBin string // expected `cp target/release/<wantBin>` when wantErr is false
		wantErr bool
	}{
		{
			name:    "plain binary",
			cargo:   plainCargo,
			files:   map[string]string{"src/main.rs": "fn main() {}"},
			wantBin: "myapp",
		},
		{
			name:    "lib plus single explicit bin",
			cargo:   "[package]\nname = \"mylib\"\nversion = \"0.1.0\"\n\n[lib]\nname = \"mylib\"\n\n[[bin]]\nname = \"myserver\"\npath = \"src/bin/server.rs\"\n",
			wantBin: "myserver",
		},
		{
			name:    "default-run picks among multiple bins",
			cargo:   "[package]\nname = \"multi\"\nversion = \"0.1.0\"\ndefault-run = \"web\"\n\n[[bin]]\nname = \"web\"\n\n[[bin]]\nname = \"worker\"\n",
			wantBin: "web",
		},
		{
			name:    "hyphenated package name kept verbatim",
			cargo:   "[package]\nname = \"my-cool-app\"\nversion = \"0.1.0\"\n",
			files:   map[string]string{"src/main.rs": "fn main() {}"},
			wantBin: "my-cool-app",
		},
		{
			name:    "multiple bins without default-run is ambiguous",
			cargo:   "[package]\nname = \"multi\"\nversion = \"0.1.0\"\n\n[[bin]]\nname = \"web\"\n\n[[bin]]\nname = \"worker\"\n",
			wantErr: true,
		},
		{
			name:    "virtual workspace is refused",
			cargo:   "[workspace]\nmembers = [\"a\", \"b\"]\n",
			wantErr: true,
		},
		{
			name:    "library-only crate is refused",
			cargo:   "[package]\nname = \"mylib\"\nversion = \"0.1.0\"\n\n[lib]\nname = \"mylib\"\n",
			wantErr: true,
		},
		{
			name:    "src/bin makes it ambiguous",
			cargo:   plainCargo,
			files:   map[string]string{"src/main.rs": "fn main() {}", "src/bin/tool.rs": "fn main() {}"},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gen := writeRustProject(t, tc.cargo, tc.files)
			out, err := gen.Generate()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got Dockerfile:\n%s", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			want := "cp target/release/" + tc.wantBin + " /app/server"
			if !strings.Contains(out, want) {
				t.Fatalf("expected %q in Dockerfile, got:\n%s", want, out)
			}
		})
	}
}

func TestGoDockerfile_ToolchainVersionArg(t *testing.T) {
	dockerfile, _ := NewDockerfileGenerator("/tmp/test", LanguageGo).Generate()
	if !strings.Contains(dockerfile, "ARG GO_VERSION=1.23") {
		t.Fatal("Go Dockerfile should declare GO_VERSION with the existing default")
	}
	if !strings.Contains(dockerfile, "FROM golang:${GO_VERSION}-alpine AS builder") {
		t.Fatal("Go builder image should consume GO_VERSION")
	}
}

func TestRustDockerfile_ToolchainVersionArg(t *testing.T) {
	// plainCargo declares edition 2021 (min 1.56), so the floor default wins.
	gen := writeRustProject(t, plainCargo, map[string]string{"src/main.rs": "fn main() {}"})
	dockerfile, err := gen.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dockerfile, "ARG RUST_VERSION=1.85") {
		t.Fatalf("Rust Dockerfile should declare the floor RUST_VERSION default, got:\n%s", dockerfile)
	}
	// One builder stage now (the dummy deps stage is gone); it must still
	// consume RUST_VERSION so matrix --build-arg pins the toolchain.
	if strings.Count(dockerfile, "FROM rust:${RUST_VERSION}-slim") != 1 {
		t.Fatal("the Rust builder stage should consume RUST_VERSION")
	}
}

// TestRustToolchainDefault pins the edition→toolchain mapping (and MSRV
// override) that keeps the pinned base image new enough for the crate — the
// edition-2024-needs-1.85 fix. A present-but-unmappable edition/rust-version is
// a fail-safe error, not a silent wrong toolchain.
func TestRustToolchainDefault(t *testing.T) {
	cases := []struct {
		name    string
		cargo   string
		wantArg string // expected `ARG RUST_VERSION=<x.y>` when wantErr is false
		wantErr bool
	}{
		{
			name:    "edition 2015 floored to default",
			cargo:   "[package]\nname = \"a\"\nversion = \"0.1.0\"\nedition = \"2015\"\n",
			wantArg: "1.85",
		},
		{
			name:    "edition 2018 floored to default",
			cargo:   "[package]\nname = \"a\"\nversion = \"0.1.0\"\nedition = \"2018\"\n",
			wantArg: "1.85",
		},
		{
			name:    "edition 2021 floored to default",
			cargo:   "[package]\nname = \"a\"\nversion = \"0.1.0\"\nedition = \"2021\"\n",
			wantArg: "1.85",
		},
		{
			name:    "edition 2024 needs 1.85",
			cargo:   "[package]\nname = \"a\"\nversion = \"0.1.0\"\nedition = \"2024\"\n",
			wantArg: "1.85",
		},
		{
			name:    "missing edition falls to floor",
			cargo:   "[package]\nname = \"a\"\nversion = \"0.1.0\"\n",
			wantArg: "1.85",
		},
		{
			name:    "rust-version above edition floor wins",
			cargo:   "[package]\nname = \"a\"\nversion = \"0.1.0\"\nedition = \"2021\"\nrust-version = \"1.90\"\n",
			wantArg: "1.90",
		},
		{
			name:    "rust-version with patch, major.minor kept",
			cargo:   "[package]\nname = \"a\"\nversion = \"0.1.0\"\nrust-version = \"1.88.2\"\n",
			wantArg: "1.88",
		},
		{
			name:    "rust-version below floor is ignored",
			cargo:   "[package]\nname = \"a\"\nversion = \"0.1.0\"\nrust-version = \"1.70\"\n",
			wantArg: "1.85",
		},
		{
			name:    "unknown edition is a fail-safe error",
			cargo:   "[package]\nname = \"a\"\nversion = \"0.1.0\"\nedition = \"2027\"\n",
			wantErr: true,
		},
		{
			name:    "malformed rust-version is a fail-safe error",
			cargo:   "[package]\nname = \"a\"\nversion = \"0.1.0\"\nrust-version = \"stable\"\n",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gen := writeRustProject(t, tc.cargo, map[string]string{"src/main.rs": "fn main() {}"})
			out, err := gen.Generate()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected a fail-safe error, got Dockerfile:\n%s", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			want := "ARG RUST_VERSION=" + tc.wantArg + "\n"
			if !strings.Contains(out, want) {
				t.Fatalf("expected %q in Dockerfile, got:\n%s", want, out)
			}
		})
	}
}

// TestGoToolchainDefault pins that the generated GO_VERSION default follows the
// module's own `go` directive (floored), so a module needing a newer toolchain
// than the floor builds without a hand override.
func TestGoToolchainDefault(t *testing.T) {
	cases := []struct {
		name    string
		goMod   string // empty means no go.mod written
		wantArg string
	}{
		{name: "directive above floor wins", goMod: "module x\n\ngo 1.27\n", wantArg: "1.27"},
		{name: "directive with patch, major.minor kept", goMod: "module x\n\ngo 1.27.3\n", wantArg: "1.27"},
		{name: "directive below floor floored", goMod: "module x\n\ngo 1.19\n", wantArg: "1.23"},
		{name: "no go.mod falls to floor", goMod: "", wantArg: "1.23"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.goMod != "" {
				if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(tc.goMod), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			out, err := NewDockerfileGenerator(dir, LanguageGo).Generate()
			if err != nil {
				t.Fatal(err)
			}
			want := "ARG GO_VERSION=" + tc.wantArg + "\n"
			if !strings.Contains(out, want) {
				t.Fatalf("expected %q in Dockerfile, got:\n%s", want, out)
			}
		})
	}
}

func TestDockerfileGenerator_UnsupportedLanguage(t *testing.T) {
	gen := NewDockerfileGenerator("/tmp/test", Language("python"))
	out, err := gen.Generate()
	if err != nil {
		t.Errorf("unsupported language should not error, got %v", err)
	}
	if out != "" {
		t.Error("unsupported language should return empty string")
	}
}

func TestGoDockerfile_StaticBinary(t *testing.T) {
	gen := NewDockerfileGenerator("/tmp/test", LanguageGo)
	dockerfile, _ := gen.Generate()

	// Should use CGO_ENABLED=0 for static binary
	if !strings.Contains(dockerfile, "CGO_ENABLED=0") {
		t.Error("Go Dockerfile should use CGO_ENABLED=0 for static binary")
	}
	// Should use scratch or alpine for minimal runtime
	if !strings.Contains(dockerfile, "FROM scratch") {
		t.Error("Go Dockerfile should use scratch for minimal runtime image")
	}
}

func TestRustDockerfile_MinimalRuntime(t *testing.T) {
	gen := writeRustProject(t, plainCargo, map[string]string{"src/main.rs": "fn main() {}"})
	dockerfile, err := gen.Generate()
	if err != nil {
		t.Fatal(err)
	}

	// Should use debian:slim for glibc compatibility
	if !strings.Contains(dockerfile, "debian:bookworm-slim") {
		t.Error("Rust Dockerfile should use debian:slim for runtime")
	}
	// glibc Rust binaries linking openssl-sys need libssl at runtime.
	if !strings.Contains(dockerfile, "libssl3") {
		t.Error("Rust runtime should install libssl3 for dynamically-linked OpenSSL")
	}
	// Both runtimes must drop root.
	if !strings.Contains(dockerfile, "USER app") {
		t.Error("Rust runtime should run as a non-root user")
	}
}

// The manifest COPY instructions must use optional wildcards: a dependency-free
// Go project has no go.sum (and a fresh `cargo new` project no Cargo.lock), and
// an unconditional COPY of a missing file fails the whole build.
func TestDockerfile_OptionalManifestWildcards(t *testing.T) {
	goGen := NewDockerfileGenerator("/tmp/test", LanguageGo)
	goDockerfile, _ := goGen.Generate()
	if !strings.Contains(goDockerfile, "COPY go.mod go.sum* ./") {
		t.Fatal("Go Dockerfile must copy go.sum with the optional wildcard")
	}
	// The Rust path no longer copies manifests separately — it `COPY . .`s the
	// whole project, so a missing Cargo.lock is a non-issue (nothing to match).
	rustGen := writeRustProject(t, plainCargo, map[string]string{"src/main.rs": "fn main() {}"})
	rust, err := rustGen.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rust, "COPY . .") {
		t.Fatal("Rust Dockerfile must copy the whole project so Cargo.lock is optional")
	}
}
