package authz

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// The authorization audit trail answers exactly one question:
//
//	Who was allowed or denied execution of this exact plan, and when?
//
// It is not a SIEM, not an event bus and not a general activity log. One
// append-only JSONL record per decision, bounded, with no free-form text and
// structurally no secrets — see [DecisionRecord].

const (
	// decisionsFile is the append-only decision log under <data-dir>/authz/.
	decisionsFile = "decisions.jsonl"
	// maxDecisionRecords bounds the log. When exceeded it is rewritten
	// keeping the newest records, the same bounded-history discipline the
	// webhook delivery ledger and rollback history use. An execution-path
	// audit log must not be able to fill the disk.
	maxDecisionRecords = 512
	// maxDecisionLineBytes bounds one record on read, so a corrupted log
	// cannot be used to exhaust memory on the execution path.
	maxDecisionLineBytes = 8 * 1024
)

// decisionIDPrefix — azd_ + 16 hex chars. The ID lets an allowed decision be
// joined to the operation it authorized: the operation record stores it (see
// ops.Record.AuthzDecisionID), closing the correlation chain
// actor → decision → approval → plan → operation → deployment.
const decisionIDPrefix = "azd_"

// DecisionRecord is one durable authorization decision.
//
// Field selection is a security decision, not a convenience one. Only safe
// identifiers are persisted: actor type/id, action, target name, plan id,
// plan hash, approval id, decision, code, mode and timestamps. No credential,
// token, header, environment value or secret name/value can reach this struct
// — there is nowhere to put one.
type DecisionRecord struct {
	SchemaVersion string `json:"schema_version"`
	DecisionID    string `json:"decision_id"`
	DecidedAtMs   int64  `json:"decided_at_ms"`

	ActorType          string `json:"actor_type"`
	ActorID            string `json:"actor_id,omitempty"`
	ActorAuthenticated bool   `json:"actor_authenticated"`
	// ActorProvenance is the unverified host login name of the calling
	// process. Audit colour only; it is not an identity and grants nothing.
	ActorProvenance string `json:"actor_provenance,omitempty"`

	Action   string `json:"action"`
	Target   string `json:"target"`
	PlanID   string `json:"plan_id,omitempty"`
	PlanHash string `json:"plan_hash,omitempty"`

	Decision   string `json:"decision"`
	Code       string `json:"code"`
	Reason     string `json:"reason,omitempty"`
	Mode       string `json:"mode"`
	RuleIndex  int    `json:"rule_index"`
	ApprovalID string `json:"approval_id,omitempty"`

	// OperationID is empty at decision time by construction: authorization
	// happens BEFORE the operation record exists, so a denied decision can
	// never leave a stray operation behind. The operation record carries the
	// decision ID in the other direction.
	OperationID string `json:"operation_id,omitempty"`
}

// DecisionsPath returns the audit log location.
func DecisionsPath() string { return filepath.Join(Dir(), decisionsFile) }

// newDecisionID mints a decision ID.
func newDecisionID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeFilesystem, "generate decision id", err)
	}
	return decisionIDPrefix + hex.EncodeToString(buf), nil
}

// RecordDecision appends one decision to the audit log and returns its ID.
//
// Recording is best-effort by design, matching the operation-record
// discipline: a log-write failure must not turn a legitimate, authorized
// execution into a failure, and it must not turn a denial into an allow
// either — the decision has already been made before this is called. The
// error is returned so the caller can log it; it is never the gate.
func RecordDecision(req Request, d Decision, at int64) (string, error) {
	id, err := newDecisionID()
	if err != nil {
		return "", err
	}
	rec := DecisionRecord{
		SchemaVersion:      SchemaVersion,
		DecisionID:         id,
		DecidedAtMs:        at,
		ActorType:          req.Actor.Type,
		ActorID:            req.Actor.ID,
		ActorAuthenticated: req.Actor.Authenticated,
		ActorProvenance:    phelixerr.Redact(LocalProvenance()),
		Action:             req.Action,
		Target:             req.Target.App,
		Decision:           d.Effect,
		Code:               d.Code,
		// The reason is drawn from this package's authored strings, and is
		// run through the shared redactor anyway: the audit path reuses the
		// existing redaction infrastructure rather than introducing a third
		// secret-handling mechanism.
		Reason:     phelixerr.Redact(d.Reason),
		Mode:       d.Mode,
		RuleIndex:  d.RuleIndex,
		ApprovalID: d.ApprovalID,
	}
	if req.Plan != nil {
		rec.PlanID = req.Plan.ID
		rec.PlanHash = req.Plan.Hash
	}
	if err := appendDecision(rec); err != nil {
		return id, err
	}
	return id, nil
}

func appendDecision(rec DecisionRecord) error {
	dir := Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "create authorization state directory", err)
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "encode authorization decision", err)
	}
	path := DecisionsPath()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "open authorization decision log", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "append authorization decision", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "sync authorization decision log", err)
	}
	if err := f.Close(); err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "close authorization decision log", err)
	}
	return trimDecisions(path)
}

// trimDecisions keeps the log bounded by rewriting it with the newest
// maxDecisionRecords lines when it grows past the cap.
func trimDecisions(path string) error {
	records, err := readDecisionLines(path)
	if err != nil || len(records) <= maxDecisionRecords {
		return err
	}
	keep := records[len(records)-maxDecisionRecords:]
	tmp := fmt.Sprintf("%s.tmp-%d", path, os.Getpid())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "create authorization decision log temp file", err)
	}
	w := bufio.NewWriter(f)
	for _, line := range keep {
		if _, err := w.WriteString(line + "\n"); err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "write authorization decision log", err)
		}
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "flush authorization decision log", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "sync authorization decision log", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "close authorization decision log", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "replace authorization decision log", err)
	}
	return nil
}

// readDecisionLines returns the raw log lines, skipping blanks. Oversized
// lines are rejected so a corrupted log cannot exhaust memory.
func readDecisionLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeAuthzUnavailable, "read authorization decision log", err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), maxDecisionLineBytes)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	if err := sc.Err(); err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeAuthzUnavailable, "scan authorization decision log", err)
	}
	return out, nil
}

// ListDecisions returns the most recent decisions, newest first. Undecodable
// lines are skipped and counted rather than failing the query — a read path
// must stay readable even when one record is damaged. limit <= 0 returns all
// retained records.
func ListDecisions(planID string, limit int) ([]DecisionRecord, int, error) {
	lines, err := readDecisionLines(DecisionsPath())
	if err != nil {
		return nil, 0, err
	}
	out := make([]DecisionRecord, 0, len(lines))
	skipped := 0
	for i := len(lines) - 1; i >= 0; i-- {
		var rec DecisionRecord
		if err := json.Unmarshal([]byte(lines[i]), &rec); err != nil || rec.DecisionID == "" {
			skipped++
			continue
		}
		if planID != "" && rec.PlanID != planID {
			continue
		}
		out = append(out, rec)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, skipped, nil
}
