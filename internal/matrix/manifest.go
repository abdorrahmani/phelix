package matrix

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// Matrix Release Manifest: the release-set view of one Matrix Run.
//
// A Matrix Build is ONE logical build/release of one application version that
// happens to produce multiple artifacts (one per combination). The manifest
// describes that artifact set — it is the durable answer to "what exactly was
// released by run mx_… under version vN, and how can each artifact be
// verified?". Combinations are artifacts of the release, never independent
// application versions:
//
//	Application
//	    └── Version vN (one versions.json row)
//	            └── Matrix Run mx_…
//	                    └── Artifacts (combination × 1) ← this manifest
//
// One manifest file is stored per run next to the run record
// (<runs dir>/<run-id>.manifest.json). It is regenerated (overwritten) when a
// resumed run reaches a new final state, so it always describes the run's
// final artifact set.

// Release manifest statuses. A manifest exists only for terminal runs that
// produced at least one artifact; its status makes completeness explicit so a
// partial matrix can never masquerade as a complete release.
const (
	// ReleaseStatusComplete: every combination in the run succeeded.
	ReleaseStatusComplete = "complete"
	// ReleaseStatusPartial: some combinations succeeded and some failed — the
	// manifest lists only the successful artifacts, and says so.
	ReleaseStatusPartial = "partial"
)

// manifestSchemaVersion is the current ReleaseManifest schema version.
// Consumers should reject manifests with a higher version than they know.
const manifestSchemaVersion = 1

// ReleaseManifest describes the complete artifact set produced by one Matrix
// Run, recorded under one logical application version.
type ReleaseManifest struct {
	SchemaVersion     int       `json:"manifest_version"`
	AppName           string    `json:"app"`
	Version           int       `json:"version"` // logical application version (versions.json vN)
	Tag               string    `json:"tag,omitempty"`
	MatrixRunID       RunID     `json:"matrix_run_id"`
	CreatedAt         time.Time `json:"created_at"`
	Status            string    `json:"status"`             // "complete" or "partial"
	TotalCombinations int       `json:"total_combinations"` // combinations in the run, successful or not
	// Configuration snapshot summary (the run's recorded effective config).
	Language          string             `json:"language,omitempty"`
	ToolchainVersions []string           `json:"toolchain_versions,omitempty"`
	Platforms         []string           `json:"platforms,omitempty"`
	Artifacts         []ManifestArtifact `json:"artifacts"`
}

// ManifestArtifact is one artifact entry in a ReleaseManifest — the
// combination identity plus enough information to identify and verify the
// artifact bytes.
type ManifestArtifact struct {
	CombinationID string `json:"combination_id"` // e.g. "go1.27-linux-amd64"
	Identity      string `json:"identity"`       // run-scoped: "<runID>/<combinationID>"
	Toolchain     string `json:"toolchain"`      // "go" or "rust"
	Version       string `json:"toolchain_version"`
	Platform      string `json:"platform"` // "os/arch[/variant]"
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	Variant       string `json:"variant,omitempty"`
	Artifact      string `json:"artifact"` // binary path or image reference
	SizeBytes     int64  `json:"size_bytes,omitempty"`
	SHA256        string `json:"sha256,omitempty"`
}

