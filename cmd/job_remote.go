package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/authz"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/job"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/abdorrahmani/phelix/internal/server"
	"github.com/robfig/cron/v3"
)

// Remote job management runs on the existing job engine: the same
// phelix.yaml jobs, durable store, executor and scheduler the local CLI uses.
// The remote layer only resolves targets, validates shape, emits lifecycle
// observations and converts records to proto.

// jobAcceptTimeout bounds how long the transport waits for the durable
// acceptance of a manual run before answering UNAVAILABLE.
const jobAcceptTimeout = 10 * time.Second

// RemoteJobCommand executes one validated job_* command.
func RemoteJobCommand(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
	switch req.GetType() {
	case phelixgrpc.JobCommandList:
		return remoteJobList(req)
	case phelixgrpc.JobCommandRun:
		return remoteJobRun(req)
	case phelixgrpc.JobCommandHistory:
		return remoteJobHistory(req)
	case phelixgrpc.JobCommandLogs:
		return remoteJobLogs(req)
	default:
		return remoteJobError(phelixerr.Newf(phelixerr.CodeInvalidArgument, "unknown job command type %q", req.GetType()))
	}
}

// remoteJobTarget resolves the app and enforces that it is a real type: job
// app, then verifies an optional job_name against it (one job per app).
func remoteJobTarget(req *pb.MonitorCommandRequest) (jobInfo *jobTargetInfo, err error) {
	info, cfg, err := jobTarget(req.GetAppName())
	if err != nil {
		return nil, err
	}
	if name := req.GetJob().GetJobName(); name != "" && name != info.Name {
		return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"job_name %q does not match the target job %q", name, info.Name)
	}
	return &jobTargetInfo{info: info, cfg: cfg}, nil
}

// jobTargetInfo keeps the resolved app/config pair small at call sites.
type jobTargetInfo struct {
	info *app.AppInfo
	cfg  *project.Config
}

func remoteJobList(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
	target, err := remoteJobTarget(req)
	if err != nil {
		return remoteJobError(err)
	}
	definition, err := jobDefinition(target.info.Name, target.info.ID, target.cfg)
	if err != nil {
		return remoteJobError(err)
	}
	return &pb.MonitorCommandResult{Status: "success", Jobs: []*pb.JobDefinition{definition}}
}

func remoteJobRun(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
	target, err := remoteJobTarget(req)
	if err != nil {
		return remoteJobError(err)
	}
	settings := target.cfg.Job.Resolve()
	executor := job.NewExecutor(newRemoteJobStore())

	accepted := make(chan *job.Record, 1)
	executor.OnAccepted = func(record *job.Record) { accepted <- record }

	runErr := make(chan error, 1)
	go func() {
		_, err := executor.Run(context.Background(), job.RunRequest{
			AppName:     target.info.Name,
			AppID:       target.info.ID,
			Dir:         target.info.Directory,
			Command:     settings.Command,
			Timeout:     settings.Timeout,
			Retries:     settings.Retries,
			Concurrency: settings.ConcurrencyPolicy,
			Trigger:     job.TriggerManual,
			Version:     currentJobVersion(target.info.Name),
		})
		runErr <- err
	}()

	select {
	case record := <-accepted:
		return &pb.MonitorCommandResult{Status: "success", Execution: jobRecordProto(record, target.info.Name, target.info.ID)}
	case err := <-runErr:
		if err != nil {
			if errors.Is(err, job.ErrRunLockHeld) {
				return remoteJobError(phelixerr.Newf(phelixerr.CodeUnavailable,
					"job %q already has a run in progress", target.info.Name))
			}
			return remoteJobError(err)
		}
		if record := <-accepted; record != nil {
			return &pb.MonitorCommandResult{Status: "success", Execution: jobRecordProto(record, target.info.Name, target.info.ID)}
		}
		return remoteJobError(phelixerr.New(phelixerr.CodeUnknown, "job run finished without a durable record"))
	case <-time.After(jobAcceptTimeout):
		return remoteJobError(phelixerr.Newf(phelixerr.CodeTimeout,
			"job %q was not accepted within %s; check job_history before retrying", target.info.Name, jobAcceptTimeout))
	}
}

