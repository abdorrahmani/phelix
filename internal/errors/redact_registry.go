package phelixerr

import (
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// valueRedactor rewrites one rendered string, masking everything it was
// built to mask. The registry installs either a compiled matcher or a
// plain replacement loop; both are fail-closed by construction.
type valueRedactor func(string) string

// secretRegistry holds the secret values that have been resolved inside this
// process. Registration is reference-counted: the same value may be received
// by several launches at once, and it stays masked until every owner has
// released it. There is no size cap, because removing a live value from the
// registry would silently break the masking guarantee; the owners release
// their references when their instance ends and its logs have drained, so the
// registry stays as small as the running workload.
type secretRegistry struct {
	mu      sync.RWMutex
	refs    map[string]int
	matcher atomic.Value
}

// registry is the process-wide instance.
var registry secretRegistry

// RegisterSecretValue masks a secret value in every output path that runs
// through Redact. Owners call it when they receive a value and must call
// UnregisterSecretValue when they no longer produce output that could carry
// it. Empty values are ignored: there is nothing to mask and an empty
// pattern would over-mask everything.
func RegisterSecretValue(value string) {
	if value == "" {
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.refs == nil {
		registry.refs = make(map[string]int)
	}
	registry.refs[value]++
	if registry.refs[value] == 1 {
		registry.rebuildLocked()
	}
}

// UnregisterSecretValue drops one reference to a value. The value stays
// masked until its last reference is released. Releasing an unknown value is
// harmless.
func UnregisterSecretValue(value string) {
	if value == "" {
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	n, ok := registry.refs[value]
	if !ok {
		return
	}
	if n <= 1 {
		delete(registry.refs, value)
		registry.rebuildLocked()
		return
	}
	registry.refs[value] = n - 1
}

// registeredValueCount reports how many distinct values are currently
// masked. It exists for tests and for the doctor's own diagnostics.
func registeredValueCount() int {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	return len(registry.refs)
}

// rebuildLocked recompiles the matcher after the reference set changed.
// Longer values are placed first so that a value which is a prefix of
// another is never partially left behind. If the combined pattern ever
// fails to compile, the registry falls back to a plain replacement loop
// over every registered value: masking may get slower, but it never stops.
func (r *secretRegistry) rebuildLocked() {
	if len(r.refs) == 0 {
		r.matcher.Store(valueRedactor(nil))
		return
	}
	values := make([]string, 0, len(r.refs))
	for v := range r.refs {
		values = append(values, v)
	}
	sort.Slice(values, func(i, j int) bool {
		if len(values[i]) != len(values[j]) {
			return len(values[i]) > len(values[j])
		}
		return values[i] < values[j]
	})
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = regexp.QuoteMeta(v)
	}
	re, err := regexp.Compile("(" + strings.Join(parts, "|") + ")")
	if err != nil {
		r.matcher.Store(valueRedactor(r.replaceLoopLocked()))
		return
	}
	r.matcher.Store(valueRedactor(func(s string) string {
		return re.ReplaceAllString(s, "***")
	}))
}

// replaceLoopLocked builds the fallback redactor. It reads the reference set
// under the registry lock and copies the values, so the returned closure
// never races with later changes.
func (r *secretRegistry) replaceLoopLocked() func(string) string {
	values := make([]string, 0, len(r.refs))
	for v := range r.refs {
		values = append(values, v)
	}
	return func(s string) string {
		for _, v := range values {
			s = strings.ReplaceAll(s, v, "***")
		}
		return s
	}
}

// applyRegisteredValues masks registered secret values in s. It is the first
// pass of Redact: anything the secret layer resolved is masked before the
// general credential patterns run.
func applyRegisteredValues(s string) string {
	m, _ := registry.matcher.Load().(valueRedactor)
	if m == nil {
		return s
	}
	return m(s)
}
