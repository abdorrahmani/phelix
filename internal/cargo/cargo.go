package cargo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// Metadata is the subset of `cargo metadata --no-deps` that identifies the
// workspace layout and every package's build targets.
type Metadata struct {
	Packages                []Package `json:"packages"`
	WorkspaceMembers        []string  `json:"workspace_members"`
	WorkspaceDefaultMembers []string  `json:"workspace_default_members"`
	TargetDirectory         string    `json:"target_directory"`
	WorkspaceRoot           string    `json:"workspace_root"`
}

// Package describes one Cargo package.
type Package struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	ManifestPath string   `json:"manifest_path"`
	DefaultRun   string   `json:"default_run"`
	Targets      []Target `json:"targets"`
}

// Target describes one build target (bin, lib, example, test, ...).
type Target struct {
	Kind    []string `json:"kind"`
	Name    string   `json:"name"`
	SrcPath string   `json:"src_path"`
}

// Artifact is one `compiler-artifact` message emitted by cargo's JSON output.
type Artifact struct {
	Reason       string   `json:"reason"`
	PackageID    string   `json:"package_id"`
	ManifestPath string   `json:"manifest_path"`
	Target       Target   `json:"target"`
	Executable   *string  `json:"executable"`
	Fresh        bool     `json:"fresh"`
	Filenames    []string `json:"filenames"`
}

// Selection is the exact package and binary target a build must produce.
type Selection struct {
	Package Package
	Target  Target
}

// LoadMetadata asks cargo for the workspace and package layout. It never runs
// a build and never resolves dependencies.
func LoadMetadata(ctx context.Context, projectRoot string) (*Metadata, error) {
	cmd := exec.CommandContext(ctx, "cargo", "metadata", "--format-version", "1", "--no-deps")
	cmd.Dir = projectRoot

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, phelixerr.Wrapf(phelixerr.CodeToolchainNotFound, err,
				"cargo is required to resolve the Rust binary target")
		}
		return nil, phelixerr.Wrapf(phelixerr.CodeBuildFailed, err,
			"cargo metadata failed for %s: %s", projectRoot, firstLine(stderr.String()))
	}

	var meta Metadata
	if err := json.Unmarshal(out, &meta); err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeBuildFailed, err,
			"cargo metadata returned invalid JSON for %s", projectRoot)
	}
	return &meta, nil
}

// SelectFor loads metadata and resolves the package and binary target.
func SelectFor(ctx context.Context, projectRoot string, extraArgs []string) (Selection, error) {
	meta, err := LoadMetadata(ctx, projectRoot)
	if err != nil {
		return Selection{}, err
	}
	return Select(meta, projectRoot, extraArgs)
}

// Select resolves one package and one binary target from metadata and the
// user's cargo arguments. Any selection that would leave more than one
// candidate is rejected instead of guessed.
func Select(meta *Metadata, projectRoot string, extraArgs []string) (Selection, error) {
	if meta == nil || len(meta.Packages) == 0 {
		return Selection{}, phelixerr.New(phelixerr.CodeUnsupportedProject,
			"cargo metadata reported no packages for this project")
	}

	pkg, err := selectPackage(meta, projectRoot, extraArgs)
	if err != nil {
		return Selection{}, err
	}
	target, err := selectTarget(pkg, extraArgs)
	if err != nil {
		return Selection{}, err
	}
	return Selection{Package: pkg, Target: target}, nil
}

