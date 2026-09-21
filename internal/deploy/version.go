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

	"github.com/abdorrahmani/phelix/internal/buildreport"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
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
	// MatrixArtifacts holds per-platform artifacts when this version was
	// built via a matrix build (phelix build --matrix or phelix dockerize --matrix).
	// When nil/empty, the version is a single-artifact build (the legacy
	// BinaryPath or DockerImage path). This field is additive — existing
	// non-matrix builds are unaffected.
	MatrixArtifacts []MatrixArtifact `json:"matrix_artifacts,omitempty"`
	// MultiArchImage is the manifest-list image reference when a matrix
	// dockerize produced a multi-arch manifest (via docker buildx). Distinct
	// from the per-combination tags in MatrixArtifacts.
	MultiArchImage string `json:"multi_arch_image,omitempty"`
	// BuildReport holds the structured build metrics (compiler, duration,
	// cache status, artifact size/platform) captured for this build. The
	// field is additive: versions recorded before build reports existed (and
	// Docker builds without native artifacts) simply leave it nil. A nil
	// report only makes that version unavailable for regression comparisons.
	BuildReport *buildreport.Report `json:"build_report,omitempty"`
}

// MatrixArtifact describes one artifact from a matrix build, corresponding
// to a single {toolchain version} × {platform} combination.
type MatrixArtifact struct {
	Platform  string `json:"platform"`            // e.g. "linux/amd64"
	Version   string `json:"toolchain_version"`   // e.g. "1.22"
	Binary    string `json:"binary,omitempty"`    // binary path (Go/Rust native builds)
	ImageTag  string `json:"image_tag,omitempty"` // per-combination Docker image tag
	Status    string `json:"status"`              // "success", "failed"
	Error     string `json:"error,omitempty"`     // error message if failed
	SizeBytes int64  `json:"size_bytes,omitempty"`

	// MatrixRunID associates the artifact with its Matrix Run
	// ("mx_YYYYMMDD_xxxx"), closing the Matrix Run → combination → artifact
	// chain (`phelix matrix show`). Additive: absent on versions recorded
	// before run IDs existed.
	MatrixRunID string `json:"matrix_run_id,omitempty"`

	// SHA256 is the checksum of the artifact's final bytes (lowercase hex),
	// or the image digest for Docker image artifacts — the integrity
	// identity carried into the release manifest. Additive: absent on
	// versions recorded before checksums existed.
	SHA256 string `json:"sha256,omitempty"`

	// Report carries this combination's own build metrics so each matrix
	// combination retains independent telemetry for regression analysis.
	// Additive: absent on older artifacts. nil = no comparable metadata.
	Report *buildreport.Report `json:"report,omitempty"`
}

// UnmarshalJSON decodes one version row while tolerating a malformed optional
// build_report payload: only that field degrades to nil instead of failing
// the whole versions.json load (which would break rollback/status/pruning).
// Malformed *core* fields still surface their normal decoding error.
func (m *VersionMeta) UnmarshalJSON(data []byte) error {
	type alias VersionMeta // strips methods, keeps field types
	aux := struct {
		*alias
		RawReport json.RawMessage `json:"build_report,omitempty"`
	}{alias: (*alias)(m)}

	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if len(aux.RawReport) > 0 && string(aux.RawReport) != "null" {
		var rep buildreport.Report
		if err := json.Unmarshal(aux.RawReport, &rep); err == nil {
			m.BuildReport = &rep
		}
		// else: leave nil — malformed optional metadata must not break loading.
	}
	return nil
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
		return nil, phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "deploy: decode versions.json")
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

// LastKnownGoodVersion returns the version automatic rollback should restore:
// the version versions.json currently marks, when it was successfully
// promoted and its binary still exists. A failed deploy never promotes
// itself, so the current pointer still names the version that was serving
// before the failed rollout — even when the failed version already reached
// some replicas. When the current record was never promoted (or its binary is
// gone), the newest other promoted version with an existing binary wins, so a
// chain of bad deploys (v11, v12 bad, v13 current-failed) resolves to the
// last genuinely good one, never to "current - 1".
func LastKnownGoodVersion(appName string) (int, error) {
	vf, err := LoadVersions(appName)
	if err != nil {
		return 0, err
	}
	promoted := func(v VersionMeta) bool { return v.DeployedAt != nil }
	usable := func(ver int) bool {
		_, _, err := VersionPaths(appName, ver)
		return err == nil
	}
	best := 0
	for _, v := range vf.Versions {
		if v.Version > best && promoted(v) && usable(v.Version) {
			best = v.Version
		}
	}
	if best == 0 {
		list := formatAvailableVersions(vf)
		return 0, phelixerr.Newf(phelixerr.CodeRollbackTargetNotFound,
			"deploy: no previous known-good version to roll back to for %q (available: %s)", appName, list)
	}
	return best, nil
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
		return 0, phelixerr.Newf(phelixerr.CodeRollbackTargetNotFound, "deploy: no previous version to roll back to for %q (current v%d)", appName, cur)
	}
	prev := cur - 1
	if !versionExists(vf, prev) {
		list := formatAvailableVersions(vf)
		return 0, phelixerr.Newf(phelixerr.CodeRollbackTargetNotFound, "deploy: version v%d is not available for %q; available: %s", prev, appName, list)
	}
	return prev, nil
}

