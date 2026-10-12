package workspace

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// ManifestFileName is the workspace manifest filename, read from the user's
// repo root and its ancestors (see FindManifest).
const ManifestFileName = "phelix.workspace.yaml"

// Coordinator state filenames under <base>/workspaces/<name>/.
const (
	stateFileName = "workspace.json"
	lockFileName  = "workspace.lock"
	logFileName   = "workspace.log"
)

// baseDir resolves the base directory Phelix keeps app state under, matching
// internal/deploy (os.UserHomeDir()/.phelix). Workspace coordination state is a
// sibling of that package's apps/ tree.
//
// ponytail: this duplicates internal/deploy's unexported base-dir rule
// (os.UserHomeDir()+".phelix"); converge on one exported helper when deploy and
// internal/app are unified. internal/app resolves its data dir differently
// (honoring PHELIX_DATA_DIR and /.dockerenv) — a pre-existing discrepancy left
// untouched here; workspace deliberately follows deploy's base.
func baseDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "resolve home directory for workspace state")
	}
	return filepath.Join(home, ".phelix"), nil
}

var (
	unsafeNameChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
	repeatedDashes  = regexp.MustCompile(`-{2,}`)
)

// SanitizeName makes a workspace name safe as a single filesystem path
// component: it keeps [A-Za-z0-9._-], replaces every other run with a single
// '-', collapses repeats, trims leading/trailing '-', and falls back to
// "workspace" for an empty result or a bare "."/".." (which would otherwise be
// a path-traversal component).
func SanitizeName(name string) string {
	s := unsafeNameChars.ReplaceAllString(strings.TrimSpace(name), "-")
	s = repeatedDashes.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" || s == "." || s == ".." {
		return "workspace"
	}
	return s
}

// WorkspaceDir is <base>/workspaces/<SanitizeName(name)>.
func WorkspaceDir(name string) (string, error) {
	base, err := baseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "workspaces", SanitizeName(name)), nil
}

// StatePath is the coordinator record path (<dir>/workspace.json).
func StatePath(name string) (string, error) { return workspaceFile(name, stateFileName) }

// LockPath is the flock file path (<dir>/workspace.lock).
func LockPath(name string) (string, error) { return workspaceFile(name, lockFileName) }

// LogPath is the group-op audit log path (<dir>/workspace.log).
func LogPath(name string) (string, error) { return workspaceFile(name, logFileName) }

func workspaceFile(name, file string) (string, error) {
	dir, err := WorkspaceDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, file), nil
}

// EnsureWorkspaceDir creates the workspace state directory with 0700 perms (it
// holds sensitive coordination state) and returns it. The only disk-touching
// helper in this file.
func EnsureWorkspaceDir(name string) (string, error) {
	dir, err := WorkspaceDir(name)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "create workspace state dir %s", dir)
	}
	return dir, nil
}

// FindManifest searches startDir and its ancestors for phelix.workspace.yaml,
// returning its absolute path. It stops at the filesystem root and reports
// CodeNotFound when no manifest exists (never hangs).
func FindManifest(startDir string) (string, error) {
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "resolve %s", startDir)
	}
	for {
		candidate := filepath.Join(dir, ManifestFileName)
		if info, serr := os.Stat(candidate); serr == nil && !info.IsDir() {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", phelixerr.Newf(phelixerr.CodeNotFound, "no %s found in %s or any parent directory", ManifestFileName, startDir)
		}
		dir = parent
	}
}
