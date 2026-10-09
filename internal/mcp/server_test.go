package mcp

// server_test.go — transport-layer tests for the MCP adapter. They drive the
// REAL server over the real MCP protocol (an in-memory transport pair) with a
// stub Services, so they exercise tool registration, input-schema validation,
// envelope→result mapping, redaction and cancellation without depending on the
// command/deploy logic (that reuse is covered in cmd/mcp_test.go).

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/machine"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// stubServices is a configurable Services double. It records per-tool call
// counts so tests can prove, e.g., that an invalid resource never reaches the
// service.
type stubServices struct {
	env   *machine.Envelope
	err   error
	block bool // block until the handler ctx is cancelled

	ctxN, inspectN, showN, listN, opN, createN, applyN atomic.Int32
}

func (s *stubServices) ret(ctx context.Context) (*machine.Envelope, error) {
	if s.block {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
			return nil, context.DeadlineExceeded
		}
	}
	return s.env, s.err
}

func (s *stubServices) Context(ctx context.Context, _ ContextInput) (*machine.Envelope, error) {
	s.ctxN.Add(1)
	return s.ret(ctx)
}
func (s *stubServices) Inspect(ctx context.Context, _ InspectInput) (*machine.Envelope, error) {
	s.inspectN.Add(1)
	return s.ret(ctx)
}
func (s *stubServices) PlanShow(ctx context.Context, _ PlanShowInput) (*machine.Envelope, error) {
	s.showN.Add(1)
	return s.ret(ctx)
}
func (s *stubServices) PlanList(ctx context.Context, _ PlanListInput) (*machine.Envelope, error) {
	s.listN.Add(1)
	return s.ret(ctx)
}
func (s *stubServices) OperationStatus(ctx context.Context, _ OperationStatusInput) (*machine.Envelope, error) {
	s.opN.Add(1)
	return s.ret(ctx)
}
func (s *stubServices) PlanCreate(ctx context.Context, _ PlanCreateInput) (*machine.Envelope, error) {
	s.createN.Add(1)
	return s.ret(ctx)
}
func (s *stubServices) PlanApply(ctx context.Context, _ PlanApplyInput) (*machine.Envelope, error) {
	s.applyN.Add(1)
	return s.ret(ctx)
}

// newTestPair connects a client to a Server over an in-memory transport pair.
func newTestPair(t *testing.T, svc Services) *mcpsdk.ClientSession {
	t.Helper()
	ctx := context.Background()
	srvT, cliT := mcpsdk.NewInMemoryTransports()
	s := New(svc, Options{Diagnostics: io.Discard})
	ss, err := s.sdk.Connect(ctx, srvT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, cliT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close(); _ = ss.Close() })
	return cs
}

func textOf(t *testing.T, res *mcpsdk.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("result has no content")
	}
	tc, ok := res.Content[0].(*mcpsdk.TextContent)
	if !ok {
		t.Fatalf("content[0] is %T, want *mcp.TextContent", res.Content[0])
	}
	return tc.Text
}

func envOf(t *testing.T, res *mcpsdk.CallToolResult) *machine.Envelope {
	t.Helper()
	var env machine.Envelope
	if err := json.Unmarshal([]byte(textOf(t, res)), &env); err != nil {
		t.Fatalf("result content is not a machine envelope: %v", err)
	}
	return &env
}

func TestNewRegistersStableToolSurface(t *testing.T) {
	cs := newTestPair(t, &stubServices{env: machine.Success("", nil)})
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	wantReadOnly := map[string]bool{
		ToolContext: true, ToolInspect: true, ToolPlanShow: true, ToolPlanList: true,
		ToolOperationStatus: true, ToolPlanCreate: false, ToolPlanApply: false,
	}
	if len(res.Tools) != len(wantReadOnly) {
		names := make([]string, 0, len(res.Tools))
		for _, tl := range res.Tools {
			names = append(names, tl.Name)
		}
		t.Fatalf("tool count = %d (%s), want %d", len(res.Tools), strings.Join(names, ","), len(wantReadOnly))
	}
	byName := map[string]*mcpsdk.Tool{}
	for _, tl := range res.Tools {
		byName[tl.Name] = tl
	}
	for name, ro := range wantReadOnly {
		tl, ok := byName[name]
		if !ok {
			t.Fatalf("missing tool %q", name)
		}
		if tl.Annotations == nil || tl.Annotations.ReadOnlyHint != ro {
			t.Fatalf("%s ReadOnlyHint = %v, want %v", name, tl.Annotations != nil && tl.Annotations.ReadOnlyHint, ro)
		}
	}
}

