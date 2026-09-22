package docker

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	toml "github.com/pelletier/go-toml/v2"
)

// DockerfileGenerator produces multi-stage Dockerfiles optimized for layer caching.
type DockerfileGenerator struct {
	ProjectRoot string
	Language    Language
}

// NewDockerfileGenerator creates a generator for the given project.
func NewDockerfileGenerator(projectRoot string, lang Language) *DockerfileGenerator {
	return &DockerfileGenerator{ProjectRoot: projectRoot, Language: lang}
}

// HasExistingDockerfile reports whether a user-provided Dockerfile already exists.
func (g *DockerfileGenerator) HasExistingDockerfile() bool {
	_, err := os.Stat(filepath.Join(g.ProjectRoot, "Dockerfile"))
	return err == nil
}

// Generate produces a multi-stage Dockerfile string optimized for Docker layer
// caching. It returns an error only for the Rust path, and only when the
// project's binary target cannot be resolved deterministically (a virtual
// workspace, or several binaries with no default-run) — a fail-safe refusal to
// emit a Dockerfile that would build the wrong artifact.
//
// Caching layer ordering rationale:
//
// For Go:
//  1. Builder stage: Copy go.mod + go.sum FIRST, then run `go mod download`.
//     This creates a cached layer for all dependency downloads. Docker rebuilds
//     this layer only when go.mod or go.sum change, not on every source edit.
//  2. Copy the rest of the source AFTER dependencies are cached.
//  3. Final stage: minimal scratch image with just the static binary, the CA
//     bundle (so outbound HTTPS works), and a non-root numeric UID.
//
// For Rust:
//  1. Builder stage: copy the whole project, then `cargo build --release`
//     under BuildKit cache mounts for the cargo registry and the target dir.
//     Both caches persist across builds on the host, so dependency compilation
//     (minutes for a typical project) happens once. Because a cache mount is
//     not an image layer, the release binary is copied out to /app/server in
//     the same RUN.
//  2. Final stage: debian:bookworm-slim with ca-certificates + libssl3 (many
//     Rust crates dynamically link OpenSSL) and a non-root user.
//
// The dummy-main.rs "deps stage" approach the earlier generator used was
// removed: a placeholder src/main.rs does not satisfy a project with a [lib],
// multiple [[bin]] targets, or a [workspace], so `cargo build` produced no
// target/ and the downstream `COPY --from=deps /app/target` failed with a
// cryptic "not found" checksum error. Cache mounts cache the same work without
// a placeholder that has to mirror the real crate's shape.
func (g *DockerfileGenerator) Generate() (string, error) {
	switch g.Language {
	case LanguageGo:
		return g.generateGo(), nil
	case LanguageRust:
		return g.generateRust()
	default:
		return "", nil
	}
}

func (g *DockerfileGenerator) generateGo() string {
	var b strings.Builder
	b.WriteString("# Stage 1: Build\n")
	// GO_VERSION is a build arg so `docker build --build-arg GO_VERSION=1.22`
	// (e.g. from a matrix build) can pin a different toolchain without editing
	// the generated file. Its DEFAULT is derived from the module's own `go`
	// directive (floored at a sane recent stable) so a project that needs a
	// newer toolchain than the floor builds without a hand override.
	b.WriteString("ARG GO_VERSION=" + goDefaultVersion(g.ProjectRoot) + "\n")
	b.WriteString("FROM golang:${GO_VERSION}-alpine AS builder\n\n")
	b.WriteString("WORKDIR /app\n\n")
	// The scratch runtime carries no CA bundle; alpine's Go image ships none
	// either, so install it here and copy it across. Without it every outbound
	// HTTPS call from the app fails with an x509 "unknown authority" error.
	b.WriteString("# CA bundle for the scratch runtime (any outbound HTTPS needs it)\n")
	b.WriteString("RUN apk add --no-cache ca-certificates\n\n")
	// Dependency layer — cached unless go.mod/go.sum change. The go.sum
	// wildcard keeps the copy valid for dependency-free projects (no go.sum).
	b.WriteString("# Copy dependency manifests first for layer caching\n")
	b.WriteString("COPY go.mod go.sum* ./\n")
	b.WriteString("RUN go mod download\n\n")
	// Source layer — only rebuilt when source changes
	b.WriteString("# Copy source code\n")
	b.WriteString("COPY . .\n\n")
	// Build with CGO_ENABLED=0 for a fully static binary compatible with scratch.
	b.WriteString("RUN CGO_ENABLED=0 GOOS=linux go build -ldflags=\"-s -w\" -o /app/server .\n\n")

	// --- Stage 2: Runtime ---
	// scratch for a statically-linked binary (smallest possible image).
	b.WriteString("# Stage 2: Runtime (minimal image)\n")
	b.WriteString("FROM scratch\n\n")
	b.WriteString("# CA certificates so the app can make outbound HTTPS calls\n")
	b.WriteString("COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt\n")
	b.WriteString("COPY --from=builder /app/server /server\n\n")
	// scratch has no user database, so a numeric UID:GID is the only way to
	// drop root. 65532 is the conventional "nonroot" id (distroless).
	b.WriteString("# Run as a non-root user (numeric — scratch has no /etc/passwd)\n")
	b.WriteString("USER 65532:65532\n\n")
	b.WriteString("ENTRYPOINT [\"/server\"]\n")
	return b.String()
}

