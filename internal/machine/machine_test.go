package machine

import (
	"encoding/json"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

func TestSuccessEnvelope_HasSchemaVersion(t *testing.T) {
	data, err := json.Marshal(Success("op_0123456789abcdef", map[string]any{"port": 8080}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if raw["schema_version"] != SchemaVersion {
		t.Fatalf("schema_version = %v, want %q", raw["schema_version"], SchemaVersion)
	}
	if raw["operation_id"] != "op_0123456789abcdef" {
		t.Fatalf("operation_id = %v", raw["operation_id"])
	}
	if raw["status"] != StatusSucceeded {
		t.Fatalf("status = %v, want %q", raw["status"], StatusSucceeded)
	}
}

func TestReadEnvelope_OmitsOperationID(t *testing.T) {
	// Read-only commands must not fabricate an operation identity: the key
	// must be absent from the JSON entirely, not present but empty.
	data, err := json.Marshal(Success("", map[string]any{"count": 0}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "operation_id") {
		t.Fatalf("read-only envelope must omit operation_id, got: %s", data)
	}
}

func TestFailureEnvelope_ErrorShape(t *testing.T) {
	err := phelixerr.Newf(phelixerr.CodeNotFound, "no app %q", "demo")
	env := Failure(err, ExitCodeForTesting, "op_0123456789abcdef")
	if env.Error == nil {
		t.Fatal("failure envelope must carry an error body")
	}
	if env.Error.Code != "NOT_FOUND" {
		t.Fatalf("code = %q", env.Error.Code)
	}
	if env.Error.ExitCode != ExitCodeForTesting {
		t.Fatalf("exit_code = %d, want %d", env.Error.ExitCode, ExitCodeForTesting)
	}
	if env.Error.OperationID != "op_0123456789abcdef" {
		t.Fatalf("operation_id = %q", env.Error.OperationID)
	}
	if env.Error.Retryable == nil || *env.Error.Retryable {
		t.Fatalf("NOT_FOUND must be classified not-retryable, got %v", env.Error.Retryable)
	}
	// Failure envelopes carry the failed status so naive consumers get a
	// symmetric shape with the success path.
	if env.Status != StatusFailed {
		t.Fatalf("status = %q, want %q", env.Status, StatusFailed)
	}
}

// TestFailureEnvelope_RedactsSecrets pins the machine error contract's
// redaction chokepoint: a cause chain that embedded a credential must not
// reach the envelope message.
func TestFailureEnvelope_RedactsSecrets(t *testing.T) {
	err := phelixerr.Wrapf(phelixerr.CodeConnection, nil, "dial failed with token ghp_abcdef0123456789012345678901234567890")
	env := Failure(err, 30, "")
	if strings.Contains(env.Error.Message, "ghp_abcdef") {
		t.Fatalf("machine error message leaked a credential: %q", env.Error.Message)
	}
}

func TestErrorBodyFor_UnknownOmitsRetryable(t *testing.T) {
	body := ErrorBodyFor(mapError{}, 1, "")
	if body.Code != "UNKNOWN" {
		t.Fatalf("plain error must surface as UNKNOWN, got %q", body.Code)
	}
	if body.Retryable != nil {
		t.Fatalf("unclassifiable errors must omit retryable, got %v", *body.Retryable)
	}
}

func TestErrorBodyFor_RetryableClassification(t *testing.T) {
	env := Failure(phelixerr.New(phelixerr.CodeTimeout, "deadline"), 60, "")
	if env.Error.Retryable == nil || !*env.Error.Retryable {
		t.Fatal("TIMEOUT must be classified retryable")
	}
}

func TestFailure_FallsBackToRegisteredOperation(t *testing.T) {
	SetActiveOperation("op_fedcba9876543210")
	defer SetActiveOperation("")
	env := Failure(phelixerr.New(phelixerr.CodeDeployFailed, "boom"), 21, "")
	if env.OperationID != "op_fedcba9876543210" {
		t.Fatalf("operation_id = %q, want the registered operation", env.OperationID)
	}
	if env.Error.OperationID != env.OperationID {
		t.Fatalf("error body operation_id = %q, want %q", env.Error.OperationID, env.OperationID)
	}
}

func TestWriteRawEnvelope_RejectsForeignSchema(t *testing.T) {
	stale := []byte(`{"schema_version":"0","status":"succeeded"}`)
	if err := WriteRawEnvelope(stale); err == nil {
		t.Fatal("a foreign schema_version must be rejected, not replayed")
	}
	good := []byte(`{"schema_version":"1","status":"succeeded","result":{"v":1}}`)
	if err := WriteRawEnvelope(good); err != nil {
		t.Fatalf("current-schema envelope must replay: %v", err)
	}
	if err := WriteRawEnvelope([]byte(`{not json`)); err == nil {
		t.Fatal("invalid JSON must be rejected")
	}
}

func TestLifecycleVocabulary_IsStable(t *testing.T) {
	// The external vocabulary is contract; accidental edits must fail here.
	want := map[string]string{
		StatusPending:   "pending",
		StatusRunning:   "running",
		StatusSucceeded: "succeeded",
		StatusFailed:    "failed",
		StatusCancelled: "cancelled",
	}
	for got, expected := range want {
		if got != expected {
			t.Fatalf("lifecycle vocabulary drifted: %q != %q", got, expected)
		}
	}
}

func TestStatusMapping(t *testing.T) {
	cases := []struct {
		fn   func(string) string
		in   string
		want string
	}{
		{MapWebhookJobStatus, "deploying", StatusRunning},
		{MapWebhookJobStatus, "health_checking", StatusRunning},
		{MapWebhookJobStatus, "succeeded", StatusSucceeded},
		{MapWebhookJobStatus, "rolled_back", StatusFailed},
		{MapWebhookJobStatus, "cancelled", StatusCancelled},
		{MapMatrixRunStatus, "partial", StatusFailed},
		{MapMatrixRunStatus, "interrupted", StatusFailed},
		{MapMatrixRunStatus, "succeeded", StatusSucceeded},
		{MapDeployStatus, "in_progress", StatusRunning},
		{MapDeployStatus, "idle", StatusPending},
		{MapDeployStatus, "cancelled", StatusCancelled},
		{MapRollbackStatus, "success", StatusSucceeded},
		{MapRollbackStatus, "failed", StatusFailed},
	}
	for _, tc := range cases {
		if got := tc.fn(tc.in); got != tc.want {
			t.Errorf("mapping(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// mapError is an error type carrying no structured code.
type mapError struct{}

func (mapError) Error() string { return "plain boom" }

// ExitCodeForTesting is a stand-in exit code for tests — the real mapping
// lives in cmd and is exercised there.
const ExitCodeForTesting = 12
