package grpc

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
)

func batchDrain(t *testing.T, b *logBatcher) []*pb.MonitorLogBatch {
	t.Helper()
	var mu sync.Mutex
	var out []*pb.MonitorLogBatch
	b.drain(func(event *pb.MonitorEvent) error {
		if batch := event.GetLogBatch(); batch != nil {
			mu.Lock()
			out = append(out, batch)
			mu.Unlock()
		}
		return nil
	})
	return out
}

func makeEntries(n, logLen int) []*pb.MonitorLogEntry {
	out := make([]*pb.MonitorLogEntry, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, &pb.MonitorLogEntry{
			ServerId: "srv-1",
			Log:      strings.Repeat("x", logLen),
		})
	}
	return out
}

func TestLogBatcher_SplitsAtLineLimit(t *testing.T) {
	b := newLogBatcher()
	defer b.stop()
	if dropped := b.add(makeEntries(250, 10)); dropped != 0 {
		t.Fatalf("dropped %d entries below the queue limit", dropped)
	}

	batches := batchDrain(t, b)
	if len(batches) != 2 {
		t.Fatalf("got %d batches, want 2", len(batches))
	}
	if len(batches[0].Entries) != maxBatchLines {
		t.Errorf("first batch holds %d entries, want %d", len(batches[0].Entries), maxBatchLines)
	}
	if len(batches[1].Entries) != 50 {
		t.Errorf("second batch holds %d entries, want 50", len(batches[1].Entries))
	}
	if batches[0].Dropped != 0 || batches[1].Dropped != 0 {
		t.Errorf("no lines should be dropped below the queue limit")
	}
}

func TestLogBatcher_DropsOverflowAndCountsIt(t *testing.T) {
	b := newLogBatcher()
	defer b.stop()
	if dropped := b.add(makeEntries(maxQueueLines+137, 4)); dropped != 137 {
		t.Fatalf("dropped = %d, want 137", dropped)
	}

	batches := batchDrain(t, b)
	totalDropped := int64(0)
	totalEntries := 0
	for _, batch := range batches {
		totalDropped += batch.Dropped
		totalEntries += len(batch.Entries)
	}
	if totalDropped != 137 {
		t.Errorf("dropped counter = %d, want 137", totalDropped)
	}
	if totalEntries != maxQueueLines {
		t.Errorf("queued entries = %d, want %d", totalEntries, maxQueueLines)
	}
}

func TestLogBatcher_BytesLimitFlushes(t *testing.T) {
	b := newLogBatcher()
	defer b.stop()
	if dropped := b.add(makeEntries(130, 4096)); dropped != 0 {
		t.Fatalf("dropped %d entries", dropped)
	}

	batches := batchDrain(t, b)
	if len(batches) == 0 {
		t.Fatalf("size-triggered flush produced no batch")
	}
	total := 0
	for _, batch := range batches {
		total += len(batch.Entries)
	}
	if total != 130 {
		t.Errorf("entries = %d, want 130", total)
	}
}

func TestLogBatcher_RequeuesFailedBatch(t *testing.T) {
	b := newLogBatcher()
	defer b.stop()
	b.add(makeEntries(3, 10))

	fail := true
	b.drain(func(event *pb.MonitorEvent) error {
		if fail {
			fail = false
			return errors.New("stream unavailable")
		}
		return nil
	})

	batches := batchDrain(t, b)
	if len(batches) != 1 || len(batches[0].Entries) != 3 {
		t.Fatalf("failed batch was not requeued: %+v", batches)
	}
	if batches[0].Dropped != 0 {
		t.Errorf("dropped = %d, want 0", batches[0].Dropped)
	}
}

func TestLogBatcher_LingerFlush(t *testing.T) {
	b := newLogBatcher()
	defer b.stop()

	var mu sync.Mutex
	got := 0
	done := make(chan struct{})
	go b.run(func(event *pb.MonitorEvent) error {
		if batch := event.GetLogBatch(); batch != nil {
			mu.Lock()
			got += len(batch.Entries)
			mu.Unlock()
			select {
			case done <- struct{}{}:
			default:
			}
		}
		return nil
	})

	if dropped := b.add(makeEntries(1, 8)); dropped != 0 {
		t.Fatalf("dropped %d entries", dropped)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("linger flush did not fire within 2s")
	}
	mu.Lock()
	defer mu.Unlock()
	if got != 1 {
		t.Errorf("linger batch carried %d entries, want 1", got)
	}
}
