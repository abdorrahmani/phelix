// Package ops provides Phelix's durable operation records: one small,
// queryable record per mutation operation (rebuild, rollback, build) that
// gives an operation identity a concrete, restart-safe home on disk.
//
// Records follow the persistence discipline of the remote-command idempotency
// ledgers (internal/grpc/rollback_command.go): atomic temp file + fsync +
// rename + directory fsync. Record writes are best-effort from the command's
// point of view — a failed record write must never break a deploy — while
// the request-key ledger in ledger.go is fail-closed because idempotency
// claims must be durable to be true.
//
// Existing durable identifiers are reused wherever they already represent the
// operation: a rebuild/deploy record correlates its dep-… deployment ID, a
// matrix build keeps its mx_ run ID as the operation identity (no second ID
// is minted), and a webhook execution keeps its wh_ job ID. The op_… record
// covers the classic paths that previously had nothing queryable.
package ops

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/server"
)

// SchemaVersion is the ops record schema version.
const SchemaVersion = "1"

// Operation kinds. Kind names what the operation does; the external status
// vocabulary (machine.Status*) names how it went.
const (
	KindRebuild  = "rebuild"
	KindRollback = "rollback"
	KindBuild    = "build"
)

// IDPrefix — operation IDs are op_ + 16 hex chars (8 crypto/rand bytes),
// matching the webhook job ID discipline.
const IDPrefix = "op_"

var idPattern = regexp.MustCompile(`^op_[0-9a-f]{16}$`)

// Record is one durable operation record.
type Record struct {
	SchemaVersion string `json:"schema_version"`
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	App           string `json:"app"`
	// Status uses the external lifecycle vocabulary (machine.Status*), so the
	// record is stable even as internal engines evolve.
	Status string `json:"status"`
	// RequestKey records the idempotency key that initiated the operation,
	// when one was supplied.
	RequestKey string `json:"request_key,omitempty"`
	// Actor is provenance metadata, not authenticated identity (Phase 1 has
	// no authenticated actor model).
	Actor *machine.Actor `json:"actor,omitempty"`
	// DeploymentID correlates the record with the deployment telemetry stream
	// (dep-…) so an agent can join the operation to its event timeline.
	DeploymentID string `json:"deployment_id,omitempty"`
	// PlanID/PlanHash correlate the operation with the Phase 3 plan that
	// produced it (empty for direct command invocations). Additive metadata:
	// the operation model is unchanged.
	PlanID   string `json:"plan_id,omitempty"`
	PlanHash string `json:"plan_hash,omitempty"`
	// AuthzDecisionID/ApprovalID correlate the operation with the Phase 4
	// authorization decision that permitted it, and with the approval that
	// satisfied it when one was required. Both are empty on a host that does
	// not enforce authorization, which keeps every pre-Phase-4 record valid.
	// They point BACKWARD on purpose: the decision is made before the
	// operation record exists, so a denied execution can never leave an
	// operation behind to point forward from.
	AuthzDecisionID string `json:"authz_decision_id,omitempty"`
	ApprovalID      string `json:"approval_id,omitempty"`
	// PID of the CLI process that owned the operation. A record stuck in
	// running with a dead PID was interrupted by a crash; the status is left
	// untouched (the outcome is unknown, and honesty beats guessing).
	PID        int          `json:"pid,omitempty"`
	CreatedAt  int64        `json:"created_at_ms"`
	UpdatedAt  int64        `json:"updated_at_ms"`
	FinishedAt int64        `json:"finished_at_ms,omitempty"`
	Result     *Result      `json:"result,omitempty"`
	Error      *RecordError `json:"error,omitempty"`
}

// Result carries the operation's terminal outcome fields. Zero values are
// omitted; extra kind-specific detail stays in the owning subsystem (versions,
// matrix runs, webhook jobs) and is correlated by ID.
type Result struct {
	Version  int    `json:"version,omitempty"`  // deployed/built version (vN)
	Port     int    `json:"port,omitempty"`     // public port after deploy
	Strategy string `json:"strategy,omitempty"` // classic | blue-green | rolling | canary | progressive
	// Rollback outcomes:
	FromVersion  int    `json:"from_version,omitempty"`
	Verification string `json:"verification,omitempty"` // passed | failed | cancelled
}

// RecordError is the structured failure captured on the record. The message
// is redacted at write time so the record can never carry a secret forward.
type RecordError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Dir returns the operation-record directory under the data dir.
func Dir() string {
	return filepath.Join(server.DataDir(), "ops")
}

// NewID mints a new op_ operation ID.
func NewID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeFilesystem, "generate operation id", err)
	}
	return IDPrefix + hex.EncodeToString(buf), nil
}