func remoteJobHistory(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
	target, err := remoteJobTarget(req)
	if err != nil {
		return remoteJobError(err)
	}
	store := newRemoteJobStore()
	opts := req.GetJob()
	records := make([]*job.Record, 0)
	if id := opts.GetExecutionId(); id != "" {
		record, err := store.Load(target.info.Name, id)
		if err != nil {
			return remoteJobError(err)
		}
		records = append(records, record)
	} else {
		limit := opts.GetLimit()
		if limit == 0 {
			limit = phelixgrpc.JobHistoryLimitDefault
		}
		records, err = store.List(target.info.Name, int(limit))
		if err != nil {
			return remoteJobError(err)
		}
	}
	executions := make([]*pb.JobExecutionRecord, 0, len(records))
	for _, record := range records {
		executions = append(executions, jobRecordProto(record, target.info.Name, target.info.ID))
	}
	return &pb.MonitorCommandResult{Status: "success", Executions: executions}
}

func remoteJobLogs(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
	target, err := remoteJobTarget(req)
	if err != nil {
		return remoteJobError(err)
	}
	opts := req.GetJob()
	store := newRemoteJobStore()
	if _, err := store.Load(target.info.Name, opts.GetExecutionId()); err != nil {
		return remoteJobError(err)
	}
	path, err := store.LogPath(target.info.Name, opts.GetExecutionId())
	if err != nil {
		return remoteJobError(err)
	}
	chunk, err := readJobLog(path, opts.GetExecutionId(), opts.GetOffset(), int(opts.GetMaxBytes()))
	if err != nil {
		return remoteJobError(err)
	}
	return &pb.MonitorCommandResult{Status: "success", JobLog: chunk}
}

// newRemoteJobStore builds the store the remote commands share with the
// scheduler. Lifecycle observations are attached here so both manual and
// scheduled runs report through the same channel.
func newRemoteJobStore() *job.Store {
	store := job.NewStore(server.DataDir())
	store.Events = emitJobLifecycle
	return store
}

// emitJobLifecycle maps one engine event to the wire event. Observations are
// best-effort; the durable record is the source of truth.
func emitJobLifecycle(event job.Event) {
	phelixgrpc.SendJobLifecycleEvent(&pb.JobLifecycleEvent{
		AppId:       event.AppID,
		JobName:     event.AppName,
		ExecutionId: event.ExecutionID,
		Trigger:     string(event.Trigger),
		Phase:       string(event.Phase),
		Status:      string(event.Status),
		Timestamp:   event.Timestamp.UnixMilli(),
		Attempt:     int32(event.Attempt),
		Error:       event.Error,
		ErrorCode:   event.ErrorCode,
		ScheduledAt: eventScheduledAt(event),
	})
}

func eventScheduledAt(event job.Event) int64 {
	if event.ScheduledFor == nil {
		return 0
	}
	return event.ScheduledFor.UnixMilli()
}

// jobDefinition builds the overview answer from the same configuration and
// the newest durable records.
func jobDefinition(name, id string, cfg *project.Config) (*pb.JobDefinition, error) {
	schedule := cfg.Schedule.Resolve()
	settings := cfg.Job.Resolve()
	definition := &pb.JobDefinition{
		AppId:             id,
		JobName:           name,
		WorkloadType:      project.TypeJob,
		Cron:              schedule.Cron,
		Timezone:          schedule.Timezone,
		Command:           settings.Command,
		TimeoutMs:         settings.Timeout.Milliseconds(),
		Retries:           int32(settings.Retries),
		ConcurrencyPolicy: settings.ConcurrencyPolicy,
		CurrentStatus:     "idle",
	}

	store := newRemoteJobStore()
	records, err := store.List(name, 0)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if record.Status == job.StatusRunning && definition.RunningExecutionId == "" {
			definition.RunningExecutionId = record.ID
			definition.CurrentStatus = string(job.StatusRunning)
		}
		if definition.LastExecutionStatus == "" && record.Status.Finished() {
			definition.LastExecutionStatus = string(record.Status)
			definition.LastExecutionAt = jobRecordTimestamp(record)
		}
		if definition.RunningExecutionId != "" && definition.LastExecutionStatus != "" {
			break
		}
	}

	if spec, err := cron.ParseStandard(strings.TrimSpace(schedule.Cron)); err == nil {
		location := schedule.Location
		if location == nil {
			location = time.UTC
		}
		definition.NextScheduledAt = spec.Next(time.Now().In(location)).UnixMilli()
	}
	return definition, nil
}

