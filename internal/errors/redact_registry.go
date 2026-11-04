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
	registry.changeReferences([]string{value}, 1)
}

// UnregisterSecretValue drops one reference to a value. The value stays
// masked until its last reference is released. Releasing an unknown value is
// harmless.
func UnregisterSecretValue(value string) {
	registry.changeReferences([]string{value}, -1)
}

// changeReferences rebuilds once when a batch adds or releases distinct values.
func (r *secretRegistry) changeReferences(values []string, delta int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refs == nil {
		r.refs = make(map[string]int)
	}
	changed := false
	for _, value := range values {
		if value == "" {
			continue
		}
		before := r.refs[value]
		after := max(0, before+delta)
		if after == 0 {
			delete(r.refs, value)
		} else {
			r.refs[value] = after
		}
		changed = changed || (before == 0) != (after == 0)
	}
	if changed {
		r.rebuildLocked()
	}
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
	unique := make(map[string]bool)
	for v := range r.refs {
		unique[v] = true
		for _, line := range strings.FieldsFunc(v, func(r rune) bool { return r == '\n' || r == '\r' }) {
			unique[line] = true
		}
	}
	values := make([]string, 0, len(unique))
	for v := range unique {
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
		pairs := make([]string, 0, 2*len(values))
		for _, value := range values {
			pairs = append(pairs, value, "***")
		}
		r.matcher.Store(valueRedactor(strings.NewReplacer(pairs...).Replace))
		return
	}
	r.matcher.Store(valueRedactor(func(s string) string {
		return re.ReplaceAllString(s, "***")
	}))
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

// SecretValuesInEnv returns registered values received by this instance.
func SecretValuesInEnv(env []string) []string {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	var values []string
	seen := map[string]bool{}
	for _, kv := range env {
		_, value, ok := strings.Cut(kv, "=")
		if ok && registry.refs[value] > 0 && !seen[value] {
			values = append(values, value)
			seen[value] = true
		}
	}
	return values
}

// AcquireSecretValues holds values until the owner calls the returned release function.
func AcquireSecretValues(values []string) func() {
	values = append([]string(nil), values...)
	registry.changeReferences(values, 1)
	return sync.OnceFunc(func() {
		registry.changeReferences(values, -1)
	})
}

// SecretEnvNames records which variables need masking when an instance is adopted.
func SecretEnvNames(env []string) []string {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	names := make([]string, 0)
	for _, kv := range env {
		key, value, ok := strings.Cut(kv, "=")
		if ok && registry.refs[value] > 0 {
			names = append(names, key)
		}
	}
	return names
}