// BuildReleaseManifest assembles the manifest of a terminal run's artifact
// set. Rules:
//
//   - only terminal runs with at least one successful combination describe a
//     release: status succeeded → "complete", partial → "partial"; anything
//     else (running, interrupted, failed) is an error — an unfinished or
//     empty run must not gain a release manifest;
//   - only combinations whose final status is success AND that produced an
//     artifact are listed (a success without an artifact is a data bug and is
//     skipped rather than represented by a phantom entry);
//   - entries are deduplicated by combination ID and sorted by it, so the
//     same run and artifact set always yield the same manifest ordering —
//     never Go map or completion order.
//
// version is the logical application version the run's artifacts were
// recorded under (versions.json vN, from RecordMatrixBuild).
func BuildReleaseManifest(run *Run, version int, tag string, createdAt time.Time) (*ReleaseManifest, error) {
	if run == nil {
		return nil, phelixerr.New(phelixerr.CodeInvalidArgument, "matrix: cannot build a release manifest for a nil run")
	}
	if version <= 0 {
		return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"matrix run %s was not recorded as an application version — no release manifest", run.ID)
	}

	var status string
	switch run.Status {
	case RunStatusSucceeded:
		status = ReleaseStatusComplete
	case RunStatusPartial:
		status = ReleaseStatusPartial
	default:
		return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"matrix run %s has no release manifest (status: %s) — only finished runs with successful artifacts form a release",
			run.ID, run.Status)
	}

	m := &ReleaseManifest{
		SchemaVersion:     manifestSchemaVersion,
		AppName:           run.AppName,
		Version:           version,
		Tag:               tag,
		MatrixRunID:       run.ID,
		CreatedAt:         createdAt,
		Status:            status,
		TotalCombinations: len(run.Combinations),
		Language:          run.Config.Lang,
		ToolchainVersions: append([]string(nil), run.Config.Versions...),
		Platforms:         append([]string(nil), run.Config.Platforms...),
		Artifacts:         make([]ManifestArtifact, 0, len(run.Combinations)),
	}

	seen := make(map[string]bool, len(run.Combinations))
	for _, rc := range run.Combinations {
		if rc.Status != "success" || rc.Artifact == "" || seen[rc.ID] {
			continue
		}
		seen[rc.ID] = true
		m.Artifacts = append(m.Artifacts, ManifestArtifact{
			CombinationID: rc.ID,
			Identity:      rc.Identity,
			Toolchain:     rc.Toolchain,
			Version:       rc.Version,
			Platform:      rc.Platform,
			OS:            rc.OS,
			Arch:          rc.Arch,
			Variant:       rc.Variant,
			Artifact:      rc.Artifact,
			SizeBytes:     fileSizeOf(rc.Artifact),
			SHA256:        rc.SHA256,
		})
	}
	if len(m.Artifacts) == 0 {
		return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"matrix run %s produced no verifiable artifacts — no release manifest", run.ID)
	}
	sort.Slice(m.Artifacts, func(i, j int) bool {
		return m.Artifacts[i].CombinationID < m.Artifacts[j].CombinationID
	})
	return m, nil
}

// fileSizeOf stats an artifact path; image references and missing files yield
// 0 (the size field is omitted from the manifest for them).
func fileSizeOf(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// ManifestPath returns the per-run manifest location:
// <runs dir>/<run-id>.manifest.json — discoverable next to the run record it
// belongs to, and honoring PHELIX_DATA_DIR like all run history.
func ManifestPath(id RunID) (string, error) {
	if _, err := ParseRunID(string(id)); err != nil {
		return "", err
	}
	dir, err := RunsDir()
	if err != nil {
		return "", err
	}
	return dir + string(filepath.Separator) + string(id) + ".manifest.json", nil
}

// SaveManifest writes the manifest atomically (temp file + rename). Unlike
// run records, manifests may be overwritten: a resumed run regenerates its
// manifest to describe the final artifact set, and the last write is the
// authoritative one.
func SaveManifest(m *ReleaseManifest) error {
	if m == nil {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "matrix: cannot save a nil release manifest")
	}
	if _, err := ParseRunID(string(m.MatrixRunID)); err != nil {
		return err
	}
	if m.Status != ReleaseStatusComplete && m.Status != ReleaseStatusPartial {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"matrix: refusing to save a release manifest with status %q", m.Status)
	}
	if m.Version <= 0 {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"matrix: refusing to save a release manifest for %s without an application version", m.MatrixRunID)
	}
	path, err := ManifestPath(m.MatrixRunID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "create matrix runs directory")
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeConfiguration, "encode release manifest", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "write release manifest for %s", m.MatrixRunID)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "replace release manifest for %s", m.MatrixRunID)
	}
	return nil
}

// LoadManifest reads one run's release manifest. A missing manifest is a clean
// CodeNotFound (runs without artifacts, or recorded before manifests existed,
// simply have none); a malformed one is a configuration error.
func LoadManifest(id RunID) (*ReleaseManifest, error) {
	path, err := ManifestPath(id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, phelixerr.Newf(phelixerr.CodeNotFound,
				"no release manifest for matrix run %s", id)
		}
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "read release manifest for %s", id)
	}
	var m ReleaseManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "release manifest for %s is malformed", id)
	}
	if m.MatrixRunID != id {
		return nil, phelixerr.Newf(phelixerr.CodeConfiguration,
			"release manifest for %s is malformed: it claims run %s", id, m.MatrixRunID)
	}
	return &m, nil
}
