package grpc

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/matrix"
	"github.com/abdorrahmani/phelix/internal/server"
	"google.golang.org/protobuf/proto"
)

// Matrix event reporting: one MatrixEvent per lifecycle transition of a
// remote matrix execution, delivered through a single background sender
// goroutine with a cached connection — the same delivery model as the
// rollback and deployment reporters. Events are best-effort telemetry: the
// buffer is bounded and a full buffer drops events (the durable outcome of a
// matrix command is its ledger-backed MonitorCommandResult, and current state
// is re-queryable with a matrix_status command).
//
// MatrixReporter is NOT safe for concurrent use by itself, but its event
// funnel is: every emit goes through enqueueMatrixEvent, which deep-copies
// the message and hands it to the single sender goroutine.

// Matrix execution modes (MatrixEvent.mode).
const (
	MatrixModeNative = "native"
	MatrixModeDocker = "docker"
)

// Matrix event types (MatrixEvent.event).
const (
	MatrixEventStarted              = "matrix.started"
	MatrixEventResumed              = "matrix.resumed"
	MatrixEventCombinationStarted   = "matrix.combination_started"
	MatrixEventCombinationCompleted = "matrix.combination_completed"
	MatrixEventCompleted            = "matrix.completed"
	MatrixEventInterrupted          = "matrix.interrupted"
)

// matrixEventBuffer bounds the queue before events are dropped. A large
// matrix emits two events per combination plus retries, so this covers a
// 100-combination run with headroom.
const matrixEventBuffer = 256

// MatrixReporter emits the lifecycle events of ONE remote matrix execution
// (one command). It is created by the remote matrix handlers in the cmd
// package and is driven from executor worker goroutines; seq makes the event
// order reconstructible on the backend.
type MatrixReporter struct {
	requestID string
	runID     string // empty for docker builds
	appName   string
	mode      string

	// seq is incremented per emitted event. The reporter's methods are called
	// from concurrent executor workers, so the counter is atomic; event order
	// on the wire is still a total order because every event passes through
	// the single sender channel with its seq stamped at emit time.
	seq atomic.Int64
}

// NewMatrixReporter creates the reporter for one remote matrix command.
func NewMatrixReporter(requestID, runID, appName, mode string) *MatrixReporter {
	return &MatrixReporter{requestID: requestID, runID: runID, appName: appName, mode: mode}
}

// emit stamps identity, ordering and timestamp, then queues the event.
func (r *MatrixReporter) emit(event *pb.MatrixEvent) {
	if r == nil {
		return
	}
	event.ServerId = server.GetServerID()
	event.RequestId = r.requestID
	event.MatrixRunId = r.runID
	event.AppName = r.appName
	event.Mode = r.mode
	event.Seq = r.seq.Add(1)
	event.Timestamp = time.Now().UnixMilli()
	enqueueMatrixEvent(event)
}

// RunStarted reports that a native Matrix Run began executing (fresh build
// or first session). The full initial state rides along.
func (r *MatrixReporter) RunStarted(run *matrix.Run) {
	r.emit(&pb.MatrixEvent{
		Event:    MatrixEventStarted,
		Status:   runStatusOrEmpty(run),
		Message:  "matrix run started",
		Counters: countersFromRun(run),
		Run:      ToProtoMatrixRunState(run),
	})
}

// RunResumed reports that a resume session began on an existing run.
func (r *MatrixReporter) RunResumed(run *matrix.Run) {
	r.emit(&pb.MatrixEvent{
		Event:    MatrixEventResumed,
		Status:   runStatusOrEmpty(run),
		Message:  "matrix run resumed",
		Counters: countersFromRun(run),
		Run:      ToProtoMatrixRunState(run),
	})
}

// AttemptStarted reports that one attempt of one combination began. attempt
// >= 2 marks an automatic retry.
func (r *MatrixReporter) AttemptStarted(c matrix.Combination, attempt int) {
	r.emit(&pb.MatrixEvent{
		Event:   MatrixEventCombinationStarted,
		Attempt: int32(attempt),
		Combination: &pb.MatrixCombinationState{
			Id:               c.ID(),
			Toolchain:        string(c.Lang),
			ToolchainVersion: c.Version,
			Os:               c.OS,
			Arch:             c.Arch,
			Variant:          c.Variant,
			Platform:         c.Platform,
			Status:           "running",
			Attempts:         int32(attempt),
		},
	})
}

