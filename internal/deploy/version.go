package deploy

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// VersionMeta is one row in ~/.phelix/apps/<AppName>/versions.json.
type VersionMeta struct {
	Version    int        `json:"version"`
	Tag        string     `json:"tag,omitempty"`
	GitCommit  string     `json:"git_commit,omitempty"`
	BuiltAt    time.Time  `json:"built_at"`
	SizeBytes  int64      `json:"size_bytes"`
	DeployedAt *time.Time `json:"deployed_at,omitempty"`
	DeployMode string     `json:"deploy_mode,omitempty"`
	IsCurrent  bool       `json:"is_current"`
	// DockerImage is the full image reference (registry/repo:tag) when this
	// version was built via `phelix dockerize`. When present, future deploy
	// logic can use "docker run" as a BuildSource implementation instead of
	// running the binary directly. This field is additive — nil/absent means
	// the version is a native binary build (FreshBuildSource path).
	DockerImage string `json:"docker_image,omitempty"`
}

// VersionsFile is the on-disk metadata index for an app's build history.
type VersionsFile struct {
	Versions []VersionMeta `json:"versions"`
}

// RetentionPolicy caps how many non-current versions may be kept. Plan tiers
// can supply different implementations without changing prune logic.
type RetentionPolicy interface {
	MaxRetainedVersions() int // <= 0 means unlimited
}

// DefaultRetention keeps the last five versions (excluding mandatory retention
// of the active version — see PruneVersions).
type DefaultRetention struct{ Max int }

func (d DefaultRetention) MaxRetainedVersions() int {
	if d.Max <= 0 {
		return 5
	}
	return d.Max
}

// UnlimitedRetention disables pruning.
type UnlimitedRetention struct{}

func (UnlimitedRetention) MaxRetainedVersions() int { return 0 }

// RecordResult holds paths for a newly recorded version.
type RecordResult struct {
	Version    int
	BinaryPath string
	EnvPath    string
}

func appDataDir(appName string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".phelix", "apps", appName), nil
}

func versionsPath(appName string) (string, error) {
	dir, err := appDataDir(appName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "versions.json"), nil
}

func versionDir(appName string, ver int) (string, error) {
	dir, err := appDataDir(appName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "builds", fmt.Sprintf("v%d", ver)), nil
}

func envSnapshotPath(appName string, ver int) (string, error) {
	dir, err := appDataDir(appName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "env", fmt.Sprintf("v%d.enc", ver)), nil
}

// LoadVersions reads versions.json, or an empty file when none exists yet.
func LoadVersions(appName string) (*VersionsFile, error) {
	path, err := versionsPath(appName)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &VersionsFile{}, nil
	}
	if err != nil {
		return nil, err
	}
	var vf VersionsFile
	if err := json.Unmarshal(data, &vf); err != nil {
		return nil, fmt.Errorf("deploy: decode versions.json: %w", err)
	}
	return &vf, nil
}