func (g *DockerfileGenerator) generateRust() (string, error) {
	// Parse the manifest once: both the binary to ship and the default
	// toolchain are derived from it. An unresolvable target or an edition/
	// rust-version Phelix can't map to a toolchain is a fail-safe error, not a
	// fragile Dockerfile.
	data, err := os.ReadFile(filepath.Join(g.ProjectRoot, "Cargo.toml"))
	if err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeUnsupportedProject, "could not read Cargo.toml", err)
	}
	var m cargoManifest
	if err := toml.Unmarshal(data, &m); err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeUnsupportedProject, "could not parse Cargo.toml", err)
	}
	bin, err := g.rustBinaryName(m)
	if err != nil {
		return "", err
	}
	rustVersion, err := rustDefaultVersion(m)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	// The syntax directive must be the first line: it selects the Dockerfile
	// frontend that understands `RUN --mount`, which the default BuildKit used
	// by `docker build` (and buildx for matrix builds) then honors.
	b.WriteString("# syntax=docker/dockerfile:1\n")

	// --- Stage 1: Build ---
	b.WriteString("# Stage 1: Build\n")
	// RUST_VERSION is a build arg so the toolchain can be pinned per build
	// without editing the generated file (matrix builds pass --build-arg). Its
	// DEFAULT is derived from the crate's edition (and rust-version MSRV when
	// higher) so a modern crate — e.g. edition 2024 needs 1.85+ — builds without
	// a hand override instead of failing on the too-old pinned toolchain.
	b.WriteString("ARG RUST_VERSION=" + rustVersion + "\n")
	b.WriteString("FROM rust:${RUST_VERSION}-slim AS builder\n\n")
	// TARGETPLATFORM scopes the target-dir cache per platform so a multi-
	// platform matrix build never mixes arm64 and amd64 artifacts in one cache.
	// It is empty for a single-platform build (one shared cache), which is fine.
	b.WriteString("ARG TARGETPLATFORM\n\n")
	b.WriteString("WORKDIR /app\n\n")
	b.WriteString("# Copy the whole project (Cargo.toml, Cargo.lock, src, ...)\n")
	b.WriteString("COPY . .\n\n")
	// BuildKit cache mounts replace the old dummy-main deps stage: the cargo
	// registry and the target dir persist across builds on the host, so deps
	// compile once. A cache mount is not part of the image, so the binary is
	// copied to a non-mounted path (/app/server) inside this same RUN; after
	// the step /app/target is empty again. No `|| true` — a real build failure
	// surfaces here instead of as a downstream COPY error.
	b.WriteString("# Build under cache mounts; copy the binary out (mounts aren't layers)\n")
	b.WriteString("RUN --mount=type=cache,target=/usr/local/cargo/registry,id=phelix-cargo-registry \\\n")
	b.WriteString("    --mount=type=cache,target=/app/target,id=phelix-rust-target-${TARGETPLATFORM},sharing=locked \\\n")
	b.WriteString("    cargo build --release && \\\n")
	b.WriteString("    cp target/release/" + bin + " /app/server\n\n")

	// --- Stage 2: Runtime ---
	b.WriteString("# Stage 2: Runtime (minimal image)\n")
	b.WriteString("FROM debian:bookworm-slim AS runtime\n\n")
	// ca-certificates for outbound HTTPS; libssl3 because a glibc Rust binary
	// that pulls in openssl-sys (reqwest, sqlx, ...) links libssl dynamically
	// and will not start without it. Both are small; the apt lists are removed.
	b.WriteString("RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates libssl3 && rm -rf /var/lib/apt/lists/*\n\n")
	// Drop root. debian has adduser/useradd, so a named system user is fine.
	b.WriteString("# Run as a non-root user\n")
	b.WriteString("RUN useradd --user-group --no-create-home --uid 65532 app\n")
	b.WriteString("COPY --from=builder /app/server /server\n")
	b.WriteString("USER app\n\n")
	b.WriteString("ENTRYPOINT [\"/server\"]\n")
	return b.String(), nil
}