// ResultRecorded reports that one combination reached a final outcome. The
// combination carries the full result: status, duration, artifact, checksum,
// cache classification, attempts and the complete attempt history.
func (r *MatrixReporter) ResultRecorded(res matrix.Result) {
	r.emit(&pb.MatrixEvent{
		Event:       MatrixEventCombinationCompleted,
		Combination: ToProtoMatrixCombinationFromResult(res),
	})
}

// RunFinished reports the terminal state of a native run: matrix.completed
// for terminal statuses (succeeded/partial/failed — status disambiguates) or
// matrix.interrupted for a run stopped early (resumable). The full final
// state (including the release view) rides along.
func (r *MatrixReporter) RunFinished(run *matrix.Run, interrupted bool) {
	if run == nil {
		return
	}
	if interrupted {
		r.emit(&pb.MatrixEvent{
			Event:    MatrixEventInterrupted,
			Status:   runStatusOrEmpty(run),
			Message:  "matrix run interrupted — incomplete combinations remain and the run is resumable",
			Counters: countersFromRun(run),
			Run:      ToProtoMatrixRunState(run),
		})
		return
	}
	r.emit(&pb.MatrixEvent{
		Event:    MatrixEventCompleted,
		Status:   runStatusOrEmpty(run),
		Message:  matrixFinishMessage(run),
		Counters: countersFromRun(run),
		Run:      ToProtoMatrixRunState(run),
	})
}

// DockerStarted reports that a docker matrix build session began. There is
// no Matrix Run — metadata carries the docker options and counters the plan
// size.
func (r *MatrixReporter) DockerStarted(plan *matrix.MatrixPlan, meta map[string]string) {
	total := 0
	if plan != nil {
		total = len(plan.Combinations)
	}
	r.emit(&pb.MatrixEvent{
		Event:    MatrixEventStarted,
		Message:  "docker matrix build started",
		Counters: &pb.MatrixCounters{Total: int32(total), Pending: int32(total)},
		Metadata: copyStringMapForWire(meta),
	})
}

// DockerFinished reports the terminal outcome of a docker matrix build from
// its results (the same status derivation a run applies).
func (r *MatrixReporter) DockerFinished(results []matrix.Result) {
	var succeeded, failed, skipped int
	for _, res := range results {
		switch res.Status {
		case "success":
			succeeded++
		case "failed":
			failed++
		case "skipped":
			skipped++
		}
	}
	counters := &pb.MatrixCounters{
		Total:     int32(len(results)),
		Succeeded: int32(succeeded),
		Failed:    int32(failed),
		Skipped:   int32(skipped),
	}
	status := "failed"
	switch {
	case failed == 0:
		status = "succeeded"
	case succeeded > 0 || skipped > 0:
		status = "partial"
	}
	r.emit(&pb.MatrixEvent{
		Event:    MatrixEventCompleted,
		Status:   status,
		Message:  matrixFinishMessage(nil),
		Counters: counters,
	})
}

func runStatusOrEmpty(run *matrix.Run) string {
	if run == nil {
		return ""
	}
	return string(run.Status)
}

func matrixFinishMessage(run *matrix.Run) string {
	if run == nil {
		return "docker matrix build finished"
	}
	switch run.Status {
	case matrix.RunStatusSucceeded:
		return "matrix run completed successfully"
	case matrix.RunStatusPartial:
		return "matrix run completed with failures"
	default:
		return "matrix run finished"
	}
}