// TagForVersion returns the tag recorded for the given version, or "" when
// the version is untagged or unknown.
func TagForVersion(appName string, version int) string {
	vf, err := LoadVersions(appName)
	if err != nil {
		return ""
	}
	for _, v := range vf.Versions {
		if v.Version == version {
			return v.Tag
		}
	}
	return ""
}

func versionExists(vf *VersionsFile, ver int) bool {
	for _, v := range vf.Versions {
		if v.Version == ver {
			return true
		}
	}
	return false
}

// DockerImageForVersion returns the image reference recorded for vN, for
// container-runtime rollback. It is the image counterpart of VersionPaths: it
// resolves the DockerImage field instead of an on-disk binary. An error is
// returned when the version does not exist or was not recorded as a Docker
// build (no image reference to run).
func DockerImageForVersion(appName string, ver int) (string, error) {
	vf, err := LoadVersions(appName)
	if err != nil {
		return "", err
	}
	for _, v := range vf.Versions {
		if v.Version == ver {
			if v.DockerImage == "" {
				return "", phelixerr.Newf(phelixerr.CodeVersionNotFound,
					"deploy: version v%d of %q has no docker image (recorded as a native build?)", ver, appName)
			}
			return v.DockerImage, nil
		}
	}
	return "", phelixerr.Newf(phelixerr.CodeVersionNotFound,
		"deploy: version v%d does not exist for %q; available: %s", ver, appName, formatAvailableVersions(vf))
}

// VersionPaths returns absolute binary and env snapshot paths for vN.
func VersionPaths(appName string, ver int) (binaryPath, envPath string, err error) {
	vf, err := LoadVersions(appName)
	if err != nil {
		return "", "", err
	}
	if !versionExists(vf, ver) {
		list := formatAvailableVersions(vf)
		return "", "", phelixerr.Newf(phelixerr.CodeVersionNotFound, "deploy: version v%d does not exist for %q; available: %s", ver, appName, list)
	}
	dir, err := versionDir(appName, ver)
	if err != nil {
		return "", "", err
	}
	binaryPath = filepath.Join(dir, "binary")
	if _, err := os.Stat(binaryPath); err != nil {
		list := formatAvailableVersions(vf)
		return "", "", phelixerr.Newf(phelixerr.CodeVersionNotFound, "deploy: binary for v%d missing for %q; available: %s", ver, appName, list)
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
// record is optional build-report telemetry captured by the caller for this
// exact build; when non-nil it is persisted alongside the version row so
// later builds can run regression analysis against it.
//
// RecordFreshBuild always creates the version with is_current=false. The caller
// must call PromoteVersion only after the deploy's health check passes. This
// two-phase approach ensures that a build which succeeds but whose deploy
// (start / health check) fails leaves the version on disk for inspection but
// never becomes the active version.
func RecordFreshBuild(appName, appID, builtBinaryPath, gitCommit, tag string, report *buildreport.Report, policy RetentionPolicy, log Logger) (*RecordResult, error) {
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
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "deploy: store binary v%d", ver)
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
	// Keep the stored report consistent with what actually landed on disk.
	if report != nil && report.Artifact.Type == buildreport.ArtifactBinary {
		report.Artifact.SizeBytes = info.Size()
	}
	vf.Versions = append(vf.Versions, VersionMeta{
		Version:     ver,
		Tag:         tag,
		GitCommit:   gitCommit,
		BuiltAt:     time.Now(),
		SizeBytes:   info.Size(),
		IsCurrent:   false,
		BuildReport: report,
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

// PromoteVersion marks ver as current after a successful deploy. Both durable
// representations are staged before either changes. If activating the symlink
// fails after versions.json commits, the previous metadata is restored, so a
// caller never observes a half-promotion caused by an ordinary write failure.
func PromoteVersion(appName string, ver int, deployMode string) error {
	if ver <= 0 {
		return nil
	}
	before, err := LoadVersions(appName)
	if err != nil {
		return err
	}
	after := cloneVersionsFile(before)
	now := time.Now()
	found := false
	for i := range after.Versions {
		after.Versions[i].IsCurrent = after.Versions[i].Version == ver
		if after.Versions[i].Version == ver {
			found = true
			after.Versions[i].DeployedAt = &now
			after.Versions[i].DeployMode = deployMode
		}
	}
	if !found {
		return phelixerr.Newf(phelixerr.CodeVersionNotFound, "deploy: cannot promote unknown version v%d for %q", ver, appName)
	}

	metadataTmp, err := stageVersions(appName, after)
	if err != nil {
		return err
	}
	defer os.Remove(metadataTmp)
	linkTmp, link, err := stageCurrentSymlink(appName, ver)
	if err != nil {
		return err
	}
	defer os.Remove(linkTmp)

	path, _ := versionsPath(appName)
	if err := os.Rename(metadataTmp, path); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "deploy: activate versions metadata for v%d", ver)
	}
	if err := os.Rename(linkTmp, link); err != nil {
		if restoreErr := saveVersions(appName, before); restoreErr != nil {
			return phelixerr.Wrapf(phelixerr.CodeFilesystem, err,
				"deploy: activate current for v%d (metadata rollback also failed: %v)", ver, restoreErr)
		}
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "deploy: activate current for v%d", ver)
	}
	return nil
}

func cloneVersionsFile(vf *VersionsFile) *VersionsFile {
	if vf == nil {
		return &VersionsFile{}
	}
	out := &VersionsFile{Versions: make([]VersionMeta, len(vf.Versions))}
	copy(out.Versions, vf.Versions)
	return out
}

func stageVersions(appName string, vf *VersionsFile) (string, error) {
	path, err := versionsPath(appName)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "deploy: create versions directory")
	}
	data, err := json.MarshalIndent(vf, "", "  ")
	if err != nil {
		return "", err
	}
	tmp := filepath.Join(filepath.Dir(path), fmt.Sprintf(".versions.tmp.%d.%d", os.Getpid(), time.Now().UnixNano()))
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "deploy: stage versions metadata")
	}
	return tmp, nil
}

