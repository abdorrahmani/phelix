// Package machine defines Phelix's shared, versioned machine-readable CLI
// contract — the shape every `--json` response uses. It is deliberately
// small: one envelope, one error body, one external lifecycle vocabulary,
// and the stdout/stderr discipline that keeps JSON pure.
//
// The contract is additive-only. Any breaking shape change must bump
// SchemaVersion. Existing matrix (`matrix list/show/status`) and webhook
// (`webhook status/history`) JSON outputs predate this package and keep
// their own schemas for backward compatibility.
package machine

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// SchemaVersion is the version of the machine contract carried by every
// envelope. Consumers must check it before interpreting other fields.
const SchemaVersion = "1"

// External lifecycle vocabulary. This is the ONLY status vocabulary the
// machine contract exposes; internal state machines (webhook job states,
// matrix run states, deployment phases) map into it via the Map*Status
// helpers below. The vocabulary must stay stable even when internal
// implementations change; detail is preserved by reporting the internal
// state alongside the mapped one (see internal_status fields).
const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// Actor is provenance metadata, not authenticated identity. Phase 1 has no
// authenticated actor model, so the CLI always reports the local, anonymous
// actor. A future security phase may populate ID and Authenticated from real
// credentials; consumers must not treat these fields as a security boundary.
type Actor struct {
	Type          string  `json:"type"` // "cli" in Phase 1
	ID            *string `json:"id"`   // always null in Phase 1
	Authenticated bool    `json:"authenticated"`
}

// CLIActor returns the Phase 1 actor block: a local CLI invocation with no
// authenticated identity attached.
func CLIActor() *Actor {
	return &Actor{Type: "cli"}
}

