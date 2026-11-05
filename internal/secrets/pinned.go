package secrets

import (
	"context"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// PinnedSet is a deployment's resolved binding set. The bindings and the
// resolved versions are durable (they go into the pending transaction
// record); the plaintext values live only in the memory of the process that
// executes the deployment. Every instance the deployment launches receives
// exactly this set, which is what makes the deployment uniform even though
// values are resolved at launch time everywhere else.
type PinnedSet struct {
	Bindings []Binding
	Resolved []ResolvedVersion
	values   map[string]string
}

// PinBindings resolves every binding once and freezes the result. Resolution
// is all-or-nothing: one missing secret fails the whole set and nothing is
// registered. The resolved values are masked for this process and retained
// with the application's coverage for the lifetime of the deployment.
func PinBindings(ctx context.Context, appID string, bindings []Binding) (*PinnedSet, error) {
	if err := CheckCollisions(appID, bindings); err != nil {
		return nil, err
	}
	if len(bindings) == 0 {
		return &PinnedSet{values: map[string]string{}}, nil
	}
	set := &PinnedSet{
		Bindings: append([]Binding(nil), bindings...),
		values:   make(map[string]string, len(bindings)),
	}
	st, err := Open(appID)
	if err != nil {
		return nil, err
	}
	values := make([]string, 0, len(bindings))
	for _, b := range bindings {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, exists := set.values[b.Secret]; exists {
			continue
		}
		resolved, err := st.resolve(b.Secret)
		if err != nil {
			return nil, phelixerr.Wrapf(phelixerr.CodeOf(err), err, "secrets: cannot pin %s (bound to %s)", b.Secret, b.Env)
		}
		set.Resolved = append(set.Resolved, ResolvedVersion{
			Secret:      b.Secret,
			Version:     resolved.Version,
			Fingerprint: resolved.Fingerprint,
		})
		set.values[b.Secret] = resolved.Value
		values = append(values, resolved.Value)
	}
	AcquireCoverage(appID, values)
	return set, nil
}

// Overlay renders the pinned set as KEY=value strings for a launcher. It
// never touches the store again: the values are the ones pinned at the start
// of the deployment.
func (p *PinnedSet) Overlay() []string {
	if p == nil || len(p.Bindings) == 0 {
		return nil
	}
	out := make([]string, 0, len(p.Bindings))
	for _, b := range p.Bindings {
		out = append(out, b.Env+"="+p.values[b.Secret])
	}
	return out
}
