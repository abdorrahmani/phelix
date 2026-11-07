// Package security implements application dependency vulnerability
// scanning for Go and Rust projects. It inventories dependencies from
// ecosystem-native files, evaluates them against OSV.dev advisories, and
// persists scan history under the Phelix data directory. Scanning is
// local-only: it never invokes a toolchain, never executes project code,
// never modifies dependency files, and never transmits inventory data.
package security

import (
	"os"
	"path/filepath"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// Ecosystem identifies the dependency ecosystem of an inventory or finding.
type Ecosystem string

const (
	// EcosystemGo is the Go module ecosystem.
	EcosystemGo Ecosystem = "go"
	// EcosystemRust is the Rust crate ecosystem.
	EcosystemRust Ecosystem = "rust"
)

// Confidence states how strongly an inventory entry establishes the
// version an application actually builds with.
type Confidence string

const (
	// ConfidenceResolved: the exact selected version is established by the
	// ecosystem's own resolution record (Cargo.lock, a version-pinned
	// replace directive).
	ConfidenceResolved Confidence = "resolved"
	// ConfidenceDeclaredMinimum: the version is a manifest minimum. The
	// selected build version is equal or higher (Go MVS), never lower.
	ConfidenceDeclaredMinimum Confidence = "declared_minimum"
	// ConfidenceObserved: the version appeared in go.sum checksum
	// evidence. The module was downloaded for a build of this graph, but
	// checksums are not proof of the selected build-list version.
	ConfidenceObserved Confidence = "observed"
	// ConfidenceRequirement: only a manifest version requirement is known
	// (a Rust manifest without Cargo.lock).
	ConfidenceRequirement Confidence = "requirement"
	// ConfidenceUnknown: no usable version evidence (local replacement,
	// git or path source, unparsable version).
	ConfidenceUnknown Confidence = "unknown"
)

// SourceKind classifies where a dependency comes from.
type SourceKind string

const (
	// SourceRegistry is a crates.io dependency, matchable against public
	// RustSec advisories.
	SourceRegistry SourceKind = "registry"
	// SourceOtherRegistry is a dependency from a different registry.
	SourceOtherRegistry SourceKind = "other_registry"
	// SourceGit is a git dependency.
	SourceGit SourceKind = "git"
	// SourcePath is a filesystem dependency.
	SourcePath SourceKind = "path"
	// SourceGoModule is a Go module dependency.
	SourceGoModule SourceKind = "go_module"
	// SourceReplacePinned is a dependency whose version is pinned by a
	// replace directive.
	SourceReplacePinned SourceKind = "replace_pinned"
)

// InventoryLevel summarizes how complete an inventory is for matching.
type InventoryLevel string

const (
	// InventoryResolved: matchable dependencies carry resolved versions.
	InventoryResolved InventoryLevel = "resolved"
	// InventoryManifest: versions are manifest-level evidence only.
	InventoryManifest InventoryLevel = "manifest"
	// InventoryDegraded: files were missing or only partially readable.
	InventoryDegraded InventoryLevel = "degraded"
)

// Module is one dependency identity in an inventory.
type Module struct {
	// Name is the Go module path or Rust crate name.
	Name string `json:"name"`
	// Key is the stable graph node identity (name@version).
	Key string `json:"key,omitempty"`
	// Version is the strongest single version evidence: the resolved
	// version, the declared minimum, the only observed version, or empty.
	Version string `json:"version,omitempty"`
	// Requirement is the manifest version requirement when no resolved
	// version exists (Rust manifest-only inventories).
	Requirement string `json:"requirement,omitempty"`
	// ObservedVersions lists go.sum versions for the module, sorted and
	// deduplicated. It is evidence, never a resolution.
	ObservedVersions []string `json:"observed_versions,omitempty"`
	// Direct reports a declaration in a scanned project manifest.
	Direct bool `json:"direct"`
	// ReplacedFrom is the original module path when a replace directive
	// redirected this dependency to another identity.
	ReplacedFrom string `json:"replaced_from,omitempty"`
	// Resolved reports whether the selected version is established.
	Resolved bool `json:"resolved"`
	// ResolvedFrom names the resolution source (cargo.lock, replace
	// directive).
	ResolvedFrom string `json:"resolved_from,omitempty"`
	// Confidence classifies the version evidence.
	Confidence Confidence `json:"confidence"`
	// SourceKind classifies the dependency origin.
	SourceKind SourceKind `json:"source_kind"`
	// SourceDetail carries the registry or git source identity.
	SourceDetail string `json:"source_detail,omitempty"`
	// Matchable reports whether the module can be evaluated against
	// public advisories for its ecosystem.
	Matchable bool `json:"matchable"`
	// Optional marks a Rust optional dependency declaration.
	Optional bool `json:"optional,omitempty"`
	// Dev marks a Rust development-only dependency declaration.
	Dev bool `json:"dev,omitempty"`
	// Reachable reports whether the lockfile dependency graph connects the
	// module to a workspace member. True for Go modules and Rust
	// inventories without a lockfile.
	Reachable bool `json:"reachable"`
	// DeclaredBy lists the workspace members declaring the dependency.
	DeclaredBy []string `json:"declared_by,omitempty"`
	// Notes carries per-module caveats.
	Notes []string `json:"notes,omitempty"`
}

// GraphEdge is one dependency relationship: the From node depends on the
// To node. Both endpoints are module keys (name@version) that appear in
// Roots or in Modules.
type GraphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Inventory is the dependency inventory of one project directory.
type Inventory struct {
	// Ecosystem of the scanned project.
	Ecosystem Ecosystem `json:"ecosystem"`
	// Level summarizes version evidence quality.
	Level InventoryLevel `json:"level"`
	// Modules holds one entry per distinct dependency identity, sorted by
	// name.
	Modules []Module `json:"modules"`
	// Roots lists the graph keys of the scanned project's own modules or
	// crates: the nodes every dependency is reachable from.
	Roots []string `json:"roots,omitempty"`
	// Edges is the dependency graph: deduplicated parent-to-child
	// relationships between roots and modules.
	Edges []GraphEdge `json:"edges,omitempty"`
	// Notes records inventory-wide caveats and limitations.
	Notes []string `json:"notes,omitempty"`
}

// ModuleKey builds the stable graph node identity for a module or crate:
// the name and, when known, the selected version.
func ModuleKey(name, version string) string {
	if version == "" {
		return name
	}
	return name + "@" + version
}

// DetectEcosystem reports the dependency ecosystem of dir. A directory
// presenting both Go and Rust project markers is ambiguous and scanning
// refuses rather than guessing.
func DetectEcosystem(dir string) (Ecosystem, error) {
	goMarker := false
	for _, name := range []string{"go.mod", "go.work"} {
		if fileExists(filepath.Join(dir, name)) {
			goMarker = true
			break
		}
	}
	cargoMarker := fileExists(filepath.Join(dir, "Cargo.toml"))
	switch {
	case goMarker && cargoMarker:
		return "", phelixerr.New(phelixerr.CodeUnsupportedProject,
			"directory contains both Go (go.mod, go.work) and Rust (Cargo.toml) project markers; scan one project at a time")
	case goMarker:
		return EcosystemGo, nil
	case cargoMarker:
		return EcosystemRust, nil
	default:
		return "", phelixerr.Newf(phelixerr.CodeNotFound,
			"no Go (go.mod, go.work) or Rust (Cargo.toml) project found in %s", filepath.Base(dir))
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
