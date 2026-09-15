package webhook

import (
	"context"
	"errors"
	"sync"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/logs"
)

// Job is one accepted webhook delivery handed to the build queue. It carries
// the push metadata the pipeline must preserve: the pushed commit SHA is
// recorded as webhook metadata (ledger + logs + events), never silently
// presented as the state the rebuild compiled.
type Job struct {
	AppID      string
	AppName    string
	Directory  string
	Branch     string
	CommitSHA  string
	DeliveryID string
	Provider   string
	ReceivedAt time.Time
}

// ErrShutdown marks jobs abandoned because the webhook server is shutting
// down. It is not a rebuild failure: a rebuild subprocess that already started
// keeps running to completion on its own.
var ErrShutdown = errors.New("webhook: server shutdown while job was in flight")

// IsShutdownErr reports whether err stems from the server shutting down
// rather than the rebuild failing. Shutdown-caused outcomes are logged but
// never reported as rebuild failures.
func IsShutdownErr(err error) bool {
	return errors.Is(err, ErrShutdown) || errors.Is(err, context.Canceled)
}

// RebuildFunc performs the rebuild for one job. The production implementation
// (CliRebuild) delegates to the existing `phelix rebuild` pipeline; it owns no
// build, deploy, health, rollback or versioning logic of its own.
type RebuildFunc func(ctx context.Context, job *Job) error

// perAppQueueDepth bounds the accepted-but-not-started backlog per app.
const perAppQueueDepth = 8

// Queue is the webhook build queue: jobs for the same application execute
// strictly one at a time (each app has its own worker), while different
// applications execute independently. Workers never bypass or steal the
// per-app deploy lock — that stays authoritative inside the rebuild pipeline.
type Queue struct {
	rebuild      RebuildFunc
	onJobDone    func(job *Job, err error)
	onJobDropped func(job *Job)

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	workers map[string]*appWorker
	closed  bool
}

// appWorker serializes jobs for one application.
type appWorker struct {
	app  string
	jobs chan *Job
	stop chan struct{}
}

// QueueOptions wires the queue. Rebuild is required; the callbacks are
// optional observability hooks (event emission).
type QueueOptions struct {
	Rebuild RebuildFunc
	// OnJobDone is called when a job's rebuild returns (success or failure).
	OnJobDone func(job *Job, err error)
	// OnJobDropped is called for jobs that were accepted but never started
	// because the server shut down.
	OnJobDropped func(job *Job)
}

// NewQueue creates the queue. Close must be called to release its workers.
func NewQueue(opts QueueOptions) *Queue {
	if opts.Rebuild == nil {
		panic("webhook: queue requires a Rebuild func")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Queue{
		rebuild:      opts.Rebuild,
		onJobDone:    opts.OnJobDone,
		onJobDropped: opts.OnJobDropped,
		ctx:          ctx,
		cancel:       cancel,
		workers:      make(map[string]*appWorker),
	}
}

// ErrQueueFull is returned when the per-app backlog is exhausted; the handler
// answers 503 so the provider redelivers later.
var ErrQueueFull = phelixerr.New(phelixerr.CodeUnavailable, "webhook queue is full")

// ErrQueueClosed is returned after Close; the handler answers 503.
var ErrQueueClosed = phelixerr.New(phelixerr.CodeUnavailable, "webhook queue is closed")

// Enqueue hands a job to its app's worker. It returns as soon as the job is
// buffered — never after the rebuild — so the HTTP response never waits for
// the pipeline.
func (q *Queue) Enqueue(job *Job) error {
	// The send happens under the queue mutex so it cannot interleave with
	// Close flipping q.closed and closing the workers' stop channels.
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return ErrQueueClosed
	}
	w := q.workers[job.AppName]
	if w == nil {
		w = &appWorker{
			app:  job.AppName,
			jobs: make(chan *Job, perAppQueueDepth),
			stop: make(chan struct{}),
		}
		q.workers[job.AppName] = w
		q.wg.Add(1)
		go q.run(w)
	}
	select {
	case w.jobs <- job:
		return nil
	default:
		return ErrQueueFull
	}
}

// run is the per-app worker loop: finish the job in flight (if any), then
// stop; queued-but-unstarted jobs are dropped with OnJobDropped.
func (q *Queue) run(w *appWorker) {
	defer q.wg.Done()
	for {
		// Check stop first so a shutdown never starts another queued job.
		select {
		case <-w.stop:
			q.drainDropped(w)
			return
		default:
		}
		select {
		case job := <-w.jobs:
			q.execute(job)
		case <-w.stop:
			q.drainDropped(w)
			return
		}
	}
}

func (q *Queue) drainDropped(w *appWorker) {
	for {
		select {
		case job := <-w.jobs:
			logs.Warning("webhook", "dropping queued job app=%s delivery=%s branch=%s commit=%s: server shutdown",
				job.AppName, job.DeliveryID, job.Branch, job.CommitSHA)
			if q.onJobDropped != nil {
				q.onJobDropped(job)
			}
		default:
			return
		}
	}
}

func (q *Queue) execute(job *Job) {
	logs.Info("webhook", "job started app=%s app_id=%s delivery=%s branch=%s commit=%s",
		job.AppName, job.AppID, job.DeliveryID, job.Branch, job.CommitSHA)
	err := q.rebuild(q.ctx, job)
	if q.onJobDone != nil {
		q.onJobDone(job, err)
	}
	if err != nil {
		if IsShutdownErr(err) {
			logs.Warning("webhook", "job abandoned at shutdown app=%s delivery=%s: %v", job.AppName, job.DeliveryID, err)
			return
		}
		logs.Error("webhook", "job failed app=%s delivery=%s commit=%s: %v",
			job.AppName, job.DeliveryID, job.CommitSHA, err)
		return
	}
	logs.Info("webhook", "job completed app=%s delivery=%s commit=%s", job.AppName, job.DeliveryID, job.CommitSHA)
}

// Close stops accepting jobs, cancels waiting work and waits up to
// drainTimeout for the job in flight to finish. A rebuild subprocess that is
// already running is left to complete on its own (see ErrShutdown).
func (q *Queue) Close(drainTimeout time.Duration) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.closed = true
	workers := make([]*appWorker, 0, len(q.workers))
	for _, w := range q.workers {
		workers = append(workers, w)
	}
	q.mu.Unlock()

	// Cancel first so jobs still waiting for the deploy lock stop waiting,
	// then stop the workers (current job finishes, queued jobs are dropped).
	q.cancel()
	for _, w := range workers {
		close(w.stop)
	}

	done := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(drainTimeout):
		logs.Warning("webhook", "shutdown: %d app worker(s) still busy after %s; leaving them", len(workers), drainTimeout)
	}
}
