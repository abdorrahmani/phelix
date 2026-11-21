package supplychain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/abdorrahmani/phelix/internal/security"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var identityPattern = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,127}$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type Store struct{ DataDir string }

func NewStore(dataDir string) *Store { return &Store{DataDir: dataDir} }

func SHA256File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if !stat.Mode().IsRegular() {
		return "", 0, errors.New("artifact must be a regular file")
	}
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

func Digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func (s *Store) root() string { return filepath.Join(s.DataDir, "supply-chain") }

func atomicWrite(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".security-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errors.New("security document exceeds size limit")
	}
	return raw, nil
}

func (s *Store) PutDocument(format string, raw []byte) (Document, error) {
	if len(raw) > MaxDocumentBytes || !json.Valid(raw) {
		return Document{}, errors.New("security document must be bounded valid JSON")
	}
	d := Document{Format: format, SHA256: Digest(raw), SizeBytes: int64(len(raw))}
	if err := atomicWrite(filepath.Join(s.root(), "documents", d.SHA256+".json"), raw); err != nil {
		return Document{}, err
	}
	return d, nil
}

func (s *Store) Document(digest string) ([]byte, error) {
	if !digestPattern.MatchString(digest) {
		return nil, errors.New("invalid document digest")
	}
	raw, err := readBounded(filepath.Join(s.root(), "documents", digest+".json"), MaxDocumentBytes)
	if err != nil {
		return nil, err
	}
	if Digest(raw) != digest {
		return nil, errors.New("security document integrity verification failed")
	}
	return raw, nil
}

func reportKey(r *Report) string {
	key := "v" + strconv.Itoa(r.Version) + "-" + r.Artifact.SHA256
	if r.BuildID != "" {
		key += "-" + Digest([]byte(r.BuildID))[:32]
	}
	return key
}

func (s *Store) Save(r *Report) error {
	if err := ValidateReport(r); err != nil {
		return err
	}
	lock, err := security.NewScanStore(s.DataDir).AcquireConfigLock("artifact-" + Digest([]byte(r.AppID+"/"+Reference(r))))
	if err != nil {
		return err
	}
	defer lock.Release()
	existing, loadErr := s.Load(r.AppID, Reference(r))
	if loadErr == nil {
		if existing.Provenance.SHA256 != r.Provenance.SHA256 || len(existing.SBOMs) != len(r.SBOMs) {
			return errors.New("artifact build documents are immutable")
		}
		for i := range r.SBOMs {
			if existing.SBOMs[i].SHA256 != r.SBOMs[i].SHA256 {
				return errors.New("artifact SBOM documents are immutable")
			}
		}
		if r.Signature.Status == "missing" && len(r.Signature.Bundles) == 0 && len(existing.Signature.Bundles) > 0 {
			r.Signature = existing.Signature
		}
		if existing.Scan != nil && (r.Scan == nil || (existing.Scan.FinishedAt != nil && r.Scan.FinishedAt != nil && existing.Scan.FinishedAt.After(*r.Scan.FinishedAt))) {
			r.Scan = existing.Scan
		}
	} else if !os.IsNotExist(loadErr) {
		return loadErr
	}
	for digest, raw := range r.Documents {
		if Digest(raw) != digest {
			return errors.New("document digest does not match content")
		}
		if _, err := s.PutDocument("", raw); err != nil {
			return err
		}
	}
	copy := *r
	copy.Documents = nil
	raw, err := json.Marshal(copy)
	if err != nil {
		return err
	}
	if len(raw) > MaxReportBytes {
		return errors.New("security report exceeds size limit")
	}
	return atomicWrite(filepath.Join(s.root(), "reports", r.AppID, reportKey(r)+".json"), raw)
}

func (s *Store) Load(appID, reference string) (*Report, error) {
	if !identityPattern.MatchString(appID) || !identityPattern.MatchString(reference) {
		return nil, errors.New("invalid security report identity")
	}
	raw, err := readBounded(filepath.Join(s.root(), "reports", appID, reference+".json"), MaxReportBytes)
	if err != nil {
		return nil, err
	}
	var r Report
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	if err := ValidateReport(&r); err != nil {
		return nil, err
	}
	if r.AppID != appID || reportKey(&r) != reference {
		return nil, errors.New("security report belongs to a different artifact")
	}
	return &r, nil
}