func jobRecordProto(record *job.Record, appName, appID string) *pb.JobExecutionRecord {
	out := &pb.JobExecutionRecord{
		ExecutionId: record.ID,
		JobName:     appName,
		AppId:       appID,
		Trigger:     string(record.Trigger),
		Status:      string(record.Status),
		ScheduledAt: eventScheduledAt(job.Event{ScheduledFor: record.ScheduledFor}),
		StartedAt:   record.StartedAt.UnixMilli(),
		DurationMs:  record.DurationMS,
		Attempts:    int32(record.Attempts),
		ExitCode:    int32(record.ExitCode),
		Error:       record.Error,
	}
	if record.AppID != "" {
		out.AppId = record.AppID
	}
	if record.FinishedAt != nil {
		out.FinishedAt = record.FinishedAt.UnixMilli()
	}
	return out
}

func jobRecordTimestamp(record *job.Record) int64 {
	if record.FinishedAt != nil {
		return record.FinishedAt.UnixMilli()
	}
	return record.StartedAt.UnixMilli()
}

// readJobLog returns one bounded, offset-paged page of an execution log. The
// page always ends on a line boundary so a following page never starts
// mid-line.
func readJobLog(path, executionID string, offset int64, maxBytes int) (*pb.JobLogChunk, error) {
	chunk := &pb.JobLogChunk{ExecutionId: executionID}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		chunk.Eof = true
		return chunk, nil
	}
	if err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "job: open log for %s", executionID)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "job: stat log for %s", executionID)
	}
	size := info.Size()
	chunk.TotalBytes = size
	if offset > size {
		return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"job log offset %d is beyond the %d-byte log", offset, size)
	}
	if maxBytes <= 0 {
		maxBytes = phelixgrpc.JobLogBytesDefault
	}
	if offset == size {
		chunk.Offset = offset
		chunk.NextOffset = offset
		chunk.Eof = true
		return chunk, nil
	}

	window := int64(maxBytes)
	if remaining := size - offset; remaining < window {
		window = remaining
	}
	buf := make([]byte, window)
	n, err := f.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "job: read log for %s", executionID)
	}
	buf = buf[:n]

	consumed := len(buf)
	if offset+int64(consumed) < size {
		if idx := bytes.LastIndexByte(buf, '\n'); idx >= 0 {
			consumed = idx + 1
		} else {
			return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"job log line exceeds max_bytes %d; request a larger page", maxBytes)
		}
	}

	chunk.Offset = offset
	chunk.NextOffset = offset + int64(consumed)
	chunk.Eof = chunk.NextOffset >= size
	for _, line := range strings.Split(string(buf[:consumed]), "\n") {
		if line == "" {
			continue
		}
		chunk.Entries = append(chunk.Entries, parseJobLogLine(line))
	}
	return chunk, nil
}

// parseJobLogLine reads the engine's standard log line shape:
// "2006/01/02 15:04:05 [LEVEL] [stream] message".
func parseJobLogLine(line string) *pb.JobLogEntry {
	entry := &pb.JobLogEntry{}
	rest := strings.TrimSuffix(line, "\r")
	if len(rest) > 19 && rest[19] == ' ' {
		if parsed, err := time.Parse("2006/01/02 15:04:05", rest[:19]); err == nil {
			entry.Timestamp = parsed.UnixMilli()
			rest = strings.TrimSpace(rest[20:])
		}
	}
	if level, remainder, ok := jobLogMarker(rest); ok {
		entry.Level = level
		rest = remainder
	}
	if stream, remainder, ok := jobLogMarker(rest); ok && (stream == "stdout" || stream == "stderr") {
		entry.Stream = stream
		rest = remainder
	}
	entry.Message = rest
	return entry
}

func jobLogMarker(rest string) (value, remainder string, ok bool) {
	if !strings.HasPrefix(rest, "[") {
		return "", rest, false
	}
	end := strings.IndexByte(rest, ']')
	if end <= 1 {
		return "", rest, false
	}
	return rest[1:end], strings.TrimSpace(rest[end+1:]), true
}

// remoteJobError builds a minimal error result; the dispatcher stamps the
// identity fields and redacts the message.
func remoteJobError(err error) *pb.MonitorCommandResult {
	return &pb.MonitorCommandResult{
		Status:    "error",
		Error:     err.Error(),
		ErrorCode: string(phelixerr.CodeOf(err)),
	}
}

// remoteJobAuthorize gates job_run through the host authorization boundary
// with the dedicated job action, before any ledger entry or execution.
func remoteJobAuthorize(req *pb.MonitorCommandRequest) error {
	info, err := GetAppInfo(req.GetAppName())
	if err != nil {
		return err
	}
	return authorizeDirectExecution(authz.ActionJob, info.Name, info.ID)
}