func TestSuccessEnvelopeMapping(t *testing.T) {
	svc := &stubServices{env: machine.Success("op_abc123def4567890", map[string]any{"ok": true})}
	cs := newTestPair(t, svc)
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: ToolContext, Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("success envelope must not set IsError: %s", textOf(t, res))
	}
	env := envOf(t, res)
	if env.Status != machine.StatusSucceeded || env.OperationID != "op_abc123def4567890" {
		t.Fatalf("mapped envelope = %+v, want succeeded with operation id preserved", env)
	}
	if res.StructuredContent == nil {
		t.Fatal("structured content should mirror the envelope")
	}
}

func TestFailureEnvelopeMappingSetsIsError(t *testing.T) {
	svc := &stubServices{env: machine.Failure(phelixerr.New(phelixerr.CodeNotFound, "nope"), 12, "op_f00dface0badbead")}
	cs := newTestPair(t, svc)
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: ToolPlanShow, Arguments: map[string]any{"plan_id": "pln_0000000000000000"}})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Fatal("a failed Phelix envelope must set IsError (not a silent success)")
	}
	env := envOf(t, res)
	if env.Status != machine.StatusFailed || env.Error == nil || env.Error.Code != phelixerr.CodeNotFound.String() {
		t.Fatalf("mapped failure = %+v, want failed/%s with operation id %q", env, phelixerr.CodeNotFound, "op_f00dface0badbead")
	}
	if env.OperationID != "op_f00dface0badbead" {
		t.Fatalf("operation id not preserved: %q", env.OperationID)
	}
}

func TestUnknownInspectResourceIsToolErrorAndSkipsService(t *testing.T) {
	svc := &stubServices{env: machine.Success("", nil)}
	cs := newTestPair(t, svc)
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: ToolInspect, Arguments: map[string]any{"resource": "bogus"}})
	if err != nil {
		t.Fatalf("unknown resource should be a tool error, not a protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("unknown resource must be reported as a tool error")
	}
	if n := svc.inspectN.Load(); n != 0 {
		t.Fatalf("service was reached %d time(s) for an invalid resource", n)
	}
}

func TestMissingRequiredArgumentRejected(t *testing.T) {
	svc := &stubServices{env: machine.Success("", nil)}
	cs := newTestPair(t, svc)
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: ToolPlanShow, Arguments: map[string]any{}})
	// Either the SDK rejects it against the schema, or the service rejects the
	// empty id — never a success.
	if err == nil && (res == nil || !res.IsError) {
		t.Fatalf("missing required plan_id must be rejected, got res=%+v", res)
	}
}

func TestSecretsRedactedInToolResult(t *testing.T) {
	const secretBody = "livesecretABCDEF0123456789"
	svc := &stubServices{env: machine.Failure(
		phelixerr.Newf(phelixerr.CodeInvalidArgument, "bad value sk-%s", secretBody), 2, "")}
	cs := newTestPair(t, svc)
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: ToolInspect, Arguments: map[string]any{"resource": "config"}})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	blob, _ := json.Marshal(res)
	if strings.Contains(string(blob), secretBody) {
		t.Fatalf("secret leaked into tool result: %s", blob)
	}
}

func TestContextCancellationPropagates(t *testing.T) {
	cs := newTestPair(t, &stubServices{block: true})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var callErr error
	var res *mcpsdk.CallToolResult
	go func() {
		res, callErr = cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: ToolContext, Arguments: map[string]any{}})
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("CallTool did not return after cancellation")
	}
	if callErr == nil && (res == nil || !res.IsError) {
		t.Fatalf("cancellation should surface as an error, got res=%+v err=%v", res, callErr)
	}
}
