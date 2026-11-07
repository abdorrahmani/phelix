package security

import "sync/atomic"

// Progress receives stage updates during a scan. Stage identifies the
// phase (inventory, resolve, query, fetch, match, persist); detail is a
// short human-readable summary of where the scan is. Implementations must
// be safe for concurrent use and must never receive dependency names,
// versions, or advisory content: only counts and stage names.
type Progress func(stage string, detail string)

func noopProgress(stage, detail string) {}

// progressSafe wraps a possibly-nil Progress.
func progressSafe(p Progress) Progress {
	if p == nil {
		return noopProgress
	}
	return p
}

// fetchProgressCounter reports bounded fetch progress as advisories land.
type fetchProgressCounter struct {
	total atomic.Int64
	done  atomic.Int64
}

func (c *fetchProgressCounter) add(total int) {
	c.total.Add(int64(total))
}

func (c *fetchProgressCounter) one() {
	c.done.Add(1)
}