func (s *Store) List(appID string, limit, offset int) ([]*Report, error) {
	if !identityPattern.MatchString(appID) || offset < 0 {
		return nil, errors.New("invalid security report query")
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	entries, err := os.ReadDir(filepath.Join(s.root(), "reports", appID))
	if os.IsNotExist(err) {
		return []*Report{}, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, strings.TrimSuffix(entry.Name(), ".json"))
		}
	}
	sort.Slice(names, func(i, j int) bool {
		vi, _ := strconv.Atoi(strings.SplitN(strings.TrimPrefix(names[i], "v"), "-", 2)[0])
		vj, _ := strconv.Atoi(strings.SplitN(strings.TrimPrefix(names[j], "v"), "-", 2)[0])
		if vi != vj {
			return vi > vj
		}
		return names[i] < names[j]
	})
	if offset >= len(names) {
		return []*Report{}, nil
	}
	end := offset + limit
	if end > len(names) {
		end = len(names)
	}
	out := make([]*Report, 0, end-offset)
	for _, name := range names[offset:end] {
		r, err := s.Load(appID, name)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func ValidateReport(r *Report) error {
	if r == nil || r.SchemaVersion != 1 || !identityPattern.MatchString(r.AppID) || !identityPattern.MatchString(r.AppName) || r.Version < 0 || r.BuiltAt.IsZero() {
		return errors.New("invalid security report identity")
	}
	if r.BuildID != "" && !identityPattern.MatchString(r.BuildID) {
		return errors.New("invalid build combination identity")
	}
	if !digestPattern.MatchString(r.Artifact.SHA256) || r.Artifact.SizeBytes < 0 || r.Artifact.Filename == "" || filepath.Base(r.Artifact.Filename) != r.Artifact.Filename {
		return errors.New("invalid artifact identity")
	}
	if r.Inventory == nil || len(r.Inventory.Modules) > 100000 || len(r.Inventory.Edges) > 500000 {
		return errors.New("missing or oversized inventory")
	}
	formats := map[string]bool{}
	for _, d := range r.SBOMs {
		if (d.Format != "spdx-2.3" && d.Format != "cyclonedx-1.6") || formats[d.Format] {
			return errors.New("invalid or duplicate SBOM format")
		}
		formats[d.Format] = true
	}
	if r.Provenance.SHA256 != "" && r.Provenance.Format != "slsa-provenance-v1" {
		return errors.New("unsupported provenance format")
	}
	nodes := map[string]bool{}
	for _, root := range r.Inventory.Roots {
		nodes[root] = true
	}
	for _, m := range r.Inventory.Modules {
		if m.Name == "" || m.Key == "" || len(m.Name) > 512 || len(m.Version) > 256 || nodes[m.Key] {
			return errors.New("invalid or duplicate dependency identity")
		}
		nodes[m.Key] = true
	}
	for _, e := range r.Inventory.Edges {
		if !nodes[e.From] || !nodes[e.To] {
			return errors.New("dependency graph refers to a missing package")
		}
	}
	for key, hash := range r.PackageHashes {
		if !nodes[key] || !digestPattern.MatchString(hash) {
			return errors.New("invalid dependency checksum")
		}
	}
	refs := append([]Document{}, r.SBOMs...)
	if r.Provenance.SHA256 != "" {
		refs = append(refs, r.Provenance)
	}
	refs = append(refs, r.Signature.Bundles...)
	for _, d := range refs {
		if !digestPattern.MatchString(d.SHA256) || d.SizeBytes < 0 || d.SizeBytes > MaxDocumentBytes {
			return fmt.Errorf("invalid %s document reference", d.Format)
		}
	}
	return ValidatePolicy(r.Policy)
}

func (s *Store) LoadPolicy(appID string) (Policy, error) {
	p := DefaultPolicy()
	if !identityPattern.MatchString(appID) {
		return p, errors.New("invalid application identity")
	}
	raw, err := readBounded(filepath.Join(s.root(), "policies", appID+".json"), 64<<10)
	if os.IsNotExist(err) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	if err = json.Unmarshal(raw, &p); err != nil {
		return p, err
	}
	return p, ValidatePolicy(p)
}

func (s *Store) SavePolicy(appID string, p Policy) error {
	if !identityPattern.MatchString(appID) {
		return errors.New("invalid application identity")
	}
	if err := ValidatePolicy(p); err != nil {
		return err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(s.root(), "policies", appID+".json"), raw)
}

func (s *Store) Count(appID string) (int, error) {
	if !identityPattern.MatchString(appID) {
		return 0, errors.New("invalid application identity")
	}
	entries, err := os.ReadDir(filepath.Join(s.root(), "reports", appID))
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			count++
		}
	}
	return count, nil
}

func (s *Store) ForVersion(appID string, version int, digest string) (*Report, error) {
	return s.ForBuild(appID, version, digest, "")
}

func (s *Store) ForBuild(appID string, version int, digest, buildID string) (*Report, error) {
	if !identityPattern.MatchString(appID) || version < 1 || (digest != "" && !digestPattern.MatchString(digest)) || (buildID != "" && !identityPattern.MatchString(buildID)) {
		return nil, errors.New("invalid artifact selection")
	}
	if digest != "" && buildID != "" {
		r, err := s.Load(appID, reportKey(&Report{Version: version, Artifact: Artifact{SHA256: digest}, BuildID: buildID}))
		if err == nil && r.BuildID != buildID {
			return nil, errors.New("report belongs to a different build combination")
		}
		return r, err
	}
	entries, err := os.ReadDir(filepath.Join(s.root(), "reports", appID))
	if err != nil {
		return nil, err
	}
	prefix := "v" + strconv.Itoa(version) + "-"
	name := ""
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), prefix) && strings.HasSuffix(e.Name(), ".json") {
			if digest != "" && !strings.HasPrefix(e.Name(), prefix+digest) {
				continue
			}
			if buildID != "" && !strings.HasSuffix(e.Name(), "-"+Digest([]byte(buildID))[:32]+".json") {
				continue
			}
			if name != "" {
				return nil, errors.New("version has multiple build artifacts; specify its SHA-256 digest and build ID")
			}
			name = strings.TrimSuffix(e.Name(), ".json")
		}
	}
	if name == "" {
		return nil, os.ErrNotExist
	}
	r, err := s.Load(appID, name)
	if err == nil && buildID != "" && r.BuildID != buildID {
		return nil, errors.New("report belongs to a different build combination")
	}
	return r, err
}