func selectPackage(meta *Metadata, projectRoot string, extraArgs []string) (Package, error) {
	specs, err := flagValues(extraArgs, "--package", "-p")
	if err != nil {
		return Package{}, err
	}
	if len(specs) > 0 {
		var matches []Package
		for _, pkg := range meta.Packages {
			for _, spec := range specs {
				if matchesSpec(pkg, spec) {
					matches = appendUniquePackage(matches, pkg)
					break
				}
			}
		}
		switch len(matches) {
		case 1:
			return matches[0], nil
		case 0:
			return Package{}, phelixerr.Newf(phelixerr.CodeNotFound,
				"no package matches --package %s", strings.Join(specs, ", "))
		default:
			return Package{}, phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"--package %s is ambiguous; it matches %s", strings.Join(specs, ", "), packageNames(matches))
		}
	}

	root := cleanAbs(projectRoot)
	var exact []Package
	for _, pkg := range meta.Packages {
		if cleanAbs(filepath.Dir(pkg.ManifestPath)) == root {
			exact = append(exact, pkg)
		}
	}
	switch len(exact) {
	case 1:
		return exact[0], nil
	case 0:
	default:
		return Package{}, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"several packages share the project root %s: %s", projectRoot, packageNames(exact))
	}

	if cleanAbs(meta.WorkspaceRoot) == root {
		members := meta.WorkspaceDefaultMembers
		if len(members) == 0 {
			members = meta.WorkspaceMembers
		}
		pkgs := packagesByID(meta, members)
		switch len(pkgs) {
		case 1:
			return pkgs[0], nil
		case 0:
		default:
			return Package{}, phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"workspace %s builds several packages (%s); pass --package to choose one",
				projectRoot, packageNames(pkgs))
		}
	}

	var under []Package
	for _, pkg := range meta.Packages {
		if isWithin(root, filepath.Dir(pkg.ManifestPath)) {
			under = append(under, pkg)
		}
	}
	switch len(under) {
	case 1:
		return under[0], nil
	case 0:
		return Package{}, phelixerr.Newf(phelixerr.CodeNotFound,
			"no Cargo package found for %s; run the build from a package or workspace directory", projectRoot)
	default:
		return Package{}, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"%s contains several Cargo packages (%s); pass --package to choose one",
			projectRoot, packageNames(under))
	}
}

func selectTarget(pkg Package, extraArgs []string) (Target, error) {
	bins := binTargets(pkg)
	if len(bins) == 0 {
		return Target{}, phelixerr.Newf(phelixerr.CodeUnsupportedProject,
			"package %q has no binary target to deploy", pkg.Name)
	}

	names, err := flagValues(extraArgs, "--bin", "")
	if err != nil {
		return Target{}, err
	}
	if len(names) > 1 {
		return Target{}, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"several --bin targets were requested (%s); deploy exactly one", strings.Join(names, ", "))
	}
	if len(names) == 1 {
		for _, target := range bins {
			if target.Name == names[0] {
				return target, nil
			}
		}
		return Target{}, phelixerr.Newf(phelixerr.CodeNotFound,
			"package %q has no binary target named %q (available: %s)",
			pkg.Name, names[0], targetNames(bins))
	}

	if defaultRun := strings.TrimSpace(pkg.DefaultRun); defaultRun != "" {
		for _, target := range bins {
			if target.Name == defaultRun {
				return target, nil
			}
		}
		return Target{}, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"package %q default-run %q does not name a binary target (available: %s)",
			pkg.Name, defaultRun, targetNames(bins))
	}

	if len(bins) == 1 {
		return bins[0], nil
	}
	return Target{}, phelixerr.Newf(phelixerr.CodeInvalidArgument,
		"package %q builds several binaries (%s); pass --bin or set default-run in Cargo.toml",
		pkg.Name, targetNames(bins))
}

// ParseArtifacts reads cargo's JSON message stream and keeps the
// compiler-artifact records.
func ParseArtifacts(r io.Reader) ([]Artifact, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var artifacts []Artifact
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var artifact Artifact
		if err := json.Unmarshal([]byte(line), &artifact); err != nil {
			continue
		}
		if artifact.Reason == "compiler-artifact" {
			artifacts = append(artifacts, artifact)
		}
	}
	return artifacts, nil
}

// SelectArtifact returns the non-null executable of the selected package and
// binary. Missing or conflicting artifacts are errors, never a guess.
func SelectArtifact(artifacts []Artifact, sel Selection) (string, error) {
	var paths []string
	for _, artifact := range artifacts {
		if artifact.Reason != "compiler-artifact" {
			continue
		}
		if artifact.PackageID != sel.Package.ID {
			continue
		}
		if artifact.Target.Name != sel.Target.Name || !hasKind(artifact.Target.Kind, "bin") {
			continue
		}
		if artifact.Executable == nil || *artifact.Executable == "" {
			continue
		}
		paths = appendUniqueString(paths, filepath.Clean(*artifact.Executable))
	}

	switch len(paths) {
	case 1:
		return paths[0], nil
	case 0:
		return "", phelixerr.Newf(phelixerr.CodeNotFound,
			"cargo reported success but no executable for package %q target %q; "+
				"check that the requested build includes this binary",
			sel.Package.Name, sel.Target.Name)
	default:
		sort.Strings(paths)
		return "", phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"cargo reported several executables for package %q target %q: %s",
			sel.Package.Name, sel.Target.Name, strings.Join(paths, ", "))
	}
}

