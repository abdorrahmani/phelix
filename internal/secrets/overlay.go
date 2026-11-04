package secrets

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/abdorrahmani/phelix/internal/appid"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

var appCoverage = struct {
	mu   sync.Mutex
	apps map[string][]string
}{apps: map[string][]string{}}

// AcquireCoverage replaces app coverage only after registering every incoming value.
func AcquireCoverage(appID string, values []string) {
	replaceCoverage(appID, values)
}

// replaceCoverage updates one owner without releasing another owner's values.
func replaceCoverage(owner string, values []string) {
	appCoverage.mu.Lock()
	defer appCoverage.mu.Unlock()
	unique := make(map[string]bool, len(values))
	for _, value := range values {
		if value != "" {
			unique[value] = true
		}
	}
	next := make([]string, 0, len(unique))
	for value := range unique {
		phelixerr.RegisterSecretValue(value)
		next = append(next, value)
	}
	for _, value := range appCoverage.apps[owner] {
		phelixerr.UnregisterSecretValue(value)
	}
	if len(next) == 0 {
		delete(appCoverage.apps, owner)
	} else {
		appCoverage.apps[owner] = next
	}
}

// ReleaseCoverage releases app coverage after its instances and log drains are gone.
func ReleaseCoverage(appID string) {
	AcquireCoverage(appID, nil)
	replaceCoverage(appID+":launch", nil)
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
	if !appid.Valid(appID) {
		return nil, nil
	}
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
		MarkDegraded(appID, "cannot read secret bindings")
		return nil, err
	}
	return OverlayForLaunch(ctx, appID, bindings)
}

// OverlayForLaunch resolves every binding into KEY=value strings for a
// launcher. Resolution is all-or-nothing: one missing secret fails the whole
// overlay and nothing is registered or returned, so an application never
// starts with only part of the credentials it declared. Resolved values are
// masked for this process and retained for the application's coverage
// lifetime. Success marks the application healthy; failure marks it
// degraded, which halts its log transmission until it resolves again.
func OverlayForLaunch(ctx context.Context, appID string, bindings []Binding) ([]string, error) {
	if err := CheckRuntimeSecurity(appID); err != nil {
		return nil, err
	}
	if err := validateBindings(bindings); err != nil {
		return nil, err
	}
	if len(bindings) == 0 {
		markResolved(appID)
		return nil, nil
	}
	overlay := make([]string, 0, len(bindings))
	values := make([]string, 0, len(bindings))
	for _, b := range bindings {
		resolved, err := Resolve(ctx, appID, b.Secret)
		if err != nil {
			MarkDegraded(appID, "cannot resolve secret for "+b.Env+": "+err.Error())
			return nil, phelixerr.Wrapf(phelixerr.CodeOf(err), err, "secrets: cannot resolve %s (bound to %s)", b.Secret, b.Env)
		}
		overlay = append(overlay, b.Env+"="+resolved.Value)
		values = append(values, resolved.Value)
	}
	replaceCoverage(appID+":launch", values)
	markResolved(appID)
	return overlay, nil
}

// MergeEnv applies the overlay and preserves the last value of each variable.
func MergeEnv(base, overlay []string) []string {
	if len(overlay) == 0 {
		return base
	}
	all := append(append([]string(nil), base...), overlay...)
	last := make(map[string]int, len(all))
	for i, kv := range all {
		if key, _, ok := strings.Cut(kv, "="); ok {
			last[key] = i
		}
	}
	out := make([]string, 0, len(all))
	for i, kv := range all {
		if key, _, ok := strings.Cut(kv, "="); ok && last[key] != i {
			continue
		}
		out = append(out, kv)
	}
	return out
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