// cargoManifest is the subset of Cargo.toml that determines which binary a
// `cargo build --release` produces and which toolchain it needs.
type cargoManifest struct {
	Package *struct {
		Name        string `toml:"name"`
		DefaultRun  string `toml:"default-run"`
		Edition     string `toml:"edition"`
		RustVersion string `toml:"rust-version"`
	} `toml:"package"`
	Workspace *struct{} `toml:"workspace"`
	Bin       []struct {
		Name string `toml:"name"`
	} `toml:"bin"`
}

// validCrateName guards the binary name that is interpolated into the generated
// `cp` command. Cargo restricts names to this set; anything else in a hand-
// edited Cargo.toml is rejected rather than spliced into a shell command.
var validCrateName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// rustBinaryName resolves, deterministically, the name of the release binary
// this project builds — the crate cargo drops at target/release/<name>.
//
// Resolution:
//   - a [package] default-run always wins (explicit user intent);
//   - otherwise the binary set is the union of explicit [[bin]] names, the
//     package name when src/main.rs exists, and every src/bin/*.rs stem;
//   - exactly one member of that set is the answer;
//   - zero members (a library-only crate) or more than one (ambiguous, no
//     default-run) is a fail-safe error with guidance.
//
// A virtual workspace (a [workspace] with no [package]) builds every member and
// has no single binary, so it is refused outright.
//
// ponytail: does not honor [[bin]] path= overrides or autobins=false; those
// rare shapes fall back to the "add your own Dockerfile" error via the count
// check. Upgrade to `cargo metadata` if they show up.
func (g *DockerfileGenerator) rustBinaryName(m cargoManifest) (string, error) {
	if m.Package == nil {
		if m.Workspace != nil {
			return "", phelixerr.New(phelixerr.CodeUnsupportedProject,
				"Cargo.toml is a virtual workspace (no [package]); it has no single binary to run — "+
					"add a Dockerfile that builds the member you want to deploy, or run dockerize from that member's directory")
		}
		return "", phelixerr.New(phelixerr.CodeUnsupportedProject,
			"Cargo.toml has no [package] section; cannot determine the binary to build")
	}

	if dr := strings.TrimSpace(m.Package.DefaultRun); dr != "" {
		return validatedBin(dr)
	}

	// Build the candidate binary set.
	set := map[string]struct{}{}
	for _, bn := range m.Bin {
		if n := strings.TrimSpace(bn.Name); n != "" {
			set[n] = struct{}{}
		}
	}
	if _, err := os.Stat(filepath.Join(g.ProjectRoot, "src", "main.rs")); err == nil {
		if n := strings.TrimSpace(m.Package.Name); n != "" {
			set[n] = struct{}{}
		}
	}
	if entries, err := os.ReadDir(filepath.Join(g.ProjectRoot, "src", "bin")); err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".rs") {
				set[strings.TrimSuffix(e.Name(), ".rs")] = struct{}{}
			}
		}
	}

	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)

	switch len(names) {
	case 1:
		return validatedBin(names[0])
	case 0:
		return "", phelixerr.Newf(phelixerr.CodeUnsupportedProject,
			"no binary target found for package %q (library-only crate?); add a [[bin]] or a Dockerfile", m.Package.Name)
	default:
		return "", phelixerr.Newf(phelixerr.CodeUnsupportedProject,
			"package %q builds multiple binaries (%s); set `default-run` in Cargo.toml to pick one, or add your own Dockerfile",
			m.Package.Name, strings.Join(names, ", "))
	}
}

func validatedBin(name string) (string, error) {
	if !validCrateName.MatchString(name) {
		return "", phelixerr.Newf(phelixerr.CodeUnsupportedProject,
			"binary name %q from Cargo.toml is not a valid crate name", name)
	}
	return name, nil
}

// mmVersion is a major.minor toolchain version. Patch is deliberately dropped:
// the golang:X.Y-alpine and rust:X.Y-slim base images are published as rolling
// major.minor tags, so a derived default must not carry a patch that may have
// no image.
type mmVersion struct{ major, minor int }

func (v mmVersion) String() string { return strconv.Itoa(v.major) + "." + strconv.Itoa(v.minor) }

// atLeast reports whether v >= o.
func (v mmVersion) atLeast(o mmVersion) bool {
	if v.major != o.major {
		return v.major > o.major
	}
	return v.minor >= o.minor
}

func maxVersion(a, b mmVersion) mmVersion {
	if a.atLeast(b) {
		return a
	}
	return b
}

