package grpc

import (
	"sync"
	"time"

	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/server"
)

const (
	// maxBatchLines and maxBatchBytes bound one MonitorLogBatch: whichever
	// limit is hit first flushes the queue.
	maxBatchLines = 200
	maxBatchBytes = 500 * 1024
	// maxBatchLinger is how long a partially filled queue waits before
	// flushing anyway, so low-traffic apps never see stale lines.
	maxBatchLinger = 200 * time.Millisecond
	// maxQueueLines bounds the pending queue. A slower stream than the
	// collectors must not block them or grow without limit: overflow lines
	// are dropped and counted instead.
	maxQueueLines = 5000
)

// logBatcher accumulates log entries and drains them as MonitorLogBatch
// events. The batcher runs on its own goroutine so a slow monitor stream
// never blocks the collection ticks: when the queue is full, new lines are
// dropped and counted, and the count ships with the next batch.
type logBatcher struct {
	mu      sync.Mutex
	pending []*pb.MonitorLogEntry
	bytes   int
	dropped int64
	timer   *time.Timer
	flush   chan struct{}
	done    chan struct{}
}

func init() { RegisterCapability(CapabilityLogBatch) }

func newLogBatcher() *logBatcher {
	return &logBatcher{
		flush: make(chan struct{}, 1),
		done:  make(chan struct{}),
	}
}

// add queues entries, dropping the overflow past maxQueueLines. The return
// value reports how many entries were dropped by this call.
func (b *logBatcher) add(entries []*pb.MonitorLogEntry) int {
	b.mu.Lock()
	dropped := 0
	for _, e := range entries {
		if len(b.pending) >= maxQueueLines {
			dropped++
			b.dropped++
			continue
		}
		b.pending = append(b.pending, e)
		b.bytes += entrySize(e)
	}
	needFlush := len(b.pending) >= maxBatchLines || b.bytes >= maxBatchBytes
	b.mu.Unlock()

	if needFlush {
		select {
		case b.flush <- struct{}{}:
		default:
		}
	}
	return dropped
}

// entrySize approximates one entry's wire footprint.
func entrySize(e *pb.MonitorLogEntry) int {
	n := len(e.GetLog()) + len(e.GetComponent()) + len(e.GetApp()) + len(e.GetRequestId())
	for k, v := range e.GetFields() {
		n += len(k) + len(v)
	}
	return n + 64
}

// run drains the queue until stop. It wakes on a size-triggered flush signal
// or at least every maxBatchLinger, so the batch latency never exceeds the
// linger window even when nothing fills the queue.
func (b *logBatcher) run(sender func(*pb.MonitorEvent) error) {
	ticker := time.NewTicker(maxBatchLinger)
	defer ticker.Stop()
	for {
		select {
		case <-b.done:
			return
		case <-b.flush:
		case <-ticker.C:
		}
		b.drain(sender)
	}
}

// drain sends every pending entry as one or more batches.
func (b *logBatcher) drain(sender func(*pb.MonitorEvent) error) {
	for {
		b.mu.Lock()
		n := len(b.pending)
		if n > maxBatchLines {
			n = maxBatchLines
		}
		entries := b.pending[:n]
		rest := b.pending[n:]
		b.bytes = 0
		for _, e := range rest {
			b.bytes += entrySize(e)
		}
		dropped := b.dropped
		b.dropped = 0
		b.pending = append([]*pb.MonitorLogEntry(nil), rest...)
		b.mu.Unlock()

		if len(entries) == 0 && dropped == 0 {
			return
		}

		event := &pb.MonitorEvent{
			ServerId:  server.GetServerID(),
			Timestamp: time.Now().UnixMilli(),
			Payload: &pb.MonitorEvent_LogBatch{
				LogBatch: &pb.MonitorLogBatch{
					Entries: entries,
					Dropped: dropped,
				},
			},
		}
		if err := sender(event); err != nil {
			logs.ErrorFile("grpc", "[gRPC Monitor] failed to send log batch (%d entries): %v", len(entries), err)
			b.requeue(entries, dropped)
			return
		}
		if len(rest) == 0 && dropped == 0 {
			return
		}
	}
}

// requeue puts a failed batch back at the front of the queue. The queue stays
// bounded: if concurrent adds filled it, the oldest entries are dropped and
// counted rather than growing without limit.
func (b *logBatcher) requeue(entries []*pb.MonitorLogEntry, dropped int64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if overflow := len(b.pending) + len(entries) - maxQueueLines; overflow > 0 {
		entries = entries[:len(entries)-overflow]
		dropped += int64(overflow)
	}
	b.pending = append(append([]*pb.MonitorLogEntry(nil), entries...), b.pending...)
	b.bytes = 0
	for _, e := range b.pending {
		b.bytes += entrySize(e)
	}
	b.dropped += dropped
}

// stop ends the run loop.
func (b *logBatcher) stop() {
	close(b.done)
}
