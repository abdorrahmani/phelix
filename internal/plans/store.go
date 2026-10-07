package plans

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/server"
)

// maxPlanBytes bounds a persisted plan. Plans describe execution — they must
// never become a dumping ground for config blobs, environment dumps or
// embedded source.
const maxPlanBytes = 64 * 1024

// Dir returns the plan store directory under the data dir.
func Dir() string {
	return filepath.Join(server.DataDir(), "plans")
}

// planPath returns the on-disk path for a plan ID after validating the ID,
// so an ID can never traverse the filesystem.
func planPath(id string) (string, error) {
	if !ValidateID(id) {
		return "", phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"invalid plan id %q: expected %s<16 hex chars>", id, IDPrefix)
	}
	return filepath.Join(Dir(), id+".json"), nil
}

// Save persists a plan exactly once. The write is atomic (tmp + fsync +
// rename) and create-only (os.Link on the final path fails if it exists), so
// a persisted plan can never be silently replaced — plans are immutable by
// construction, not by convention.
func Save(p *Plan) error {
	if p == nil {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "nil plan")
	}
	if p.Status == "" {
		return phelixerr.New(phelixerr.CodePlanInvalid, "plan has no status")
	}
	if err := p.VerifyHash(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodePlanInvalid, "encode plan", err)
	}
	if len(data) > maxPlanBytes {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"plan %s is %d bytes; the maximum is %d — plans describe execution, not system state", p.PlanID, len(data), maxPlanBytes)
	}

	dir := Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "create plan store directory", err)
	}
	final, err := planPath(p.PlanID)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(final); err == nil {
		return phelixerr.Newf(phelixerr.CodeAlreadyExists, "plan %s already exists — plans are immutable; create a new plan instead", p.PlanID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "stat plan", err)
	}

	tmp := fmt.Sprintf("%s.tmp-%d", final, os.Getpid())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "create plan temp file", err)
	}
	cleanup := func() { _ = f.Close(); _ = os.Remove(tmp) }
	if _, err := f.Write(data); err != nil {
		cleanup()
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "write plan", err)
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "sync plan", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "close plan", err)
	}
	// os.Link is atomic create-only: if another writer persisted the same
	// plan ID first, this fails instead of overwriting it.
	if err := os.Link(tmp, final); err != nil {
		_ = os.Remove(tmp)
		if errors.Is(err, os.ErrExist) {
			return phelixerr.Newf(phelixerr.CodeAlreadyExists, "plan %s already exists — plans are immutable; create a new plan instead", p.PlanID)
		}
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "persist plan", err)
	}
	_ = os.Remove(tmp)
	d, err := os.Open(dir)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "open plan store directory", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "sync plan store directory", err)
	}
	return nil
}

// Load reads a plan by ID and fails closed on any integrity problem:
// unparseable content is PLAN_CORRUPT, a foreign schema version or malformed
// identity is PLAN_INVALID, and content that no longer hashes to its stored
// value is PLAN_HASH_MISMATCH. Only a fully verified plan comes back.
func Load(id string) (*Plan, error) {
	path, err := planPath(id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, phelixerr.Newf(phelixerr.CodeNotFound, "no plan %s", id)
	}
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "read plan", err)
	}
	return Decode(data)
}

// Decode parses and fully verifies plan bytes (schema, identity, hash).
func Decode(data []byte) (*Plan, error) {
	var p Plan
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodePlanCorrupt, "plan content is not a parseable plan", err)
	}
	if p.SchemaVersion != SchemaVersion {
		return nil, phelixerr.Newf(phelixerr.CodePlanInvalid,
			"plan schema_version %q, want %q — recreate the plan", p.SchemaVersion, SchemaVersion)
	}
	if !ValidateID(p.PlanID) {
		return nil, phelixerr.Newf(phelixerr.CodePlanInvalid, "plan has malformed id %q", p.PlanID)
	}
	switch p.Status {
	case StatusCreated, StatusApplied, StatusFailed:
	default:
		return nil, phelixerr.Newf(phelixerr.CodePlanInvalid, "plan has unknown status %q", p.Status)
	}
	if err := p.VerifyHash(); err != nil {
		return nil, err
	}
	return &p, nil
}

// MarkApplied records the operation a successful application produced. Only
// the status and the correlation metadata change — the semantic content is
// untouched, which the hash (recomputed over the same fields) proves.
func MarkApplied(id, operationID, deploymentID string) (*Plan, error) {
	return transition(id, StatusApplied, func(p *Plan) {
		p.Correlation = &Correlation{
			OperationID:  operationID,
			DeploymentID: deploymentID,
			AppliedAtMs:  time.Now().UnixMilli(),
		}
	})
}

// MarkFailed records that an application attempt failed. The plan stays
// inspectable; retry semantics are documented at the CLI layer.
func MarkFailed(id string) (*Plan, error) {
	return transition(id, StatusFailed, nil)
}

// transition loads a verified plan, applies the status change and persists
// it. The semantic content is hash-verified before and after, so a transition
// can never smuggle in an execution change.
func transition(id, status string, mutate func(*Plan)) (*Plan, error) {
	p, err := Load(id)
	if err != nil {
		return nil, err
	}
	before, hashErr := p.Hash()
	if hashErr != nil {
		return nil, hashErr
	}
	p.Status = status
	if mutate != nil {
		mutate(p)
	}
	after, hashErr := p.Hash()
	if hashErr != nil {
		return nil, hashErr
	}
	if before != after {
		return nil, phelixerr.New(phelixerr.CodePlanHashMismatch,
			"plan transition would change semantic content — refusing")
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodePlanInvalid, "encode plan", err)
	}
	path, err := planPath(p.PlanID)
	if err != nil {
		return nil, err
	}
	if err := atomicReplace(path, data); err != nil {
		return nil, err
	}
	return p, nil
}

// atomicReplace overwrites an existing plan file (status transitions only)
// with the same fsync discipline as creation.
func atomicReplace(path string, data []byte) error {
	tmp := fmt.Sprintf("%s.tmp-%d", path, os.Getpid())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "create plan temp file", err)
	}
	cleanup := func() { _ = f.Close(); _ = os.Remove(tmp) }
	if _, err := f.Write(data); err != nil {
		cleanup()
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "write plan", err)
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "sync plan", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "close plan", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "replace plan", err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "open plan store directory", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "sync plan store directory", err)
	}
	return nil
}

// List returns plans newest-first, optionally filtered by application name.
// Corrupt or tampered files are skipped (and counted) rather than failing the
// whole listing — a query path must stay readable; `plan show` still fails
// closed for the specific ID. limit <= 0 returns everything.
func List(app string, limit int) ([]*Plan, int, error) {
	entries, err := os.ReadDir(Dir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, phelixerr.Wrap(phelixerr.CodeFilesystem, "read plan store directory", err)
	}
	type entry struct {
		p    *Plan
		when int64
	}
	var found []entry
	skipped := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" || strings.Contains(e.Name(), ".tmp-") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(Dir(), e.Name()))
		if err != nil {
			skipped++
			continue
		}
		p, err := Decode(data)
		if err != nil {
			skipped++
			continue
		}
		if app != "" && p.Action.Application != app {
			continue
		}
		found = append(found, entry{p, p.CreatedAt})
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].when > found[j].when })
	if limit > 0 && len(found) > limit {
		found = found[:limit]
	}
	out := make([]*Plan, 0, len(found))
	for _, e := range found {
		out = append(out, e.p)
	}
	return out, skipped, nil
}