func stageCurrentSymlink(appName string, ver int) (tmp, link string, err error) {
	dir, err := appDataDir(appName)
	if err != nil {
		return "", "", err
	}
	target := filepath.Join("builds", fmt.Sprintf("v%d", ver))
	link = filepath.Join(dir, "current")
	tmp = filepath.Join(dir, fmt.Sprintf(".current.tmp.%d.%d", os.Getpid(), time.Now().UnixNano()))
	if err := os.Symlink(target, tmp); err != nil {
		return "", "", phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "deploy: stage current -> %s", target)
	}
	return tmp, link, nil
}

func updateCurrentSymlink(appName string, ver int) error {
	tmp, link, err := stageCurrentSymlink(appName, ver)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if err := os.Rename(tmp, link); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "deploy: activate current for v%d", ver)
	}
	return nil
}

// PruneVersions removes oldest versions beyond the retention limit. Never
// deleted:
//   - the version marked is_current;
//   - the current version in versions.json if it differs from is_current;
//   - any version recorded as deployed by deploy.json (blue-green active
//     slot, blue-green candidate mid-rollout, rolling replicas mid-rollout —
//     all of which may still be running that binary);
//   - the version rollback targets (the one preceding the deployed version).
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
	protected := deploymentProtectedVersions(appName)
	sort.Slice(vf.Versions, func(i, j int) bool {
		return vf.Versions[i].Version < vf.Versions[j].Version
	})
	// Keep at most max versions total, but never drop a protected version.
	for len(vf.Versions) > max {
		removed := false
		for i, v := range vf.Versions {
			if v.IsCurrent || v.Version == cur || protected[v.Version] {
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

// deploymentProtectedVersions collects the versions deploy.json says are in
// active use (slot instance or replica) plus the rollback target, so pruning
// can never delete a binary that is running or is the designated rollback.
// Best-effort: a missing deploy state protects nothing.
func deploymentProtectedVersions(appName string) map[int]bool {
	protected := make(map[int]bool)
	state, err := Load(appName)
	if err != nil || state == nil {
		return protected
	}
	collect := func(m map[string]*Instance) {
		for _, inst := range m {
			if inst != nil && inst.PID > 0 && inst.Version > 0 {
				protected[inst.Version] = true
			}
		}
	}
	collect(state.Slots)
	collect(state.Replicas)
	if state.ActiveVersion > 0 {
		protected[state.ActiveVersion] = true
	}
	// Rollback target: the highest version below the deployed one.
	if deployed := state.ActiveVersion; deployed > 0 {
		rollback := 0
		for _, v := range vfList(appName) {
			if v.Version < deployed && v.Version > rollback {
				rollback = v.Version
			}
		}
		if rollback > 0 {
			protected[rollback] = true
		}
	}
	return protected
}

// vfList returns versions.json rows (empty when unreadable).
func vfList(appName string) []VersionMeta {
	vf, err := LoadVersions(appName)
	if err != nil || vf == nil {
		return nil
	}
	return vf.Versions
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
		return 0, phelixerr.Newf(phelixerr.CodeInvalidArgument, "invalid version %q (expected vN or N)", s)
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
		return 0, phelixerr.New(phelixerr.CodeInvalidArgument, "empty version or tag")
	}

	// First try parsing as a version number.
	if ver, err := ParseVersionArg(input); err == nil {
		vf, err := LoadVersions(appName)
		if err != nil {
			return 0, err
		}
		if !versionExists(vf, ver) {
			list := formatAvailableVersions(vf)
			return 0, phelixerr.Newf(phelixerr.CodeVersionNotFound, "version %q does not exist for %q; available: %s", input, appName, list)
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
		return 0, phelixerr.Newf(phelixerr.CodeVersionNotFound, "tag %q not found for %q", input, appName)
	case 1:
		return matches[0].Version, nil
	default:
		vers := make([]string, len(matches))
		for i, m := range matches {
			vers[i] = fmt.Sprintf("v%d", m.Version)
		}
		return 0, phelixerr.Newf(phelixerr.CodeVersionNotFound, "tag %q is ambiguous for %q — matches %s; use a version ID instead",
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

// BuildReportHistory returns previous builds' report metadata for appName,
// newest first, excluding excludeVersion (the just-recorded current build).
//
// Matrix versions contribute one entry per successful artifact, each carrying
// its own per-combination report, so combinations only ever compare against
// matching toolchain/platform contexts. Versions whose metadata predates
// build reports — or Docker-only builds — yield entries with a nil Report;
// those are unavailable for regression comparison but never break it.
func BuildReportHistory(appName string, excludeVersion int) ([]buildreport.HistoryEntry, error) {
	vf, err := LoadVersions(appName)
	if err != nil {
		return nil, err
	}
	out := append([]VersionMeta(nil), vf.Versions...)
	sort.Slice(out, func(i, j int) bool {
		return out[i].Version > out[j].Version
	})

	history := make([]buildreport.HistoryEntry, 0, len(out))
	for _, v := range out {
		if v.Version == excludeVersion {
			continue
		}
		switch {
		case v.BuildReport != nil:
			history = append(history, buildreport.HistoryEntry{
				Version: v.Version,
				Tag:     v.Tag,
				BuiltAt: v.BuiltAt,
				Report:  v.BuildReport,
			})
		case len(v.MatrixArtifacts) > 0:
			for _, art := range v.MatrixArtifacts {
				if art.Status != "success" || art.Report == nil {
					continue
				}
				entry := buildreport.HistoryEntry{
					Version: v.Version,
					Tag:     v.Tag,
					BuiltAt: v.BuiltAt,
					Report:  art.Report,
				}
				history = append(history, entry)
			}
		default:
			// Keep a metadata-less placeholder so analysis can report how
			// many history rows lack comparable data.
			history = append(history, buildreport.HistoryEntry{
				Version: v.Version,
				Tag:     v.Tag,
				BuiltAt: v.BuiltAt,
				Report:  nil,
			})
		}
	}
	return history, nil
}

// RecordMatrixBuild records a matrix build as a new version entry containing
// multiple per-platform artifacts. The version is created with is_current=false
// (same two-phase protocol as RecordFreshBuild / RecordDockerBuild).
//
// artifacts should only include successfully-built combinations. Failed
// combinations are recorded in the report.json but NOT in versions.json,
// since versions.json is the deploy source of truth.
func RecordMatrixBuild(appName, tag, gitCommit string, artifacts []MatrixArtifact, multiArchImage string, policy RetentionPolicy, log Logger) (*RecordResult, error) {
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
		Version:         ver,
		Tag:             tag,
		GitCommit:       gitCommit,
		BuiltAt:         time.Now(),
		IsCurrent:       false,
		MatrixArtifacts: artifacts,
		MultiArchImage:  multiArchImage,
	})
	if err := saveVersions(appName, vf); err != nil {
		return nil, err
	}
	if err := PruneVersions(appName, policy, log); err != nil {
		return nil, err
	}
	return &RecordResult{Version: ver}, nil
}
