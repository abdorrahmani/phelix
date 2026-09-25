//go:build linux

package resources

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func writeFile(t *testing.T, g *group, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(g.dir.Name(), name), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// testInstance builds an Instance backed by plain files instead of a real
// cgroup: readControl only needs a directory with the control files present.
// The lease pipe mirrors the launcher's hold on the cleanup helper's lease.
func testInstance(t *testing.T, base MemoryEvents, memCtl, cpuCtl bool) (*Instance, *os.File) {
	t.Helper()
	g := testGroup(t)
	leaseR, leaseW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	inst := &Instance{g: g, lease: leaseW, base: base, memCtl: memCtl, cpuCtl: cpuCtl, memoryLimit: "512Mi"}
	t.Cleanup(func() { _ = inst.Close(); _ = leaseR.Close() })
	return inst, leaseR
}

func TestResourceOOMDelta(t *testing.T) {
	for _, tc := range []struct {
		name string
		base uint64
		now  uint64
		oom  bool
	}{
		{"kill during lifetime", 2, 3, true},
		{"no new kill", 2, 2, false},
		{"counter decreased never OOM", 2, 1, false},
		{"first kill from zero", 0, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst, _ := testInstance(t, MemoryEvents{OOMKill: tc.base}, true, false)
			writeFile(t, inst.g, "memory.events", "oom 0\noom_kill "+strconv.FormatUint(tc.now, 10)+"\n")
			oom, err := inst.ResourceOOM()
			if err != nil {
				t.Fatal(err)
			}
			if oom != tc.oom {
				t.Fatalf("ResourceOOM = %v, want %v", oom, tc.oom)
			}
		})
	}
}

func TestResourceOOMWithoutMemoryController(t *testing.T) {
	inst, _ := testInstance(t, MemoryEvents{}, false, true)
	oom, err := inst.ResourceOOM()
	if oom || err != nil {
		t.Fatalf("untracked instance = %v, %v; want false, nil", oom, err)
	}
}

// The classification unit is the cgroup, not the main PID: a child killed by
// the memory limit while the main process survives still proves the instance
// OOM'd.
func TestResourceOOMDescendantKill(t *testing.T) {
	inst, _ := testInstance(t, MemoryEvents{}, true, false)
	writeFile(t, inst.g, "cgroup.events", "populated 1\n")
	writeFile(t, inst.g, "memory.events", "oom 1\noom_kill 1\n")
	oom, err := inst.ResourceOOM()
	if err != nil || !oom {
		t.Fatalf("descendant OOM not classified: %v, %v", oom, err)
	}
}

// Cleanup ordering: the watcher must cache the final memory.events BEFORE
// releasing the cleanup lease (the detached helper removes the cgroup only
// after lease EOF), and the cached evidence must answer ResourceOOM even once
// the cgroup is gone.
func TestWatchCapturesEventsBeforeLeaseRelease(t *testing.T) {
	inst, leaseR := testInstance(t, MemoryEvents{OOMKill: 2}, true, false)
	writeFile(t, inst.g, "memory.events", "oom 1\noom_kill 3\n")
	writeFile(t, inst.g, "cgroup.events", "populated 1\n")
	inst.watch()

	// The instance's last process exits.
	writeFile(t, inst.g, "cgroup.events", "populated 0\n")

	// Lease EOF = cleanup was allowed to proceed. This blocks until the
	// watcher acts, so the read below cannot observe a pre-capture state.
	if _, err := io.ReadAll(leaseR); err != nil {
		t.Fatal(err)
	}

	// After cleanup the evidence must still be available — from the cache.
	oom, err := inst.ResourceOOM()
	if err != nil || !oom {
		t.Fatalf("cached OOM evidence lost after lease release: %v, %v", oom, err)
	}
	// Even with the cgroup directory gone (helper removed it).
	if err := os.RemoveAll(inst.g.dir.Name()); err != nil {
		t.Fatal(err)
	}
	oom, err = inst.ResourceOOM()
	if err != nil || !oom {
		t.Fatalf("cached OOM evidence did not survive cgroup removal: %v, %v", oom, err)
	}
}

// A watch that observes a healthy cgroup keeps the lease open: cleanup may not
// proceed while the instance lives.
func TestWatchKeepsLeaseWhilePopulated(t *testing.T) {
	inst, leaseR := testInstance(t, MemoryEvents{}, true, false)
	writeFile(t, inst.g, "cgroup.events", "populated 1\n")
	inst.watch()
	time.Sleep(250 * time.Millisecond) // several poll intervals
	leaseR.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, err := leaseR.Read(make([]byte, 1)); err == nil {
		t.Fatal("cleanup lease released while cgroup still populated")
	}
}

// Closing the handle before any evidence was captured loses the OOM
// classification — and must never report a false OOM.
func TestResourceOOMAfterCloseIsNeverTrue(t *testing.T) {
	inst, _ := testInstance(t, MemoryEvents{}, true, false)
	writeFile(t, inst.g, "memory.events", "oom 1\noom_kill 5\n")
	if err := inst.Close(); err != nil {
		t.Fatal(err)
	}
	if err := inst.Close(); err != nil {
		t.Fatalf("Close not idempotent: %v", err)
	}
	oom, err := inst.ResourceOOM()
	if oom {
		t.Fatal("closed handle classified OOM")
	}
	if err == nil {
		t.Fatal("expected evidence-lost error after close")
	}
}

func TestSnapshotParsesAllFiles(t *testing.T) {
	inst, _ := testInstance(t, MemoryEvents{}, true, true)
	writeFile(t, inst.g, "memory.current", "1048576\n")
	writeFile(t, inst.g, "memory.max", "max\n")
	writeFile(t, inst.g, "memory.events", "oom 2\noom_kill 1\n")
	writeFile(t, inst.g, "cpu.stat", "usage_usec 1000\nuser_usec 600\nsystem_usec 400\nnr_periods 10\nnr_throttled 3\nthrottled_usec 25\n")
	u, err := inst.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if u.MemoryCurrent != 1048576 || u.MemoryMax != 0 {
		t.Fatalf("memory usage wrong: %+v", u)
	}
	if u.MemoryEvents != (MemoryEvents{OOM: 2, OOMKill: 1}) {
		t.Fatalf("memory events wrong: %+v", u.MemoryEvents)
	}
	if u.CPUStat != (CPUStat{1000, 600, 400, 10, 3, 25}) {
		t.Fatalf("cpu stat wrong: %+v", u.CPUStat)
	}
}

// Snapshot only reads control files for controllers enabled on the instance
// cgroup, mirroring Phase 1's exact-enable rule.
func TestSnapshotSkipsDisabledControllers(t *testing.T) {
	inst, _ := testInstance(t, MemoryEvents{}, false, true)
	writeFile(t, inst.g, "cpu.stat", "usage_usec 7\nuser_usec 3\nsystem_usec 4\nnr_periods 1\nnr_throttled 0\nthrottled_usec 0\n")
	u, err := inst.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if u.CPUStat.UsageUsec != 7 {
		t.Fatalf("cpu stat missing: %+v", u)
	}
}
