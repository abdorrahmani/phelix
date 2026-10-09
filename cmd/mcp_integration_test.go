package cmd

// mcp_integration_test.go — the acceptance test required by Phase 5: it starts
// the REAL `phelix mcp serve --stdio` binary as a subprocess and drives it over
// the actual MCP stdio protocol with the SDK client. Because the client decodes
// the subprocess's stdout as JSON-RPC, a successful session is itself proof
// that stdout carries only protocol traffic (any human/diagnostic byte on
// stdout would break framing and fail the handshake).

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/machine"
	phelixmcp "github.com/abdorrahmani/phelix/internal/mcp"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpStdioEnvelope extracts the machine envelope from a tool result's single
// JSON text-content document.
func mcpStdioEnvelope(t *testing.T, res *mcpsdk.CallToolResult) *machine.Envelope {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("tool result has no content")
	}
	tc, ok := res.Content[0].(*mcpsdk.TextContent)
	if !ok {
		t.Fatalf("content[0] is %T, want *mcp.TextContent", res.Content[0])
	}
	var env machine.Envelope
	if err := json.Unmarshal([]byte(tc.Text), &env); err != nil {
		t.Fatalf("tool result content is not one machine envelope: %v\ncontent: %s", err, tc.Text)
	}
	return &env
}

func TestMCPStdioServer_AcceptanceWorkflow(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a subprocess; skipped under -short")
	}
	bin := phelixBin(t)
	home := t.TempDir()
	dataDir := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	proc := exec.Command(bin, "mcp", "serve", "--stdio")
	proc.Env = append(os.Environ(), "HOME="+home, "PHELIX_DATA_DIR="+dataDir)
	var stderr strings.Builder
	proc.Stderr = &stderr

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "phelix-itest", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &mcpsdk.CommandTransport{Command: proc}, nil)
	if err != nil {
		t.Fatalf("connect to stdio server: %v (stderr: %s)", err, stderr.String())
	}
	defer func() {
		_ = cs.Close()
		if proc.Process != nil {
			_ = proc.Process.Kill()
		}
	}()

	// 1. Discover the tool surface over the real protocol.
	lt, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	wantTools := []string{
		phelixmcp.ToolContext, phelixmcp.ToolInspect, phelixmcp.ToolPlanShow,
		phelixmcp.ToolPlanList, phelixmcp.ToolOperationStatus,
		phelixmcp.ToolPlanCreate, phelixmcp.ToolPlanApply,
	}
	got := map[string]bool{}
	for _, tl := range lt.Tools {
		got[tl.Name] = true
	}
	for _, name := range wantTools {
		if !got[name] {
			t.Fatalf("tool %q not advertised; got %v", name, got)
		}
	}

	// 2. A read tool returns a success envelope.
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: phelixmcp.ToolInspect, Arguments: map[string]any{"resource": "capabilities"}})
	if err != nil {
		t.Fatalf("inspect capabilities: %v", err)
	}
	if env := mcpStdioEnvelope(t, res); env.Status != machine.StatusSucceeded {
		t.Fatalf("inspect capabilities status = %q, want succeeded", env.Status)
	}

	// 3. The composite context read succeeds with no app named.
	res, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: phelixmcp.ToolContext, Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if env := mcpStdioEnvelope(t, res); env.Status != machine.StatusSucceeded {
		t.Fatalf("context status = %q, want succeeded", env.Status)
	}

	// 4. A malformed plan id fails closed as a coded failure envelope.
	res, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: phelixmcp.ToolPlanShow, Arguments: map[string]any{"plan_id": "not-a-plan"}})
	if err != nil {
		t.Fatalf("plan show (malformed): %v", err)
	}
	if !res.IsError {
		t.Fatal("malformed plan id must be a tool error (IsError)")
	}
	if env := mcpStdioEnvelope(t, res); env.Status != machine.StatusFailed || env.Error == nil || env.Error.Code != phelixerr.CodeInvalidArgument.String() {
		t.Fatalf("plan show (malformed) envelope wrong: %+v", env)
	}

	// 5. An unknown inspect resource is a tool error.
	res, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: phelixmcp.ToolInspect, Arguments: map[string]any{"resource": "nonsense"}})
	if err != nil {
		t.Fatalf("inspect (unknown resource) transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("unknown resource must be a tool error")
	}

	// Diagnostics went to stderr, never stdout (stdout decoded cleanly above).
	if !strings.Contains(stderr.String(), "serving on stdio") {
		t.Fatalf("expected a readiness diagnostic on stderr, got: %q", stderr.String())
	}
}
