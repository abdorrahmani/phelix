// Package plans implements Phelix's first-class execution plans: immutable,
// content-addressed, persistable descriptions of what a mutation WOULD do,
// generated from the same resolution semantics the existing execution paths
// use — never a second interpretation of deployment.
//
// The core invariant:
//
//	The thing that was planned is the thing that gets executed, or
//	execution fails closed.
//
// Immutability is structural: a plan file is created exactly once, its
// semantic content is covered by the canonical plan hash (which excludes
// volatile metadata such as plan_id, created_at and status), and every load
// re-verifies that hash. Persistence follows the fsync discipline of the
// operation-record store.
package plans

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// SchemaVersion is the plan-artifact schema version. It participates in the
// canonical hash: a future breaking schema change cannot silently validate
// against plans persisted under the old one.
const SchemaVersion = "1"

// IDPrefix — plan IDs are pln_ + 16 hex chars (8 crypto/rand bytes),
// mirroring the operation and webhook job ID disciplines.
const IDPrefix = "pln_"

var idPattern = regexp.MustCompile(`^pln_[0-9a-f]{16}$`)

// Plan lifecycle statuses (stored). "stale" is deliberately NOT a stored
// status: staleness is computed from current execution-relevant state at
// apply/show time, never trusted from disk.
const (
	StatusCreated = "created"
	StatusApplied = "applied"
	StatusFailed  = "failed"
)

// Action types. Names match the CLI verbs that execute them — rebuild IS the
// deploy lifecycle operation (classic or zero-downtime); there is no
// separate "deploy" execution path in the CLI to model.
const (
	ActionRebuild  = "rebuild"
	ActionRollback = "rollback"
)

// Plan is one immutable execution plan.
type Plan struct {
	SchemaVersion string         `json:"schema_version"`
	PlanID        string         `json:"plan_id"`
	PlanHash      string         `json:"plan_hash"`
	CreatedAt     int64          `json:"created_at_ms"`
	Status        string         `json:"status"`
	Action        Action         `json:"action"`
	Target        Target         `json:"target"`
	Inputs        Inputs         `json:"inputs"`
	Execution     Execution      `json:"execution"`
	Preconditions []Precondition `json:"preconditions"`
	Capabilities  []string       `json:"capabilities"`
	// Correlation records the operation the plan produced. It is metadata,
	// not semantics: it participates in neither the hash nor immutability
	// of the execution specification.
	Correlation *Correlation `json:"correlation,omitempty"`
}

// Action names the mutation and its application.
type Action struct {
	Type        string `json:"type"`
	Application string `json:"application"`
}

// Target identifies what the mutation operates on.
type Target struct {
	AppID   string `json:"app_id,omitempty"`
	AppName string `json:"app_name,omitempty"`
	// Language is the detected source language the execution path will build.
	Language string `json:"language,omitempty"`
}

