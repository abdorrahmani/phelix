package mcp

import (
	"context"
	"io"
	"log/slog"
	"os"

	"github.com/abdorrahmani/phelix/internal/version"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// serverName is the MCP implementation name reported in the initialize
	// handshake. Stable identifier, not a display string.
	serverName = "phelix"

	// maxFrameBytes bounds a single inbound JSON-RPC frame. Tool arguments are
	// tiny (ids, names, bounded ints); 1 MiB is already far more than any
	// legitimate call, and an unbounded frame is a memory-exhaustion surface on
	// an untrusted input path.
	maxFrameBytes = 1 << 20
)

// serverInstructions is the optional guidance returned to clients at
// initialize. It states the trust boundary plainly so an agent does not assume
// mutations are unconditionally permitted.
const serverInstructions = "Phelix exposes controlled execution for coding agents. " +
	"Read tools (phelix_context, phelix_inspect, phelix_plan_show, phelix_plan_list, " +
	"phelix_operation_status) are side-effect free. Mutations are two-step: " +
	"phelix_plan_create produces an immutable plan (no execution), and phelix_plan_apply " +
	"executes a plan only after the host's authorization boundary allows it (which may " +
	"require a plan-bound approval created out-of-band). Every result is a Phelix machine " +
	"envelope; a failed status with an error.code is a normal, expected outcome, not a crash."

// Server is the Phelix MCP adapter: the SDK server plus the Services seam it
// delegates every tool to. It owns no deployment logic.
type Server struct {
	services Services
	sdk      *mcpsdk.Server
}

// Options configures a Server.
type Options struct {
	// Diagnostics receives SDK/server logs. It MUST NOT be the real stdout —
	// stdout carries the JSON-RPC protocol. Defaults to os.Stderr.
	Diagnostics io.Writer
	// Version is reported to clients in the initialize handshake. Defaults to
	// the build's version.Version.
	Version string
}

// New builds the adapter over a Services implementation, registers the tool
// surface, and returns it ready to Run. It performs no I/O.
func New(services Services, opts Options) *Server {
	if opts.Version == "" {
		opts.Version = version.Version
	}
	diag := opts.Diagnostics
	if diag == nil {
		diag = os.Stderr
	}
	// Warn level keeps the stdio diagnostics stream quiet during normal
	// operation; the protocol stream is a separate fd the SDK captured at Run.
	logger := slog.New(slog.NewTextHandler(diag, &slog.HandlerOptions{Level: slog.LevelWarn}))

	impl := &mcpsdk.Implementation{
		Name:    serverName,
		Title:   "Phelix",
		Version: opts.Version,
		Description: "Controlled execution and deployment for coding agents: context and " +
			"inspection reads, immutable plan create/show/list, plan apply, and operation " +
			"status. Mutations pass the same authorization, approval and idempotency " +
			"boundary as the Phelix CLI.",
	}
	// KeepAlive left zero: no background pings, so nothing writes to the
	// protocol stream outside the request/response cycle.
	sdk := mcpsdk.NewServer(impl, &mcpsdk.ServerOptions{
		Logger:       logger,
		Instructions: serverInstructions,
	})

	s := &Server{services: services, sdk: sdk}
	s.registerTools()
	return s
}

// Run serves the MCP protocol over stdio until ctx is cancelled or the client
// disconnects. The SDK's StdioTransport binds the real os.Stdin/os.Stdout at
// connect time (now), so a captured command detouring the os.Stdout variable
// to stderr later never touches this protocol stream — the same invariant
// machine.EnterJSON relies on.
func (s *Server) Run(ctx context.Context) error {
	return s.sdk.Run(ctx, &mcpsdk.StdioTransport{MaxLineLength: maxFrameBytes})
}