func copyStringMapForWire(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ---------------------------------------------------------------------------
// Background event sender
// ---------------------------------------------------------------------------

var (
	matrixEventCh   chan *pb.MatrixEvent
	matrixEventOnce sync.Once
	matrixSenderMu  sync.Mutex // protects init + flush
	matrixSenderDn  chan struct{}
)

func initMatrixSender() {
	matrixSenderMu.Lock()
	defer matrixSenderMu.Unlock()

	matrixEventOnce.Do(func() {
		matrixEventCh = make(chan *pb.MatrixEvent, matrixEventBuffer)
		matrixSenderDn = make(chan struct{})
		go matrixSenderLoop()
		logs.InfoFile("grpc", "[gRPC] Matrix sender: background worker started")
	})
}

// matrixSenderLoop drains queued events, reusing one connection for a batch
// so a matrix build does not pay a dial per event.
func matrixSenderLoop() {
	defer close(matrixSenderDn)

	var (
		client    *Client
		sent      int
		dropped   int
		ensureCli = func() *Client {
			if client != nil && client.IsConnected() {
				return client
			}
			if client != nil {
				client.Close()
				client = nil
			}
			c := NewClient()
			if err := c.Connect(); err != nil {
				logs.ErrorFile("grpc", "[gRPC] Matrix sender: connect failed: %v", err)
				return nil
			}
			client = c
			return client
		}
	)

	for ev := range matrixEventCh {
		sanitizeMatrixEventStrings(ev)
		c := ensureCli()
		if c == nil {
			dropped++
			logs.WarningFile("grpc", "[gRPC] Matrix sender: no connection, dropping %s seq=%d (dropped=%d)", ev.GetEvent(), ev.GetSeq(), dropped)
			continue
		}
		if err := c.SendMatrixEvent(ev); err != nil {
			dropped++
			logs.ErrorFile("grpc", "[gRPC] Matrix sender: send failed for %s seq=%d: %v (dropped=%d)", ev.GetEvent(), ev.GetSeq(), err, dropped)
			if client != nil {
				client.Close()
				client = nil
			}
			continue
		}
		sent++
	}

	if client != nil {
		client.Close()
	}
	logs.InfoFile("grpc", "[gRPC] Matrix sender: stopped (sent=%d, dropped=%d)", sent, dropped)
}

// StopMatrixSender drains the event channel and waits for the background
// goroutine to finish (bounded by timeout). Called at daemon shutdown after
// in-flight matrix commands settled, so terminal events are flushed before
// the connection closes. Safe to call even if no sender was started.
func StopMatrixSender(timeout time.Duration) {
	matrixSenderMu.Lock()
	ch := matrixEventCh
	done := matrixSenderDn
	matrixSenderMu.Unlock()

	if ch == nil || done == nil {
		return
	}

	close(ch)
	matrixEventCh = nil
	matrixSenderDn = nil
	matrixEventOnce = sync.Once{}

	select {
	case <-done:
	case <-time.After(timeout):
		logs.WarningFile("grpc", "[gRPC] Matrix sender: flush timed out after %s", timeout)
	}
}

// matrixEventInterceptor, when set, receives every event instead of the
// queue. It exists for tests that need to observe emission without a
// backend connection; production leaves it nil.
var matrixEventInterceptor atomic.Value // func(*pb.MatrixEvent)

// SetMatrixEventInterceptor installs a test hook receiving every emitted
// matrix event before queueing. Passing nil restores normal delivery.
func SetMatrixEventInterceptor(fn func(*pb.MatrixEvent)) {
	if fn == nil {
		matrixEventInterceptor.Store((func(*pb.MatrixEvent))(nil))
		return
	}
	matrixEventInterceptor.Store(fn)
}

// enqueueMatrixEvent hands the event to the background sender. Non-blocking:
// a full buffer drops the event (logged; the command result and matrix_status
// remain the durable state surfaces).
func enqueueMatrixEvent(event *pb.MatrixEvent) {
	if fn, ok := matrixEventInterceptor.Load().(func(*pb.MatrixEvent)); ok && fn != nil {
		fn(proto.Clone(event).(*pb.MatrixEvent))
		return
	}
	initMatrixSender()
	select {
	case matrixEventCh <- proto.Clone(event).(*pb.MatrixEvent):
	default:
		logs.WarningFile("grpc", "[gRPC] Matrix event channel full, DROPPED %s seq=%d run=%s", event.GetEvent(), event.GetSeq(), event.GetMatrixRunId())
	}
}

// SendMatrixEvent delivers one lifecycle event to the backend.
func (c *Client) SendMatrixEvent(event *pb.MatrixEvent) error {
	if !c.IsConnected() {
		return phelixerr.New(phelixerr.CodeConnection, "dashboard backend not connected")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	serverID := server.GetServerID()
	authCtx, err := attachAuthMetadata(ctx, serverID)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeUnauthenticated, "no valid session for the dashboard backend", err)
	}

	resp, err := c.serviceClient.ReportMatrixEvent(authCtx, event)
	if err != nil {
		c.reconnectIfNeeded()
		return phelixerr.Wrap(phelixerr.CodeGRPC, "failed to report matrix event", err)
	}
	if !resp.Accepted {
		return phelixerr.Newf(phelixerr.CodeServer, "dashboard refused the matrix event: %s", resp.Message)
	}
	return nil
}