// Begin creates and persists a pending operation record. It is best-effort by
// design: a record-write failure returns an error and the command proceeds
// without an operation ID rather than failing a real mutation.
func Begin(kind, app, requestKey string) (*Record, error) {
	id, err := NewID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	rec := &Record{
		SchemaVersion: SchemaVersion,
		ID:            id,
		Kind:          kind,
		App:           app,
		Status:        machine.StatusPending,
		RequestKey:    requestKey,
		Actor:         machine.CLIActor(),
		PID:           os.Getpid(),
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := Save(rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// MarkRunning transitions the record to running.
func (r *Record) MarkRunning() error {
	return r.transition(machine.StatusRunning, nil, nil)
}

// MarkSucceeded transitions the record to succeeded with its terminal result.
func (r *Record) MarkSucceeded(res *Result) error {
	return r.transition(machine.StatusSucceeded, res, nil)
}

// MarkFailed transitions the record to failed, capturing the structured
// error (message redacted at write time).
func (r *Record) MarkFailed(err error) error {
	if err == nil {
		return nil
	}
	re := &RecordError{
		Code:    phelixerr.CodeOf(err).String(),
		Message: phelixerr.Redact(err.Error()),
	}
	return r.transition(machine.StatusFailed, nil, re)
}

func (r *Record) transition(status string, res *Result, re *RecordError) error {
	if r == nil {
		return nil
	}
	r.Status = status
	r.Result = res
	r.Error = re
	r.UpdatedAt = time.Now().UnixMilli()
	if status == machine.StatusSucceeded || status == machine.StatusFailed || status == machine.StatusCancelled {
		r.FinishedAt = r.UpdatedAt
	}
	return Save(r)
}

// Save persists the record with the ledger durability discipline (atomic
// tmp + fsync + rename + directory fsync).
func Save(r *Record) error {
	if r == nil {
		return nil
	}
	r.SchemaVersion = SchemaVersion
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "encode operation record", err)
	}
	dir := Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "create operation record directory", err)
	}
	path := filepath.Join(dir, r.ID+".json")
	tmp := fmt.Sprintf("%s.tmp-%d", path, os.Getpid())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "create operation record temp file", err)
	}
	cleanup := func() { _ = f.Close(); _ = os.Remove(tmp) }
	if _, err := f.Write(data); err != nil {
		cleanup()
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "write operation record", err)
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "sync operation record", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "close operation record", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "replace operation record", err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "open operation record directory", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "sync operation record directory", err)
	}
	return nil
}

// Load reads one operation record by ID. The ID is validated before it can
// touch the filesystem path.
func Load(id string) (*Record, error) {
	if !idPattern.MatchString(id) {
		return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"invalid operation id %q: expected %s<16 hex chars>", id, IDPrefix)
	}
	data, err := os.ReadFile(filepath.Join(Dir(), id+".json"))
	if os.IsNotExist(err) {
		return nil, phelixerr.Newf(phelixerr.CodeNotFound, "no operation record %s", id)
	}
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "read operation record", err)
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "decode operation record", err)
	}
	return &rec, nil
}

// List returns operation records newest-first, optionally filtered by app.
// Corrupt records are skipped and counted, never surfaced as failures — a
// query path must stay readable even when one file is damaged. limit <= 0
// returns everything.
func List(app string, limit int) ([]*Record, int, error) {
	entries, err := os.ReadDir(Dir())
	if os.IsNotExist(err) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, phelixerr.Wrap(phelixerr.CodeFilesystem, "read operation record directory", err)
	}
	var recs []*Record
	skipped := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(Dir(), e.Name()))
		if err != nil {
			skipped++
			continue
		}
		var rec Record
		if err := json.Unmarshal(data, &rec); err != nil || rec.ID == "" {
			skipped++
			continue
		}
		if app != "" && rec.App != app {
			continue
		}
		recs = append(recs, &rec)
	}
	sort.Slice(recs, func(i, j int) bool {
		if recs[i].CreatedAt != recs[j].CreatedAt {
			return recs[i].CreatedAt > recs[j].CreatedAt
		}
		return recs[i].ID > recs[j].ID
	})
	if limit > 0 && len(recs) > limit {
		recs = recs[:limit]
	}
	return recs, skipped, nil
}

// FindByDeploymentID scans the records for one correlating with a dep-…
// deployment telemetry ID. Deployment records only keep the last deployment
// ID in deploy.json, so this lookup walks the bounded record list instead.
func FindByDeploymentID(deploymentID string) (*Record, error) {
	if deploymentID == "" {
		return nil, phelixerr.New(phelixerr.CodeInvalidArgument, "empty deployment id")
	}
	recs, _, err := List("", 0)
	if err != nil {
		return nil, err
	}
	for _, rec := range recs {
		if rec.DeploymentID == deploymentID {
			return rec, nil
		}
	}
	return nil, phelixerr.Newf(phelixerr.CodeNotFound,
		"no operation record correlates with deployment %s", deploymentID)
}