// Inputs are the execution-defining inputs, captured exactly as the existing
// command resolution resolves them. All fields are typed; per-action fields
// are omitted for the other action. Secrets never appear here — the rebuild
// path injects env at execution time from the encrypted store, never through
// the plan.
type Inputs struct {
	// Rebuild inputs.
	SourceDir    string   `json:"source_dir,omitempty"`
	Port         int      `json:"port,omitempty"`
	Tag          string   `json:"tag,omitempty"`
	BuildArgs    []string `json:"build_args,omitempty"`
	AutoRollback bool     `json:"auto_rollback,omitempty"`
	NoUpload     bool     `json:"no_upload,omitempty"`
	// SourceCommit pins the source the plan was created from, when the
	// source directory is a git work tree. Absent when not determinable —
	// the precondition is then simply not created.
	SourceCommit string `json:"source_commit,omitempty"`
	// The strategy-selection flags exactly as they were resolved for the
	// plan (--strategy override, --blue-green, --replicas, --canary), so
	// application reproduces the original flag precedence bit-for-bit.
	StrategyOverride string `json:"strategy_override,omitempty"`
	FlagBlueGreen    bool   `json:"flag_blue_green,omitempty"`
	FlagReplicas     int    `json:"flag_replicas,omitempty"`
	FlagCanary       int    `json:"flag_canary,omitempty"`
	// PortExplicit records that the port came from an explicit --port flag
	// rather than precedence, so application can mark the synthetic flag
	// set identically.
	PortExplicit bool `json:"port_explicit,omitempty"`
	// ConfigFingerprint covers the execution-relevant configuration the
	// plan was created against (see FingerprintRebuildConfig).
	ConfigFingerprint string `json:"config_fingerprint,omitempty"`

	// Rollback inputs.
	TargetVersion int    `json:"target_version,omitempty"`
	TargetTag     string `json:"target_tag,omitempty"`
	VerifyMs      int64  `json:"verify_ms,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// Execution describes how the existing engine will execute the plan.
type Execution struct {
	// Strategy is the effective strategy resolution produced: classic,
	// blue-green, rolling, canary or progressive.
	Strategy string `json:"strategy,omitempty"`
	// Replicas for rolling; CanaryPercent for canary/progressive plans.
	Replicas      int    `json:"replicas,omitempty"`
	CanaryPercent int    `json:"canary_percent,omitempty"`
	Runtime       string `json:"runtime,omitempty"`
	Network       string `json:"network,omitempty"`
	PublicPort    int    `json:"public_port,omitempty"`
	// Rollback projections (captured from deploy.PlanRollback — the same
	// planning helper `rollback --dry-run` uses).
	Downtime             bool   `json:"downtime,omitempty"`
	CurrentSlot          string `json:"current_slot,omitempty"`
	TargetSlot           string `json:"target_slot,omitempty"`
	FromVersion          int    `json:"from_version,omitempty"`
	ToVersion            int    `json:"to_version,omitempty"`
	EnvSnapshotAvailable bool   `json:"env_snapshot_available,omitempty"`
	// Steps mirror the execution path's own step description. For rollback
	// this is deploy.PlanRollback's step list; for rebuild it names the
	// resolved top-level path (build → start → promote / engine steps).
	Steps []string `json:"steps,omitempty"`
}

// Precondition is one typed, narrowly scoped fact that held at plan-creation
// time and is re-evaluated at apply time. Expected is the canonical string
// form of the value the plan was created against. No expressions, no
// condition language.
type Precondition struct {
	Type     string `json:"type"`
	Expected string `json:"expected"`
	// Source names the state origin the precondition is checked against
	// (e.g. "versions.json", "deploy.json", "phelix.yaml", "toolchain").
	Source string `json:"source"`
}

// Correlation links the plan to the operation its application produced.
type Correlation struct {
	OperationID  string `json:"operation_id,omitempty"`
	DeploymentID string `json:"deployment_id,omitempty"`
	AppliedAtMs  int64  `json:"applied_at_ms,omitempty"`
}

// NewID mints a new pln_ plan ID.
func NewID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeFilesystem, "generate plan id", err)
	}
	return IDPrefix + hex.EncodeToString(buf), nil
}

// ValidateID reports whether id is a well-formed plan ID.
func ValidateID(id string) bool {
	return idPattern.MatchString(id)
}

// hashInput is the canonical hash preimage: exactly the execution-relevant
// fields, nothing else. plan_id, created_at, status and correlation are
// deliberately excluded — two plans differing only in those fields are the
// same semantic plan and MUST hash identically. The struct (not the serialized
// plan) is hashed, so field participation is explicit in code; json.Marshal
// of structs is deterministic across machines and processes, and the hashed
// types contain no maps (Preconditions and Capabilities are sorted at
// creation), so no map-order canonicalization can leak in.
type hashInput struct {
	SchemaVersion string
	Action        Action
	Target        Target
	Inputs        Inputs
	Execution     Execution
	Preconditions []Precondition
	Capabilities  []string
}

// Hash computes the canonical plan hash over the semantic content.
func (p *Plan) Hash() (string, error) {
	if p == nil {
		return "", phelixerr.New(phelixerr.CodeInvalidArgument, "nil plan")
	}
	hi := hashInput{
		SchemaVersion: p.SchemaVersion,
		Action:        p.Action,
		Target:        p.Target,
		Inputs:        p.Inputs,
		Execution:     p.Execution,
		Preconditions: append([]Precondition(nil), p.Preconditions...),
		Capabilities:  append([]string(nil), p.Capabilities...),
	}
	data, err := json.Marshal(hi)
	if err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeInvalidArgument, "canonicalize plan", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Canonicalize sorts the order-sensitive collections so semantically equal
// plans hash equally regardless of construction order. Call it after
// assembling a plan and before hashing/persisting.
func (p *Plan) Canonicalize() {
	sort.SliceStable(p.Preconditions, func(i, j int) bool {
		if p.Preconditions[i].Type != p.Preconditions[j].Type {
			return p.Preconditions[i].Type < p.Preconditions[j].Type
		}
		if p.Preconditions[i].Expected != p.Preconditions[j].Expected {
			return p.Preconditions[i].Expected < p.Preconditions[j].Expected
		}
		return p.Preconditions[i].Source < p.Preconditions[j].Source
	})
	sort.Strings(p.Capabilities)
	sort.Strings(p.Inputs.BuildArgs)
}

// Finalize canonicalizes the plan, computes its hash and stamps the identity
// fields. It is the single path from an assembled plan to a persistable one.
func (p *Plan) Finalize() error {
	if p.Action.Type == "" || p.Action.Application == "" {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "plan requires an action type and application")
	}
	switch p.Action.Type {
	case ActionRebuild, ActionRollback:
	default:
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "unsupported plan action %q", p.Action.Type)
	}
	p.Canonicalize()
	p.SchemaVersion = SchemaVersion
	if p.PlanID == "" {
		id, err := NewID()
		if err != nil {
			return err
		}
		p.PlanID = id
	}
	if p.CreatedAt == 0 {
		p.CreatedAt = time.Now().UnixMilli()
	}
	hash, err := p.Hash()
	if err != nil {
		return err
	}
	p.PlanHash = hash
	return nil
}

// CorrelationOperationID returns the operation the plan produced ("" when
// not yet applied).
func (p *Plan) CorrelationOperationID() string {
	if p == nil || p.Correlation == nil {
		return ""
	}
	return p.Correlation.OperationID
}

// VerifyHash recomputes the canonical hash and compares it with the stored
// value. A mismatch means the plan content was corrupted or tampered with.
func (p *Plan) VerifyHash() error {
	computed, err := p.Hash()
	if err != nil {
		return err
	}
	if computed != p.PlanHash {
		return phelixerr.Newf(phelixerr.CodePlanHashMismatch,
			"plan hash mismatch: stored %s, computed %s — the plan content changed after creation", p.PlanHash, computed)
	}
	return nil
}
