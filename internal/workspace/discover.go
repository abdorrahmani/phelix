package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/abdorrahmani/phelix/internal/builder"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/pelletier/go-toml/v2"
)

// GoWorkFile is the Go workspace file recognized for native discovery.
const GoWorkFile = "go.work"

// Discovery sources reported on the workspace. A manifest is the explicit
// source; go.work and cargo are the native ones.
const (
	SourceManifest = "manifest"
	SourceGoWork   = "go.work"
	SourceCargo    = "cargo"
)

// Discovery is the outcome of native workspace discovery: the file the
// members came from and the member paths as declared, relative to the
// workspace root.
type Discovery struct {
	Source  string
	Members []string
}

// Discover inspects root for a native Go or Cargo workspace and returns its
// member directories. go.work wins when both exist. Members are validated to
// exist on disk: a workspace file listing a missing module is a broken
// workspace and fails closed instead of silently shrinking the mesh.
func Discover(root string) (*Discovery, error) {
	workPath := filepath.Join(root, GoWorkFile)
	if raw, err := os.ReadFile(workPath); err == nil {
		members, perr := ParseGoWork(raw)
		if perr != nil {
			return nil, phelixerr.Wrapf(phelixerr.CodeConfiguration, perr, "%s is malformed", workPath)
		}
		if err := requireDirs(root, members, GoWorkFile); err != nil {
			return nil, err
		}
		return &Discovery{Source: SourceGoWork, Members: members}, nil
	} else if !os.IsNotExist(err) {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to read %s", workPath)
	}

	cargoPath := filepath.Join(root, "Cargo.toml")
	if raw, err := os.ReadFile(cargoPath); err == nil {
		members, perr := cargoMembers(root, raw)
		if perr != nil {
			return nil, err
		}
		if err := requireDirs(root, members, "Cargo.toml"); err != nil {
			return nil, err
		}
		if len(members) > 0 {
			return &Discovery{Source: SourceCargo, Members: members}, nil
		}
		return nil, phelixerr.Newf(
			phelixerr.CodeConfiguration,
			"%s declares no workspace members",
			cargoPath,
		)
	} else if !os.IsNotExist(err) {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to read %s", cargoPath)
	}

	return nil, phelixerr.Newf(
		phelixerr.CodeNotFound,
		"no workspace found in %s\nHint: create %s, or run from a directory containing %s or a Cargo workspace",
		root, FileName, GoWorkFile,
	)
}

// ParseGoWork extracts the use entries of a go.work file. Both the single
// form (use ./module) and the parenthesized block are supported, with line
// comments and optional quoting, mirroring the syntax the go command
// accepts. go, toolchain, and replace directives are ignored.
func ParseGoWork(raw []byte) ([]string, error) {
	var members []string
	inBlock := false
	for _, line := range strings.Split(string(raw), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch {
		case inBlock:
			if line == ")" {
				inBlock = false
				continue
			}
			member, err := unquoteGoWorkPath(line)
			if err != nil {
				return nil, err
			}
			members = append(members, member)
		case line == "use (":
			inBlock = true
		case strings.HasPrefix(line, "use "):
			member, err := unquoteGoWorkPath(strings.TrimSpace(strings.TrimPrefix(line, "use ")))
			if err != nil {
				return nil, err
			}
			members = append(members, member)
		}
	}
	if inBlock {
		return nil, fmt.Errorf("unterminated use block")
	}
	return members, nil
}

// unquoteGoWorkPath strips optional double quotes from one use entry.
func unquoteGoWorkPath(entry string) (string, error) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return "", fmt.Errorf("empty use entry")
	}
	if unquoted, err := strconv.Unquote(entry); err == nil {
		entry = unquoted
	} else if len(entry) > 1 && strings.HasPrefix(entry, `"`) && strings.HasSuffix(entry, `"`) {
		entry = entry[1 : len(entry)-1]
	}
	return filepath.ToSlash(filepath.Clean(entry)), nil
}

// cargoWorkspaceFile models the [workspace] section of a root Cargo.toml.
type cargoWorkspaceFile struct {
	Workspace *struct {
		Members        []string `toml:"members"`
		DefaultMembers []string `toml:"default-members"`
		Exclude        []string `toml:"exclude"`
	} `toml:"workspace"`
}