// ErrorBody is the machine-readable error object. Message is redacted through
// the centralized redactor before it is ever embedded, so the error path can
// never become a secret-leak path. Retryable is omitted when the code cannot
// be classified (UNKNOWN or plain errors). The shape leaves room for future
// fields (agent_id, requires_approval, policy metadata) without a version
// bump: add optional fields only.
type ErrorBody struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	ExitCode    int    `json:"exit_code"`
	Retryable   *bool  `json:"retryable,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
}

// Envelope is the top-level shape of every machine-readable response.
//   - Successful mutation:   {schema_version, operation_id, status, result}
//   - Successful read:       {schema_version, status, result}
//   - Failed (any command):  {schema_version, operation_id?, status:"failed", error}
//
// operation_id is present only when the command executed a real operation
// backed by durable state; read-only commands never fabricate one.
type Envelope struct {
	SchemaVersion string     `json:"schema_version"`
	OperationID   string     `json:"operation_id,omitempty"`
	Status        string     `json:"status,omitempty"`
	Result        any        `json:"result,omitempty"`
	Error         *ErrorBody `json:"error,omitempty"`
}

// Success builds the terminal envelope for a successful command. result may
// be nil for commands with nothing to report beyond the envelope itself.
func Success(operationID string, result any) *Envelope {
	return &Envelope{
		SchemaVersion: SchemaVersion,
		OperationID:   operationID,
		Status:        StatusSucceeded,
		Result:        result,
	}
}

// Failure builds the terminal envelope for a failed command. The message is
// redacted here — this is the machine contract's redaction chokepoint, the
// counterpart of the human renderer's per-field redaction. When operationID
// is empty, the operation identity registered for the executing command (if
// any) is used, so operation-associated failures always carry it.
func Failure(err error, exitCode int, operationID string) *Envelope {
	if operationID == "" {
		operationID = activeOperationID
	}
	env := &Envelope{
		SchemaVersion: SchemaVersion,
		OperationID:   operationID,
		Status:        StatusFailed,
		Error:         ErrorBodyFor(err, exitCode, operationID),
	}
	return env
}

// ErrorBodyFor builds the structured error body for err. exitCode is supplied
// by the caller (the CLI's ExitCodeFor mapping) so this package stays
// independent of the command layer. Unknown or plain errors omit retryable —
// they cannot be classified safely. A nil err yields an UNKNOWN body rather
// than a panic: the envelope must always be writable.
func ErrorBodyFor(err error, exitCode int, operationID string) *ErrorBody {
	code := phelixerr.CodeUnknown
	var msg string
	if err != nil {
		code = phelixerr.CodeOf(err)
		msg = phelixerr.Redact(err.Error())
	}
	if msg == "" {
		msg = "operation failed"
	}
	body := &ErrorBody{
		Code:        code.String(),
		Message:     msg,
		ExitCode:    exitCode,
		OperationID: operationID,
	}
	if code != phelixerr.CodeUnknown {
		retryable := phelixerr.Retryable(code)
		body.Retryable = &retryable
	}
	return body
}

// --- stdout/stderr discipline ------------------------------------------------
//
// In --json mode stdout must carry machine-readable JSON only. Rather than
// gating every progress print site, the command redirects the process-wide
// default stdout (which fmt.Printf and every progress helper in cmd/ write
// to) to stderr for the duration of the command, and pins the envelope
// writer to the real stdout captured before the switch. Human behavior with
// progress on stdout is untouched; child processes inherit file descriptors,
// not Go variables, so their fds are unaffected by the swap.

var (
	active bool
	// envelopeWriter is bound to the stdout in effect when the command
	// entered machine mode (the real stdout in production), so envelopes
	// always land there regardless of when they are written.
	envelopeWriter io.Writer = os.Stdout
	// activeOperationID is the operation identity of the executing mutation,
	// registered by the command once its durable record exists. The error
	// boundary embeds it in failure envelopes so an error is always
	// correlatable with the operation it aborted.
	activeOperationID string
)

// SetActiveOperation registers the operation identity of the mutation the
// command is executing. It is a no-op when no record was created.
func SetActiveOperation(operationID string) {
	activeOperationID = operationID
}

// ActiveOperation returns the registered operation identity ("" when none).
func ActiveOperation() string { return activeOperationID }

// EnterJSON switches the executing command into machine mode: progress
// output moves to stderr and the envelope writer is bound to the stdout in
// effect at entry. The returned restore func must be deferred by the command
// so the error boundary in main renders any failure against the original
// streams.
//
// Machine mode deliberately outlives the command body: main renders the
// structured failure envelope after RunE returns, so restore only undoes the
// stdout detour, never the mode itself. Hosts that run several commands in
// one process (tests) must call LeaveJSON between them.
func EnterJSON() (restore func()) {
	active = true
	envelopeWriter = os.Stdout
	prev := os.Stdout
	os.Stdout = os.Stderr
	return func() {
		os.Stdout = prev
		envelopeWriter = prev
	}
}

// LeaveJSON resets machine mode. The CLI runs one command per process and
// never calls this; it exists for tests and long-lived hosts that execute
// several commands in one process.
func LeaveJSON() {
	active = false
	envelopeWriter = os.Stdout
	SetActiveOperation("")
}

// Active reports whether the executing command was invoked with --json. The
// error boundary consults it to emit the structured error contract.
func Active() bool { return active }

// EnterCapture puts the process into machine mode with the envelope directed
// to w — an in-process buffer — instead of the real stdout, and progress
// output detoured to stderr. It exists for non-CLI hosts (the MCP adapter in
// internal/mcp) that must keep os.Stdout reserved for their own protocol
// stream while still reusing the exact command code paths that emit machine
// envelopes.
//
// It is the capture counterpart of EnterJSON: EnterJSON pins the envelope
// writer to the real stdout (the CLI's output channel), whereas EnterCapture
// pins it to w and never writes an envelope to the real stdout. The returned
// restore func must be deferred by the caller; machine mode must not outlive
// one captured command, so restore also clears the active-operation
// registration.
//
// Like EnterJSON it redirects the process-wide os.Stdout to os.Stderr for the
// duration, so stray human progress prints land on the diagnostics stream and
// never corrupt the host's protocol stream. The os.Stdout and envelope-writer
// globals are process-wide, so a host that handles several commands must
// serialize the captured calls.
func EnterCapture(w io.Writer) (restore func()) {
	active = true
	prevEnv := envelopeWriter
	envelopeWriter = w
	prevStdout := os.Stdout
	os.Stdout = os.Stderr
	return func() {
		os.Stdout = prevStdout
		envelopeWriter = prevEnv
		active = false
		activeOperationID = ""
	}
}

// MarshalEnvelope serializes an envelope exactly as WriteEnvelope prints it
// (pretty-printed, no trailing newline). The idempotency ledger stages this
// same byte string so a replay is byte-identical to the fresh response —
// fresh and replayed output must never differ in formatting.
func MarshalEnvelope(env *Envelope) ([]byte, error) {
	return json.MarshalIndent(env, "", "  ")
}

// ParseEnvelope decodes envelope bytes (e.g. a stored idempotency result) back
// into an Envelope, so a caller can inspect the recorded terminal outcome
// (status, operation id) without re-emitting it.
func ParseEnvelope(data []byte) (*Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeUnavailable, "parse machine envelope", err)
	}
	return &env, nil
}

// WriteEnvelope writes exactly one envelope document to the real stdout,
// pretty-printed with a trailing newline — the same formatting discipline
// the existing matrix/webhook JSON commands use.
func WriteEnvelope(env *Envelope) error {
	data, err := MarshalEnvelope(env)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(envelopeWriter, "%s\n", data)
	return err
}

// WriteRawEnvelope writes a previously serialized envelope (an idempotency
// replay) verbatim, after validating that it still parses as a current-schema
// envelope. Byte-for-byte replay guarantees the retried consumer sees exactly
// what the first execution produced.
func WriteRawEnvelope(data []byte) error {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return phelixerr.Wrap(phelixerr.CodeUnavailable,
			"stored idempotency result is not a valid envelope", err)
	}
	if env.SchemaVersion != SchemaVersion {
		return phelixerr.Newf(phelixerr.CodeUnavailable,
			"stored idempotency result has schema_version %q, want %q", env.SchemaVersion, SchemaVersion)
	}
	_, err := fmt.Fprintf(envelopeWriter, "%s\n", data)
	return err
}

// --- external lifecycle mapping ----------------------------------------------

// MapWebhookJobStatus maps a webhook JobRecord status into the external
// lifecycle vocabulary. rolled_back maps to failed: the deployment did not
// stick, which is what an automation consumer needs to know. The raw status
// stays available on the record for detail.
func MapWebhookJobStatus(status string) string {
	switch status {
	case "accepted", "queued", "syncing", "building", "deploying", "health_checking":
		return StatusRunning
	case "succeeded":
		return StatusSucceeded
	case "failed", "rolled_back":
		return StatusFailed
	case "cancelled":
		return StatusCancelled
	default:
		return StatusFailed
	}
}

// MapMatrixRunStatus maps a matrix run status into the external vocabulary.
// partial and interrupted both map to failed — an automation consumer must
// not treat either as a success — while the raw status preserves the
// distinction.
func MapMatrixRunStatus(status string) string {
	switch status {
	case "running", "pending":
		return StatusRunning
	case "succeeded":
		return StatusSucceeded
	case "failed", "partial", "interrupted":
		return StatusFailed
	default:
		return StatusFailed
	}
}

// MapDeployStatus maps a deployment telemetry status into the external
// vocabulary.
func MapDeployStatus(status string) string {
	switch status {
	case "idle":
		return StatusPending
	case "in_progress":
		return StatusRunning
	case "succeeded":
		return StatusSucceeded
	case "failed":
		return StatusFailed
	case "cancelled":
		return StatusCancelled
	default:
		return StatusFailed
	}
}

// MapRollbackStatus maps a rollback history status into the external
// vocabulary.
func MapRollbackStatus(status string) string {
	switch status {
	case "success":
		return StatusSucceeded
	case "failed":
		return StatusFailed
	default:
		return StatusFailed
	}
}
