package cmd

import (
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/ops"
)

// opRun carries one mutation command's operation identity and request-key
// idempotency through its execution.
//
// Two independent guarantees live here, and they fail differently on purpose:
//   - The operation record is best-effort identity. A record-write failure
//     must never break a real deploy, so a failed Begin simply leaves the
//     command without an operation ID (the envelope omits operation_id).
//   - The request-key ledger is fail-closed idempotency. Once a key is
//     presented, its begin/complete states persist durably (fsync'd JSON,
//     survives restart); a corrupted ledger fails the keyed operation rather
//     than silently executing without idempotency.
type opRun struct {
	rec         *ops.Record
	kind        string
	app         string
	key         string
	fingerprint string
	replayed    bool
	// replayedStatus/replayedOpID carry the terminal outcome of a replayed
	// idempotency entry so the plan path can reconcile the plan's own status
	// with what the ledger already recorded (see applyPlan). Empty unless a
	// replay occurred.
	replayedStatus string
	replayedOpID   string
	result         *ops.Result
	successEnv     *machine.Envelope
}

// newOpRun prepares the operation runner. No durable state is touched yet:
// the record is only created on the execute path (a replayed key must not
// leave a stray pending record behind).
func newOpRun(kind, app, requestKey string) *opRun {
	return &opRun{kind: kind, app: app, key: requestKey}
}

// beginIdempotency consults the request-key ledger when a key was supplied,
// then creates the durable operation record on the execute path.
//
// material carries the operation-defining inputs fingerprinted for conflict
// detection (the kind and app are added automatically). When a replay is
// returned the stored terminal envelope has already been written to stdout
// verbatim and replayed=true; the caller returns without executing.
func (o *opRun) beginIdempotency(material map[string]string) (replayed bool, err error) {
	if o == nil {
		return false, nil
	}
	if o.key != "" {
		o.fingerprint = ops.Fingerprint(o.kind, o.app, material)
		outcome, entry, berr := ops.BeginKey(o.key, o.fingerprint, o.kind, o.app)
		if berr != nil {
			return false, berr
		}
		if outcome == ops.BeginReplay {
			o.replayed = true
			// Record the replayed terminal outcome so a derived-key caller (plan
			// apply) can reconcile its own durable status with the ledger's,
			// making retry semantics independent of ledger eviction.
			if env, perr := machine.ParseEnvelope(entry.Result); perr == nil {
				o.replayedStatus = env.Status
				o.replayedOpID = env.OperationID
			}
			if werr := machine.WriteRawEnvelope(entry.Result); werr != nil {
				return false, phelixerr.Wrap(phelixerr.CodeFilesystem, "replay idempotent result", werr)
			}
			return true, nil
		}
	}
	o.beginRecord()
	return false, nil
}

func (o *opRun) beginRecord() {
	rec, err := ops.Begin(o.kind, o.app, o.key)
	if err != nil {
		// Best-effort identity: the mutation proceeds without an operation ID.
		logs.WarningFile("ops", "operation record not created for %s %q: %v", o.kind, o.app, err)
		return
	}
	o.rec = rec
	machine.SetActiveOperation(rec.ID)
	if err := rec.MarkRunning(); err != nil {
		logs.WarningFile("ops", "operation %s not marked running: %v", rec.ID, err)
	}
	// Bound the operation-record store (best-effort). The record just created
	// is in-flight, so ops.Prune's in-flight guard never prunes it.
	pruneOperationRecords()
}

// setDeploymentID correlates the record with the deployment telemetry stream.
func (o *opRun) setDeploymentID(tracker *deploy.Tracker) {
	if o == nil || o.rec == nil || tracker == nil {
		return
	}
	o.rec.DeploymentID = tracker.DeploymentID()
}

// setPlanCorrelation records the plan that produced this operation.
func (o *opRun) setPlanCorrelation(planID, planHash string) {
	if o == nil || o.rec == nil {
		return
	}
	o.rec.PlanID = planID
	o.rec.PlanHash = planHash
}

// setAuthzCorrelation records the authorization decision (and the approval
// that satisfied it, when one was required) that permitted this operation.
// It closes the Phase 4 correlation chain:
//
//	actor → decision → approval → plan → operation → deployment
func (o *opRun) setAuthzCorrelation(decisionID, approvalID string) {
	if o == nil || o.rec == nil {
		return
	}
	o.rec.AuthzDecisionID = decisionID
	o.rec.ApprovalID = approvalID
}

// setResult stages the terminal result written to the record on success.
func (o *opRun) setResult(res *ops.Result) {
	if o == nil {
		return
	}
	o.result = res
}

// setSuccessEnv stages the success envelope the command wrote (or is about
// to write) so finish can persist it as the key's replayable result.
func (o *opRun) setSuccessEnv(env *machine.Envelope) {
	if o == nil {
		return
	}
	o.successEnv = env
}

// operationID returns the durable record's ID, "" when none was created.
func (o *opRun) operationID() string {
	if o == nil || o.rec == nil {
		return ""
	}
	return o.rec.ID
}

// finish finalizes the operation record and, when a request key is in play,
// durably records the terminal envelope. It must be deferred by the command
// with the named return error so the terminal state matches what the process
// actually reports.
func (o *opRun) finish(err *error) {
	if o == nil {
		return
	}
	if o.rec != nil {
		switch {
		case *err == nil:
			_ = o.rec.MarkSucceeded(o.result)
		case o.result != nil && o.result.Verification != "" &&
			phelixerr.IsCode(*err, phelixerr.CodeRollbackVerifyFailed):
			// The rollback committed (the target version is serving); only the
			// stability window failed or was cancelled. The record reflects
			// execution success with the verification outcome attached — the
			// same truth exit code 23 encodes.
			_ = o.rec.MarkSucceeded(o.result)
		default:
			_ = o.rec.MarkFailed(*err)
		}
	}
	if o.key == "" || o.replayed {
		return
	}
	env := o.successEnv
	if *err != nil {
		env = machine.Failure(*err, ExitCodeFor(*err), o.operationID())
	}
	if env == nil {
		return
	}
	data, merr := machine.MarshalEnvelope(env)
	if merr != nil {
		logs.WarningFile("ops", "idempotency result not encodable for request key %q: %v", o.key, merr)
		return
	}
	if cerr := ops.CompleteKeyData(o.key, o.operationID(), data); cerr != nil {
		logs.WarningFile("ops", "idempotency result not recorded for request key %q: %v", o.key, cerr)
	}
}

// writeResultEnv emits the success envelope on stdout and stages it for the
// idempotency ledger. Non-JSON invocations only stage the envelope (used by
// the ledger) — nothing is printed.
func (o *opRun) writeResultEnv(env *machine.Envelope) error {
	if o != nil {
		o.setSuccessEnv(env)
	}
	return writeEnvelopeResult(env)
}

// writeEnvelopeResult writes a success envelope on stdout in machine mode —
// the shared terminal step for results that are not staged through opRun
// (e.g. a matrix build, whose operation identity is the run ID).
func writeEnvelopeResult(env *machine.Envelope) error {
	if !machine.Active() {
		return nil
	}
	if err := machine.WriteEnvelope(env); err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "write machine result", err)
	}
	return nil
}

// requestIdForEvents returns the correlation ID for rollback/deployment
// events: the operation record ID when one exists, falling back to the raw
// request key.
func (o *opRun) requestIdForEvents() string {
	if id := o.operationID(); id != "" {
		return id
	}
	return o.key
}
