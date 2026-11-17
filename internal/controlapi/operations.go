package controlapi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	pb "github.com/abdorrahmani/phelix/internal/controlapi/proto"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type Executor func(context.Context, *pb.CommandRequest, func(string, bool)) (int, error)

type operation struct {
	mu       sync.Mutex
	record   *pb.Operation
	progress []*pb.Progress
	changed  chan struct{}
	cancel   context.CancelFunc
}

type Operations struct {
	mu        sync.Mutex
	directory string
	jobs      map[string]*operation
	slots     chan struct{}
	executor  Executor
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closed    bool
}

func NewOperations(ctx context.Context, directory string, maximum int, executor Executor) (*Operations, error) {
	ctx, cancel := context.WithCancel(ctx)
	m := &Operations{directory: directory, jobs: map[string]*operation{}, slots: make(chan struct{}, maximum), executor: executor, ctx: ctx, cancel: cancel}
	if maximum < 1 || executor == nil {
		cancel()
		return nil, fmt.Errorf("operations require a positive limit and an executor")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		cancel()
		return nil, err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		cancel()
		return nil, err
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		record := &pb.Operation{}
		if err != nil || protojson.Unmarshal(raw, record) != nil || record.Id+".json" != entry.Name() {
			cancel()
			return nil, fmt.Errorf("unreadable Controller operation %s", entry.Name())
		}
		if !terminal(record.State) {
			record.State = pb.OperationState_INTERRUPTED
			record.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
			record.Error = &pb.Error{Code: "INTERRUPTED", Message: "Controller restarted; inspect deployment status before retrying"}
			if err := m.persist(record); err != nil {
				cancel()
				return nil, err
			}
		}
		m.jobs[record.Id] = &operation{record: record, changed: make(chan struct{})}
	}
	return m, nil
}

func terminal(state pb.OperationState) bool {
	return state == pb.OperationState_SUCCEEDED || state == pb.OperationState_FAILED || state == pb.OperationState_CANCELLED || state == pb.OperationState_INTERRUPTED
}

