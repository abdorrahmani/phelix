package secrets

import (
	"context"
	"github.com/shirou/gopsutil/process"
	"strings"

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
	release  func()
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
	set.release = phelixerr.AcquireSecretValues(values)
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

// Close releases this operation's references after all launchers have their own.
func (p *PinnedSet) Close() {
	if p != nil && p.release != nil {
		p.release()
	}
}

// FromRuntime captures only named secret variables from an instance's environment.
func FromRuntime(env []string, names []string) (*PinnedSet, error) {
	set := &PinnedSet{values: map[string]string{}}
	vars := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			vars[k] = v
		}
	}
	var values []string
	for _, name := range names {
		value, ok := vars[name]
		if !ok || ValidateSecretValue(value) != nil {
			return nil, phelixerr.New(phelixerr.CodeEncryption, "cannot read running instance credentials")
		}
		set.Bindings = append(set.Bindings, Binding{Secret: name, Env: name})
		set.values[name] = value
		values = append(values, value)
	}
	set.release = phelixerr.AcquireSecretValues(values)
	return set, nil
}

// RuntimeForPID reads credentials from a still-running process before replacing it.
func RuntimeForPID(pid int, names []string) (*PinnedSet, error) {
	if len(names) == 0 {
		return FromRuntime(nil, nil)
	}
	proc, err := process.NewProcess(int32(pid))
	if err != nil {
		return nil, phelixerr.New(phelixerr.CodeEncryption, "cannot inspect running credentials")
	}
	env, err := proc.Environ()
	if err != nil {
		return nil, phelixerr.New(phelixerr.CodeEncryption, "cannot read running credentials")
	}
	return FromRuntime(env, names)
}

// Values returns this set's values for an output owner to register.
func (p *PinnedSet) Values() []string {
	values := make([]string, 0, len(p.values))
	for _, value := range p.values {
		values = append(values, value)
	}
	return values
}