// MessageFormatArgs removes any user-supplied message format and appends the
// machine-readable one this build depends on.
func MessageFormatArgs(args []string) []string {
	out := make([]string, 0, len(args)+1)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--message-format" {
			if i+1 < len(args) {
				i++
			}
			continue
		}
		if strings.HasPrefix(arg, "--message-format=") {
			continue
		}
		out = append(out, arg)
	}
	return append(out, "--message-format=json-render-diagnostics")
}

// ProfileDir maps cargo profile flags to the directory cargo writes into.
func ProfileDir(args []string) string {
	if profile, _ := flagValue(args, "--profile", ""); profile != "" {
		switch profile {
		case "dev", "test":
			return "debug"
		case "release", "bench":
			return "release"
		default:
			return profile
		}
	}
	if hasFlag(args, "--release") {
		return "release"
	}
	return "debug"
}

// TargetTriple returns the --target triple when one was requested.
func TargetTriple(args []string) string {
	triple, _ := flagValue(args, "--target", "")
	return triple
}

// TargetDir resolves cargo's target directory, honoring --target-dir,
// CARGO_TARGET_DIR and the metadata-provided default.
func TargetDir(meta *Metadata, projectRoot string, args []string) string {
	if dir, _ := flagValue(args, "--target-dir", ""); dir != "" {
		if filepath.IsAbs(dir) {
			return dir
		}
		return filepath.Join(projectRoot, dir)
	}
	if dir := os.Getenv("CARGO_TARGET_DIR"); dir != "" {
		if filepath.IsAbs(dir) {
			return dir
		}
		return filepath.Join(projectRoot, dir)
	}
	if meta != nil && meta.TargetDirectory != "" {
		return meta.TargetDirectory
	}
	return filepath.Join(projectRoot, "target")
}

// ExecutablePath builds the conventional path cargo writes for a binary.
func ExecutablePath(targetDir, triple, profile, bin string) string {
	dir := targetDir
	if triple != "" {
		dir = filepath.Join(dir, triple)
	}
	return filepath.Join(dir, profile, bin)
}

func binTargets(pkg Package) []Target {
	var bins []Target
	for _, target := range pkg.Targets {
		if hasKind(target.Kind, "bin") {
			bins = append(bins, target)
		}
	}
	sort.Slice(bins, func(i, j int) bool { return bins[i].Name < bins[j].Name })
	return bins
}

func hasKind(kinds []string, want string) bool {
	for _, kind := range kinds {
		if kind == want {
			return true
		}
	}
	return false
}

func flagValues(args []string, long, short string) ([]string, error) {
	var values []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == long || (short != "" && arg == short):
			if i+1 >= len(args) {
				return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument, "%s requires a value", arg)
			}
			values = appendUniqueString(values, args[i+1])
			i++
		case strings.HasPrefix(arg, long+"="):
			values = appendUniqueString(values, strings.TrimPrefix(arg, long+"="))
		case short != "" && strings.HasPrefix(arg, short+"="):
			values = appendUniqueString(values, strings.TrimPrefix(arg, short+"="))
		}
	}
	return values, nil
}

func flagValue(args []string, long, short string) (string, error) {
	values, err := flagValues(args, long, short)
	if err != nil {
		return "", err
	}
	if len(values) == 0 {
		return "", nil
	}
	return values[len(values)-1], nil
}

func hasFlag(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func matchesSpec(pkg Package, spec string) bool {
	if pkg.Name == spec || pkg.ID == spec {
		return true
	}
	if name, _, ok := strings.Cut(spec, "@"); ok && name == pkg.Name {
		return true
	}
	return false
}

func packagesByID(meta *Metadata, ids []string) []Package {
	var pkgs []Package
	for _, id := range ids {
		for _, pkg := range meta.Packages {
			if pkg.ID == id {
				pkgs = appendUniquePackage(pkgs, pkg)
				break
			}
		}
	}
	return pkgs
}

func packageNames(pkgs []Package) string {
	names := make([]string, 0, len(pkgs))
	for _, pkg := range pkgs {
		names = append(names, pkg.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func targetNames(targets []Target) string {
	names := make([]string, 0, len(targets))
	for _, target := range targets {
		names = append(names, target.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func appendUniquePackage(pkgs []Package, pkg Package) []Package {
	for _, existing := range pkgs {
		if existing.ID == pkg.ID {
			return pkgs
		}
	}
	return append(pkgs, pkg)
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func cleanAbs(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(abs)
}

func isWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if line == "" {
		return "cargo metadata failed"
	}
	return line
}