func (m *Operations) persist(record *pb.Operation) error {
	raw, err := protojson.Marshal(record)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(m.directory, ".operation-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	if _, err := temp.Write(raw); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(temp.Name(), filepath.Join(m.directory, record.Id+".json")); err != nil {
		return err
	}
	dir, err := os.Open(m.directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (m *Operations) Submit(request *pb.CommandRequest, cancellable bool) (*pb.Operation, error) {
	if request.TimeoutSeconds < 0 || request.TimeoutSeconds > 86400 {
		return nil, status.Error(codes.InvalidArgument, "timeout_seconds must be between 0 and 86400")
	}
	duration := time.Hour
	if request.TimeoutSeconds > 0 {
		duration = time.Duration(request.TimeoutSeconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(m.ctx, duration)
	record := &pb.Operation{Id: uuid.NewString(), State: pb.OperationState_QUEUED, Cancellable: cancellable, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	job := &operation{record: record, changed: make(chan struct{}), cancel: cancel}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		cancel()
		return nil, status.Error(codes.Unavailable, "Controller is shutting down")
	}
	if len(m.jobs) >= 10000 {
		cancel()
		return nil, status.Error(codes.ResourceExhausted, "Controller operation history is full; archive completed operation records during maintenance")
	}
	if err := m.persist(record); err != nil {
		cancel()
		return nil, status.Error(codes.Internal, "cannot persist operation")
	}
	m.jobs[record.Id] = job
	response := proto.Clone(record).(*pb.Operation)
	m.wg.Add(1)
	go func() { defer m.wg.Done(); defer cancel(); m.run(ctx, job, proto.Clone(request).(*pb.CommandRequest)) }()
	return response, nil
}

func (m *Operations) run(ctx context.Context, job *operation, request *pb.CommandRequest) {
	select {
	case m.slots <- struct{}{}:
		defer func() { <-m.slots }()
	case <-ctx.Done():
		m.finish(job, 1, ctx.Err())
		return
	}
	job.mu.Lock()
	job.record.State = pb.OperationState_RUNNING
	err := m.persist(job.record)
	job.notifyLocked("", false)
	job.mu.Unlock()
	if err != nil {
		m.finish(job, 1, fmt.Errorf("cannot persist running operation"))
		return
	}
	exit, err := m.executor(ctx, request, func(output string, stderr bool) {
		job.mu.Lock()
		defer job.mu.Unlock()
		output = phelixerr.Redact(output)
		for len(output) > 8192 {
			job.notifyLocked(output[:8192], stderr)
			output = output[8192:]
		}
		if output != "" {
			job.notifyLocked(output, stderr)
		}
	})
	m.finish(job, exit, err)
}

func (o *operation) notifyLocked(output string, stderr bool) {
	sequence := uint64(1)
	if len(o.progress) > 0 {
		sequence = o.progress[len(o.progress)-1].Sequence + 1
	}
	if len(output) > 8192 {
		output = output[len(output)-8192:]
	}
	o.progress = append(o.progress, &pb.Progress{Sequence: sequence, Output: output, Stderr: stderr, Operation: proto.Clone(o.record).(*pb.Operation)})
	if len(o.progress) > 512 {
		o.progress = o.progress[len(o.progress)-512:]
	}
	close(o.changed)
	o.changed = make(chan struct{})
}

func (m *Operations) finish(job *operation, exit int, err error) {
	job.mu.Lock()
	defer job.mu.Unlock()
	job.record.State = pb.OperationState_SUCCEEDED
	job.record.ExitCode = int32(exit)
	if err != nil || exit != 0 {
		job.record.State = pb.OperationState_FAILED
		if err == context.Canceled {
			job.record.State = pb.OperationState_CANCELLED
		}
		if err == context.DeadlineExceeded {
			job.record.State = pb.OperationState_INTERRUPTED
		}
		if err == nil {
			err = fmt.Errorf("operation failed with exit code %d", exit)
		}
		job.record.Error = &pb.Error{Code: string(phelixerr.CodeOf(err)), Message: phelixerr.Redact(err.Error())}
		if rendered, ok := err.(interface{ Display() string }); ok {
			job.record.Error.Display = phelixerr.Redact(rendered.Display())
		}
	}
	job.record.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := m.persist(job.record); err != nil {
		job.record.State = pb.OperationState_FAILED
		job.record.Error = &pb.Error{Code: "FILESYSTEM", Message: "operation settled but terminal record could not be persisted; inspect application status"}
	}
	job.notifyLocked("", false)
}

func (m *Operations) find(id string) (*operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[id]
	if job == nil {
		return nil, status.Error(codes.NotFound, "operation not found")
	}
	return job, nil
}

func (m *Operations) Get(id string) (*pb.Operation, error) {
	job, err := m.find(id)
	if err != nil {
		return nil, err
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	return proto.Clone(job.record).(*pb.Operation), nil
}

func (m *Operations) Cancel(id string) (*pb.Operation, error) {
	job, err := m.find(id)
	if err != nil {
		return nil, err
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	if !terminal(job.record.State) {
		if !job.record.Cancellable {
			return nil, status.Error(codes.FailedPrecondition, "operation cannot be cancelled safely")
		}
		job.cancel()
	}
	return proto.Clone(job.record).(*pb.Operation), nil
}

func (m *Operations) Watch(ctx context.Context, id string, after uint64, send func(*pb.Progress) error) error {
	job, err := m.find(id)
	if err != nil {
		return err
	}
	for {
		job.mu.Lock()
		changed := job.changed
		var updates []*pb.Progress
		for _, event := range job.progress {
			if event.Sequence > after {
				updates = append(updates, proto.Clone(event).(*pb.Progress))
			}
		}
		complete := terminal(job.record.State)
		record := proto.Clone(job.record).(*pb.Operation)
		job.mu.Unlock()
		for _, event := range updates {
			if err := send(event); err != nil {
				return err
			}
			after = event.Sequence
		}
		if complete {
			if len(updates) == 0 {
				return send(&pb.Progress{Sequence: after + 1, Operation: record})
			}
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (m *Operations) Close() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.cancel()
	m.wg.Wait()
}