// sanitizeMatrixEventStrings replaces invalid UTF-8 in every string field so
// the backend cannot reject the RPC over wire-format violations (compiler
// output fragments are the usual culprit). Same treatment as rollback events.
func sanitizeMatrixEventStrings(e *pb.MatrixEvent) {
	e.ServerId = sanitizeUTF8(e.ServerId)
	e.RequestId = sanitizeUTF8(e.RequestId)
	e.MatrixRunId = sanitizeUTF8(e.MatrixRunId)
	e.AppName = sanitizeUTF8(e.AppName)
	e.Event = sanitizeUTF8(e.Event)
	e.Mode = sanitizeUTF8(e.Mode)
	e.Status = sanitizeUTF8(e.Status)
	e.Message = sanitizeUTF8(e.Message)
	e.Error = sanitizeUTF8(e.Error)
	e.ErrorCode = sanitizeUTF8(e.ErrorCode)
	for k, v := range e.Metadata {
		e.Metadata[k] = sanitizeUTF8(v)
	}
	if e.Combination != nil {
		sanitizeMatrixCombinationStrings(e.Combination)
	}
	if e.Run != nil {
		sanitizeMatrixRunStateStrings(e.Run)
	}
}

func sanitizeMatrixCombinationStrings(c *pb.MatrixCombinationState) {
	c.Id = sanitizeUTF8(c.Id)
	c.Identity = sanitizeUTF8(c.Identity)
	c.Toolchain = sanitizeUTF8(c.Toolchain)
	c.ToolchainVersion = sanitizeUTF8(c.ToolchainVersion)
	c.Os = sanitizeUTF8(c.Os)
	c.Arch = sanitizeUTF8(c.Arch)
	c.Variant = sanitizeUTF8(c.Variant)
	c.Platform = sanitizeUTF8(c.Platform)
	c.Status = sanitizeUTF8(c.Status)
	c.Artifact = sanitizeUTF8(c.Artifact)
	c.Sha256 = sanitizeUTF8(c.Sha256)
	c.Error = sanitizeUTF8(c.Error)
	c.CacheStatus = sanitizeUTF8(c.CacheStatus)
	for k, v := range c.Metadata {
		c.Metadata[k] = sanitizeUTF8(v)
	}
	for _, a := range c.AttemptLog {
		a.Status = sanitizeUTF8(a.Status)
		a.Error = sanitizeUTF8(a.Error)
	}
}

func sanitizeMatrixRunStateStrings(s *pb.MatrixRunState) {
	s.MatrixRunId = sanitizeUTF8(s.MatrixRunId)
	s.AppName = sanitizeUTF8(s.AppName)
	s.ProjectDir = sanitizeUTF8(s.ProjectDir)
	s.Status = sanitizeUTF8(s.Status)
	s.ParentRunId = sanitizeUTF8(s.ParentRunId)
	for _, c := range s.Combinations {
		sanitizeMatrixCombinationStrings(c)
	}
	if s.Release != nil {
		s.Release.Tag = sanitizeUTF8(s.Release.Tag)
		s.Release.Status = sanitizeUTF8(s.Release.Status)
	}
}