// cargoMembers resolves the effective member list of a Cargo workspace:
// default-members replaces members when present, exclude removes entries,
// and glob patterns expand against the filesystem. Every surviving member
// must be a directory containing a Cargo.toml; crates without one are
// filtered out rather than failing the whole workspace.
func cargoMembers(root string, raw []byte) ([]string, error) {
	var file cargoWorkspaceFile
	if err := toml.Unmarshal(raw, &file); err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "Cargo.toml is malformed")
	}
	if file.Workspace == nil {
		return nil, nil
	}
	patterns := file.Workspace.Members
	if len(file.Workspace.DefaultMembers) > 0 {
		patterns = file.Workspace.DefaultMembers
	}

	declared := map[string]bool{}
	var order []string
	for _, pattern := range patterns {
		matches, err := expandMemberGlob(root, pattern)
		if err != nil {
			return nil, err
		}
		for _, member := range matches {
			normalized := filepath.ToSlash(filepath.Clean(member))
			if !declared[normalized] {
				declared[normalized] = true
				order = append(order, normalized)
			}
		}
	}

	excluded := map[string]bool{}
	for _, pattern := range file.Workspace.Exclude {
		matches, err := expandMemberGlob(root, pattern)
		if err != nil {
			return nil, err
		}
		for _, member := range matches {
			excluded[filepath.ToSlash(filepath.Clean(member))] = true
		}
	}

	var members []string
	for _, member := range order {
		if excluded[member] {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, member, "Cargo.toml")); err != nil {
			continue
		}
		members = append(members, member)
	}
	sort.Strings(members)
	return members, nil
}

// expandMemberGlob expands one Cargo members pattern relative to root. A
// plain path yields itself when it exists on disk; a glob yields every
// matching directory.
func expandMemberGlob(root, pattern string) ([]string, error) {
	pattern = filepath.FromSlash(pattern)
	full := pattern
	if !filepath.IsAbs(full) {
		full = filepath.Join(root, pattern)
	}
	matches, err := filepath.Glob(full)
	if err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "Cargo.toml has invalid members pattern %q", pattern)
	}
	var dirs []string
	for _, match := range matches {
		info, err := os.Stat(match)
		if err != nil || !info.IsDir() {
			continue
		}
		rel, err := filepath.Rel(root, match)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			continue
		}
		dirs = append(dirs, filepath.ToSlash(rel))
	}
	return dirs, nil
}

// requireDirs verifies every declared member exists below root and never
// escapes it.
func requireDirs(root string, members []string, source string) error {
	for _, member := range members {
		full := member
		if !filepath.IsAbs(full) {
			full = filepath.Join(root, filepath.FromSlash(member))
		}
		info, err := os.Stat(full)
		if err != nil {
			return phelixerr.Newf(
				phelixerr.CodeConfiguration,
				"%s lists member %q which does not exist in %s",
				source, member, root,
			)
		}
		if !info.IsDir() {
			return phelixerr.Newf(phelixerr.CodeConfiguration, "%s lists member %q which is not a directory", source, member)
		}
	}
	return nil
}

// DiscoverRunnables filters discovery members down to the directories Phelix
// can actually run: a Go module with a main package, or a Rust crate with a
// binary target. Library-only members are part of the workspace for the
// language toolchain but are not deployable services, so they are skipped.
// The returned map keys service names (directory base names) to member paths.
func DiscoverRunnables(root string, members []string) (map[string]string, error) {
	runnable := map[string]string{}
	for _, member := range members {
		dir := filepath.Join(root, filepath.FromSlash(member))
		if !runnableDir(dir) {
			continue
		}
		name := filepath.Base(filepath.FromSlash(member))
		if err := validateServiceName(name); err != nil {
			return nil, phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "member %q cannot be a service name", member)
		}
		if existing, clash := runnable[name]; clash {
			return nil, phelixerr.Newf(
				phelixerr.CodeAlreadyExists,
				"workspace members %q and %q both resolve to service name %q",
				existing, member, name,
			)
		}
		runnable[name] = member
	}
	return runnable, nil
}

// runnableDir reports whether dir holds a service Phelix can build and run.
func runnableDir(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
		return builder.NewProjectScan(dir).HasMainPackage()
	}
	if _, err := os.Stat(filepath.Join(dir, "Cargo.toml")); err == nil {
		return rustCrateHasBinary(dir)
	}
	return false
}

// rustCrateHasBinary reports whether a crate directory produces a binary:
// either an explicit [[bin]] target or the conventional src/main.rs.
func rustCrateHasBinary(dir string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, "Cargo.toml"))
	if err != nil {
		return false
	}
	var crate struct {
		Bin []struct {
			Name string `toml:"name"`
		} `toml:"bin"`
	}
	if err := toml.Unmarshal(raw, &crate); err == nil && len(crate.Bin) > 0 {
		return true
	}
	if _, err := os.Stat(filepath.Join(dir, "src", "main.rs")); err == nil {
		return true
	}
	return false
}
