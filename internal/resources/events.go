package resources

import (
	"fmt"
	"strconv"
	"strings"
)

// MemoryEvents mirrors the cgroup-v2 memory.events counters of one instance
// cgroup. OOM counts OOM events considered; OOMKill counts processes the
// kernel actually killed inside the cgroup (possibly a descendant, not the
// main PID). OOMKill is Phelix's resource-OOM signal.
type MemoryEvents struct {
	OOM     uint64
	OOMKill uint64
}

// CPUStat mirrors the cgroup-v2 cpu.stat accounting of one instance cgroup.
// The throttling counters are informational only: CPU limits stay
// enforcement-only and never fail an instance by themselves.
type CPUStat struct {
	UsageUsec     uint64
	UserUsec      uint64
	SystemUsec    uint64
	NrPeriods     uint64
	NrThrottled   uint64
	ThrottledUsec uint64
}

// Usage is a live snapshot of one instance cgroup's resource accounting:
// memory usage, the enforced memory ceiling, cumulative memory events and CPU
// accounting. It is internal runtime information for lifecycle/error
// classification; dashboard or backend reporting is not part of this phase.
// MemoryMax is 0 when no limit applies (the kernel reports "max").
type Usage struct {
	MemoryCurrent uint64
	MemoryMax     uint64
	MemoryEvents  MemoryEvents
	CPUStat       CPUStat
}

// parseCounters parses kernel "<key> <value>" control content. required
// lists the keys that must be present; unknown keys are ignored (kernels add
// counters over time). Malformed lines and non-numeric values error, matching
// unpopulated's fail-visible convention. file names the control file for
// error messages.
func parseCounters(data []byte, file string, required []string, out map[string]*uint64) error {
	present := make(map[string]bool, len(required))
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return fmt.Errorf("%s: malformed line %q", file, line)
		}
		p, ok := out[fields[0]]
		if !ok {
			continue
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return fmt.Errorf("%s: invalid %s value %q", file, fields[0], fields[1])
		}
		*p = v
		present[fields[0]] = true
	}
	for _, k := range required {
		if !present[k] {
			return fmt.Errorf("%s: missing %s counter", file, k)
		}
	}
	return nil
}

// parseMemoryEvents parses memory.events content. Kernel-written files always
// carry both counters; a file missing either is malformed.
func parseMemoryEvents(data []byte) (MemoryEvents, error) {
	var ev MemoryEvents
	err := parseCounters(data, "memory.events", []string{"oom", "oom_kill"},
		map[string]*uint64{"oom": &ev.OOM, "oom_kill": &ev.OOMKill})
	return ev, err
}

// parseCPUStat parses cpu.stat content. Kernel-written files always carry all
// six counters.
func parseCPUStat(data []byte) (CPUStat, error) {
	var s CPUStat
	err := parseCounters(data, "cpu.stat",
		[]string{"usage_usec", "user_usec", "system_usec", "nr_periods", "nr_throttled", "throttled_usec"},
		map[string]*uint64{
			"usage_usec":     &s.UsageUsec,
			"user_usec":      &s.UserUsec,
			"system_usec":    &s.SystemUsec,
			"nr_periods":     &s.NrPeriods,
			"nr_throttled":   &s.NrThrottled,
			"throttled_usec": &s.ThrottledUsec,
		})
	return s, err
}

// parseScalarControl parses single-value control content such as
// memory.current and memory.max. The kernel's "max" shorthand for unlimited
// becomes 0.
func parseScalarControl(data []byte) (uint64, error) {
	s := strings.TrimSpace(string(data))
	if s == "max" {
		return 0, nil
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid control value %q: expected unsigned integer or max", s)
	}
	return v, nil
}