// mmVersionRe accepts "1", "1.85", or "1.85.0" (patch ignored). A leading "go"
// (go.mod toolchain lines) is not expected here — callers pass the bare number.
var mmVersionRe = regexp.MustCompile(`^(\d+)(?:\.(\d+))?(?:\.\d+)?$`)

func parseMMVersion(s string) (mmVersion, bool) {
	m := mmVersionRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return mmVersion{}, false
	}
	major, _ := strconv.Atoi(m[1])
	minor := 0
	if m[2] != "" {
		minor, _ = strconv.Atoi(m[2])
	}
	return mmVersion{major, minor}, true
}

// rustEditionMin maps a Cargo edition to the first Rust release that stabilized
// it. A newer toolchain compiles any older edition, so these are lower bounds.
var rustEditionMin = map[string]mmVersion{
	"2015": {1, 0},
	"2018": {1, 31},
	"2021": {1, 56},
	"2024": {1, 85},
}

// rustFloorDefault is the toolchain used when the crate constrains nothing.
// 1.85 is the minimum that can compile every current edition (incl. 2024) and
// is a really-published rust:1.85-slim image, so it is safe for any crate that
// only declares an older or no edition.
var rustFloorDefault = mmVersion{1, 85}

// rustDefaultVersion derives the default RUST_VERSION ARG value from the crate:
// the highest of {floor, edition minimum, declared rust-version MSRV}. An
// edition or rust-version that is PRESENT but unrecognized/unparseable is a
// fail-safe error (CodeUnsupportedProject) rather than a silent floor: shipping
// the floor for, say, a future edition reproduces exactly the cryptic deep-in-
// cargo failure this guards against, so we surface it at generation time and
// point at the --build-arg RUST_VERSION override. A MISSING edition is Cargo's
// 2015 default — any toolchain builds it — so the floor is correct and silent.
func rustDefaultVersion(m cargoManifest) (string, error) {
	best := rustFloorDefault
	if m.Package != nil {
		if ed := strings.TrimSpace(m.Package.Edition); ed != "" {
			min, ok := rustEditionMin[ed]
			if !ok {
				return "", phelixerr.Newf(phelixerr.CodeUnsupportedProject,
					"Cargo.toml edition %q is not one Phelix maps to a Rust toolchain; "+
						"pin it explicitly with --build-arg RUST_VERSION=<x.y>", ed)
			}
			best = maxVersion(best, min)
		}
		if rv := strings.TrimSpace(m.Package.RustVersion); rv != "" {
			v, ok := parseMMVersion(rv)
			if !ok {
				return "", phelixerr.Newf(phelixerr.CodeUnsupportedProject,
					"Cargo.toml rust-version %q is not a valid version; "+
						"pin the toolchain with --build-arg RUST_VERSION=<x.y>", rv)
			}
			best = maxVersion(best, v)
		}
	}
	return best.String(), nil
}

// goFloorDefault is the toolchain used when go.mod is missing or its `go`
// directive can't be parsed. It preserves the generator's historical default.
var goFloorDefault = mmVersion{1, 23}

// goDirectiveRe captures the version token of the go.mod `go` directive
// (e.g. "go 1.27" or "go 1.27.0").
var goDirectiveRe = regexp.MustCompile(`(?m)^\s*go\s+(\S+)`)

// goDefaultVersion derives the default GO_VERSION ARG from the module's `go`
// directive, floored at goFloorDefault (a newer toolchain compiles an older-
// directive module, so max is safe). Unlike Rust this never errors: the `go`
// directive is a tool-validated, simple field, and if it is absent or oddly
// shaped the floor is a safe recent stable — no reason to refuse a Dockerfile.
func goDefaultVersion(projectRoot string) string {
	best := goFloorDefault
	if data, err := os.ReadFile(filepath.Join(projectRoot, "go.mod")); err == nil {
		if m := goDirectiveRe.FindSubmatch(data); m != nil {
			if v, ok := parseMMVersion(string(m[1])); ok {
				best = maxVersion(best, v)
			}
		}
	}
	return best.String()
}

// WriteDockerfile writes the generated Dockerfile to projectRoot/Dockerfile.
func (g *DockerfileGenerator) WriteDockerfile() error {
	content, err := g.Generate()
	if err != nil {
		return err
	}
	if content == "" {
		return phelixerr.Newf(phelixerr.CodeUnsupportedProject, "unsupported language: %s", g.Language)
	}
	if err := os.WriteFile(filepath.Join(g.ProjectRoot, "Dockerfile"), []byte(content), 0o644); err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to write Dockerfile", err)
	}
	return nil
}
