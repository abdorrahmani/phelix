package secrets

import (
	"context"
	"sort"
	"sync"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// overlayGenerations is how many recent launch generations an application
// keeps masked at once. The current generation is the one the running
// instances received; the retained previous ones cover instances that are
// being replaced and log output that is still draining. Keeping a bounded
// history is deliberately conservative: a value is dropped only after two
// further full launches, by which time the instances that held it have
// certainly ended and their logs drained. When in doubt the registry
// over-retains, because under-retention would silently break the masking
// guarantee while over-retention only costs a little memory.
const overlayGenerations = 3

// coverage tracks, per application, the sets of secret values that recent
// launches resolved. Everything in it is registered with the redaction
// layer; releasing an application or aging out a generation unregisters
// exactly the values that set holds.
type coverage struct {
	mu   sync.Mutex
	apps map[string][][]string
}

var appCoverage = &coverage{apps: map[string][][]string{}}

// AcquireCoverage registers a launch's resolved values for an application.
// Repeated launches with the same values are safe: registration is
// reference-counted underneath.
func AcquireCoverage(appID string, values []string) {
	if len(values) == 0 {
		return
	}
	appCoverage.mu.Lock()
	defer appCoverage.mu.Unlock()
	sets := appCoverage.apps[appID]
	for _, v := range values {
		phelixerr.RegisterSecretValue(v)
	}
	sets = append(sets, values)
	for len(sets) > overlayGenerations {
		oldest := sets[0]
		sets = sets[1:]
		for _, v := range oldest {
			phelixerr.UnregisterSecretValue(v)
		}
	}
	appCoverage.apps[appID] = sets
}

// ReleaseCoverage unregisters every value the application has resolved and
// forgets its history. The monitor calls it once an application has fully
// stopped and its log streams have drained; nothing that could still emit
// the values is running at that point.
func ReleaseCoverage(appID string) {
	appCoverage.mu.Lock()
	defer appCoverage.mu.Unlock()
	sets := appCoverage.apps[appID]
	delete(appCoverage.apps, appID)
	for _, set := range sets {
		for _, v := range set {
			phelixerr.UnregisterSecretValue(v)
		}
	}
}

// RegisterResolvedValue masks a single value in the current process. The CLI
// uses it while it handles a plaintext value; a short-lived command needs no
// explicit release, because the process exit drops the registry.
func RegisterResolvedValue(value string) {
	phelixerr.RegisterSecretValue(value)
}

// UnregisterResolvedValue drops one reference the caller took with
// RegisterResolvedValue.
func UnregisterResolvedValue(value string) {
	phelixerr.UnregisterSecretValue(value)
}

// CommittedBindings returns the active bindings of an application. An app
// with no store yet simply has none.
func CommittedBindings(appID string) ([]Binding, error) {
	st, err := Open(appID)
	if err != nil {
		return nil, err
	}
	return st.Bindings, nil
}

// ClassicEnvOverlay resolves the committed bindings of an application that
// runs as a plain host process rather than through the deploy engine. It is
// the classic-path counterpart of the deploy overlay: empty for apps without
// bindings, and a hard failure when a declared secret is missing.
func ClassicEnvOverlay(ctx context.Context, appID string) ([]string, error) {
	bindings, err := CommittedBindings(appID)
	if err != nil {
		return nil, err
	}
	return OverlayForLaunch(ctx, appID, bindings)
}

// OverlayForLaunch resolves every binding into KEY=value strings for a
// launcher. Resolution is all-or-nothing: one missing secret fails the whole
// overlay and nothing is registered or returned, so an application never
// starts with only part of the credentials it declared. Resolved values are
// masked for this process and retained for the application's coverage
// lifetime.
func OverlayForLaunch(ctx context.Context, appID string, bindings []Binding) ([]string, error) {
	if len(bindings) == 0 {
		return nil, nil
	}
	overlay := make([]string, 0, len(bindings))
	values := make([]string, 0, len(bindings))
	for _, b := range bindings {
		resolved, err := Resolve(ctx, appID, b.Secret)
		if err != nil {
			return nil, phelixerr.Wrapf(phelixerr.CodeOf(err), err, "secrets: cannot resolve %s (bound to %s)", b.Secret, b.Env)
		}
		overlay = append(overlay, b.Env+"="+resolved.Value)
		values = append(values, resolved.Value)
	}
	AcquireCoverage(appID, values)
	return overlay, nil
}

// mergeByKey merges an overlay over a base environment by variable name: an
// overlay entry replaces the base entry of the same name, every other base
// entry is kept, and each name appears exactly once in the result. This is
// what makes an old snapshot with a stale copy of a managed name safe: the
// managed value always wins and is never duplicated.
func mergeByKey(base, overlay []string) []string {
	if len(overlay) == 0 {
		return base
	}
	keys := make(map[string]string, len(overlay))
	for _, kv := range overlay {
		if k, _, ok := splitEnv(kv); ok {
			keys[k] = kv
		}
	}
	seen := make(map[string]bool, len(base)+len(overlay))
	out := make([]string, 0, len(base)+len(overlay))
	for _, kv := range base {
		k, _, ok := splitEnv(kv)
		if ok && keys[k] != "" {
			continue
		}
		if ok {
			if seen[k] {
				continue
			}
			seen[k] = true
		}
		out = append(out, kv)
	}
	for _, kv := range overlay {
		k, _, ok := splitEnv(kv)
		if ok && seen[k] {
			continue
		}
		if ok {
			seen[k] = true
		}
		out = append(out, kv)
	}
	return out
}

// splitEnv splits a KEY=value string. It reports false for strings without
// the separator so malformed entries are passed through untouched rather
// than silently dropped.
func splitEnv(kv string) (key, value string, ok bool) {
	for i := 0; i < len(kv); i++ {
		if kv[i] == '=' {
			return kv[:i], kv[i+1:], true
		}
	}
	return "", "", false
}

// sortBindings gives bindings a stable order for display and for the durable
// record, so two views of the same set always compare equal.
func sortBindings(bindings []Binding) {
	sort.Slice(bindings, func(i, j int) bool {
		if bindings[i].Env != bindings[j].Env {
			return bindings[i].Env < bindings[j].Env
		}
		return bindings[i].Secret < bindings[j].Secret
	})
}