func saveVersions(appName string, vf *VersionsFile) error {
	path, err := versionsPath(appName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(vf, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// NextVersion returns the next monotonic integer version id.
func NextVersion(appName string) (int, error) {
	vf, err := LoadVersions(appName)
	if err != nil {
		return 0, err
	}
	max := 0
	for _, v := range vf.Versions {
		if v.Version > max {
			max = v.Version
		}
	}
	return max + 1, nil
}

// CurrentVersion returns the version marked current, or 0 when unknown.
func CurrentVersion(appName string) (int, error) {
	vf, err := LoadVersions(appName)
	if err != nil {
		return 0, err
	}
	for _, v := range vf.Versions {
		if v.IsCurrent {
			return v.Version, nil
		}
	}
	return 0, nil
}

// PreviousVersion returns the version before the current one in build order.
func PreviousVersion(appName string) (int, error) {
	vf, err := LoadVersions(appName)
	if err != nil {
		return 0, err
	}
	cur, _ := CurrentVersion(appName)
	if cur == 0 && len(vf.Versions) > 0 {
		// Fall back to highest version as "current" when flag missing.
		sort.Slice(vf.Versions, func(i, j int) bool {
			return vf.Versions[i].Version < vf.Versions[j].Version
		})
		cur = vf.Versions[len(vf.Versions)-1].Version
	}
	if cur <= 1 {
		return 0, fmt.Errorf("deploy: no previous version to roll back to for %q (current v%d)", appName, cur)
	}
	prev := cur - 1
	if !versionExists(vf, prev) {
		list := formatAvailableVersions(vf)
		return 0, fmt.Errorf("deploy: version v%d is not available for %q; available: %s", prev, appName, list)
	}
	return prev, nil
}

func versionExists(vf *VersionsFile, ver int) bool {
	for _, v := range vf.Versions {
		if v.Version == ver {
			return true
		}
	}
	return false
}

// VersionPaths returns absolute binary and env snapshot paths for vN.
func VersionPaths(appName string, ver int) (binaryPath, envPath string, err error) {
	vf, err := LoadVersions(appName)
	if err != nil {
		return "", "", err
	}
	if !versionExists(vf, ver) {
		list := formatAvailableVersions(vf)
		return "", "", fmt.Errorf("deploy: version v%d does not exist for %q; available: %s", ver, appName, list)
	}
	dir, err := versionDir(appName, ver)
	if err != nil {
		return "", "", err
	}
	binaryPath = filepath.Join(dir, "binary")
	if _, err := os.Stat(binaryPath); err != nil {
		list := formatAvailableVersions(vf)
		return "", "", fmt.Errorf("deploy: binary for v%d missing for %q; available: %s", ver, appName, list)
	}
	envPath, _ = envSnapshotPath(appName, ver)
	if _, err := os.Stat(envPath); os.IsNotExist(err) {
		envPath = ""
	}
	return binaryPath, envPath, nil
}

func formatAvailableVersions(vf *VersionsFile) string {
	if vf == nil || len(vf.Versions) == 0 {
		return "(none)"
	}
	vers := make([]int, 0, len(vf.Versions))
	for _, v := range vf.Versions {
		vers = append(vers, v.Version)
	}
	sort.Ints(vers)
	parts := make([]string, len(vers))
	for i, v := range vers {
		parts[i] = "v" + strconv.Itoa(v)
	}
	return strings.Join(parts, ", ")
}

// RecordFreshBuild copies the built binary and env snapshot into versioned
// storage and appends metadata. Pruning runs immediately; the current version
// is never removed even when over retention.
//
// RecordFreshBuild always creates the version with is_current=false. The caller
// must call PromoteVersion only after the deploy's health check passes. This
// two-phase approach ensures that a build which succeeds but whose deploy
// (start / health check) fails leaves the version on disk for inspection but
// never becomes the active version.
func RecordFreshBuild(appName, appID, builtBinaryPath, gitCommit, tag string, policy RetentionPolicy, log Logger) (*RecordResult, error) {
	if policy == nil {
		policy = DefaultRetention{}
	}
	ver, err := NextVersion(appName)
	if err != nil {
		return nil, err
	}
	vdir, err := versionDir(appName, ver)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(vdir, 0o755); err != nil {
		return nil, err
	}
	destBin := filepath.Join(vdir, "binary")
	if err := copyFile(builtBinaryPath, destBin, 0o755); err != nil {
		return nil, fmt.Errorf("deploy: store binary v%d: %w", ver, err)
	}
	info, err := os.Stat(destBin)
	if err != nil {
		return nil, err
	}
	envPath, err := snapshotEnvForVersion(appName, appID, ver)
	if err != nil {
		return nil, err
	}
	vf, err := LoadVersions(appName)
	if err != nil {
		return nil, err
	}
	vf.Versions = append(vf.Versions, VersionMeta{
		Version:   ver,
		Tag:       tag,
		GitCommit: gitCommit,
		BuiltAt:   time.Now(),
		SizeBytes: info.Size(),
		IsCurrent: false,
	})
	if err := saveVersions(appName, vf); err != nil {
		return nil, err
	}
	if err := PruneVersions(appName, policy, log); err != nil {
		return nil, err
	}
	return &RecordResult{Version: ver, BinaryPath: destBin, EnvPath: envPath}, nil
}

// RecordDockerBuild records a Docker image as a new version entry. Unlike
// RecordFreshBuild, it does not copy a binary — the version metadata carries
// a DockerImage reference that future deploy logic can use to run the image.
// The version is created with is_current=false (same two-phase protocol).
func RecordDockerBuild(appName, dockerImage, tag, gitCommit string, policy RetentionPolicy, log Logger) (*RecordResult, error) {
	if policy == nil {
		policy = DefaultRetention{}
	}
	ver, err := NextVersion(appName)
	if err != nil {
		return nil, err
	}
	vf, err := LoadVersions(appName)
	if err != nil {
		return nil, err
	}
	vf.Versions = append(vf.Versions, VersionMeta{
		Version:     ver,
		Tag:         tag,
		GitCommit:   gitCommit,
		BuiltAt:     time.Now(),
		SizeBytes:   0, // Docker images don't have a local binary size
		IsCurrent:   false,
		DockerImage: dockerImage,
	})
	if err := saveVersions(appName, vf); err != nil {
		return nil, err
	}
	if err := PruneVersions(appName, policy, log); err != nil {
		return nil, err
	}
	return &RecordResult{Version: ver}, nil
}

// PromoteVersion marks ver as current after a successful deploy: updates
// versions.json, deployed_at, and the current → builds/vN symlink.
func PromoteVersion(appName string, ver int, deployMode string) error {
	if ver <= 0 {
		return nil
	}
	vf, err := LoadVersions(appName)
	if err != nil {
		return err
	}
	now := time.Now()
	found := false
	for i := range vf.Versions {
		vf.Versions[i].IsCurrent = vf.Versions[i].Version == ver
		if vf.Versions[i].Version == ver {
			found = true
			vf.Versions[i].DeployedAt = &now
			vf.Versions[i].DeployMode = deployMode
		}
	}
	if !found {
		return fmt.Errorf("deploy: cannot promote unknown version v%d for %q", ver, appName)
	}
	if err := saveVersions(appName, vf); err != nil {
		return err
	}
	return updateCurrentSymlink(appName, ver)
}

func updateCurrentSymlink(appName string, ver int) error {
	dir, err := appDataDir(appName)
	if err != nil {
		return err
	}
	target := filepath.Join("builds", fmt.Sprintf("v%d", ver))
	link := filepath.Join(dir, "current")
	_ = os.Remove(link)
	return os.Symlink(target, link)
}

// PruneVersions removes oldest versions beyond the retention limit. The
// version marked is_current is never deleted.
func PruneVersions(appName string, policy RetentionPolicy, log Logger) error {
	if policy == nil {
		policy = DefaultRetention{}
	}
	max := policy.MaxRetainedVersions()
	if max <= 0 {
		return nil
	}
	vf, err := LoadVersions(appName)
	if err != nil {
		return err
	}
	cur, _ := CurrentVersion(appName)
	sort.Slice(vf.Versions, func(i, j int) bool {
		return vf.Versions[i].Version < vf.Versions[j].Version
	})
	// Keep at most max versions total, but never drop cur.
	for len(vf.Versions) > max {
		removed := false
		for i, v := range vf.Versions {
			if v.IsCurrent || v.Version == cur {
				continue
			}
			if err := removeVersionArtifacts(appName, v.Version); err != nil {
				return err
			}
			if log != nil {
				log.Infof("pruned version v%d for %s", v.Version, appName)
			}
			vf.Versions = append(vf.Versions[:i], vf.Versions[i+1:]...)
			removed = true
			break
		}
		if !removed {
			break
		}
	}
	return saveVersions(appName, vf)
}

func removeVersionArtifacts(appName string, ver int) error {
	vdir, err := versionDir(appName, ver)
	if err != nil {
		return err
	}
	_ = os.RemoveAll(vdir)
	envPath, _ := envSnapshotPath(appName, ver)
	_ = os.Remove(envPath)
	return nil
}

// ListVersionsForDisplay returns metadata sorted newest-first, with a flag for
// versions that would be pruned next under the given policy.
func ListVersionsForDisplay(appName string, policy RetentionPolicy) ([]VersionMeta, error) {
	if policy == nil {
		policy = DefaultRetention{}
	}
	vf, err := LoadVersions(appName)
	if err != nil {
		return nil, err
	}
	out := append([]VersionMeta(nil), vf.Versions...)
	sort.Slice(out, func(i, j int) bool {
		return out[i].Version > out[j].Version
	})
	return out, nil
}

// WouldPruneOnNextBuild reports whether ver would be removed after one more
// successful build given retention (excluding the active version).
func WouldPruneOnNextBuild(appName string, ver int, policy RetentionPolicy) bool {
	if policy == nil {
		policy = DefaultRetention{}
	}
	max := policy.MaxRetainedVersions()
	if max <= 0 {
		return false
	}
	vf, err := LoadVersions(appName)
	if err != nil {
		return false
	}
	cur, _ := CurrentVersion(appName)
	if ver == cur {
		return false
	}
	// Simulate one more version appended.
	n := len(vf.Versions) + 1
	return n > max
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

// ParseVersionArg accepts "v3" or "3".
func ParseVersionArg(s string) (int, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	s = strings.TrimPrefix(s, "V")
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("invalid version %q (expected vN or N)", s)
	}
	return n, nil
}

// ResolveVersionOrTag accepts either a version string ("v3" or "3") or a tag
// string and returns the concrete version number. Tags are resolved by exact
// match against versions.json. An error is returned if the tag matches zero or
// more than one version (tags are not guaranteed unique).
func ResolveVersionOrTag(appName, input string) (int, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return 0, fmt.Errorf("empty version or tag")
	}

	// First try parsing as a version number.
	if ver, err := ParseVersionArg(input); err == nil {
		vf, err := LoadVersions(appName)
		if err != nil {
			return 0, err
		}
		if !versionExists(vf, ver) {
			list := formatAvailableVersions(vf)
			return 0, fmt.Errorf("version %q does not exist for %q; available: %s", input, appName, list)
		}
		return ver, nil
	}

	// Not a version number — try as a tag.
	vf, err := LoadVersions(appName)
	if err != nil {
		return 0, err
	}
	var matches []VersionMeta
	for _, v := range vf.Versions {
		if v.Tag == input {
			matches = append(matches, v)
		}
	}
	switch len(matches) {
	case 0:
		return 0, fmt.Errorf("tag %q not found for %q", input, appName)
	case 1:
		return matches[0].Version, nil
	default:
		vers := make([]string, len(matches))
		for i, m := range matches {
			vers[i] = fmt.Sprintf("v%d", m.Version)
		}
		return 0, fmt.Errorf("tag %q is ambiguous for %q — matches %s; use a version ID instead",
			input, appName, strings.Join(vers, ", "))
	}
}

// CurrentVersionMeta returns the VersionMeta for the current version, or nil
// when no version is current (e.g. app predating versioning, or build succeeded
// but deploy hasn't yet).
func CurrentVersionMeta(appName string) (*VersionMeta, error) {
	vf, err := LoadVersions(appName)
	if err != nil {
		return nil, err
	}
	for i := range vf.Versions {
		if vf.Versions[i].IsCurrent {
			return &vf.Versions[i], nil
		}
	}
	return nil, nil
}

// RecentVersions returns the N most recent versions (newest first), or all if
// n <= 0.
func RecentVersions(appName string, n int) ([]VersionMeta, error) {
	vf, err := LoadVersions(appName)
	if err != nil {
		return nil, err
	}
	out := append([]VersionMeta(nil), vf.Versions...)
	sort.Slice(out, func(i, j int) bool {
		return out[i].Version > out[j].Version
	})
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out, nil
}
