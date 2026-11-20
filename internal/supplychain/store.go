package supplychain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

func reportKey(r *Report) string { return "v" + strconv.Itoa(r.Version) + "-" + r.Artifact.SHA256 }

func (s *Store) Save(r *Report) error {
	if err := ValidateReport(r); err != nil {
		return err
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
	var out []*Report
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			r, err := s.Load(appID, strings.TrimSuffix(entry.Name(), ".json"))
			if err != nil {
				return nil, err
			}
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Version != out[j].Version {
			return out[i].Version > out[j].Version
		}
		return out[i].BuiltAt.After(out[j].BuiltAt)
	})
	if offset >= len(out) {
		return []*Report{}, nil
	}
	end := offset + limit
	if end > len(out) {
		end = len(out)
	}
	return out[offset:end], nil
}

func ValidateReport(r *Report) error {
	if r == nil || r.SchemaVersion != 1 || !identityPattern.MatchString(r.AppID) || !identityPattern.MatchString(r.AppName) || r.Version < 0 || r.BuiltAt.IsZero() {
		return errors.New("invalid security report identity")
	}
	if !digestPattern.MatchString(r.Artifact.SHA256) || r.Artifact.SizeBytes < 0 || r.Artifact.Filename == "" || filepath.Base(r.Artifact.Filename) != r.Artifact.Filename {
		return errors.New("invalid artifact identity")
	}
	if r.Inventory == nil || len(r.Inventory.Modules) > 100000 || len(r.Inventory.Edges) > 500000 {
		return errors.New("missing or oversized inventory")
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
