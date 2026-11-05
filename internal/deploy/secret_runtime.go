package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/secrets"
)

// RuntimeSecrets captures credentials before an instance is replaced.
func RuntimeSecrets(inst *Instance, fallback []secrets.Binding) (*secrets.PinnedSet, error) {
	names := inst.SecretEnvs
	if names == nil {
		for _, b := range fallback {
			names = append(names, b.Env)
		}
	}
	if len(names) == 0 {
		return secrets.FromRuntime(nil, nil)
	}
	if inst.ContainerID == "" {
		return secrets.RuntimeForPID(inst.PID, names)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := execDockerRunner(ctx, "inspect", "--format", "{{json .}}", inst.ContainerID)
	if err != nil {
		if isDockerNotRunning(err) {
			return secrets.FromRuntime(nil, nil)
		}
		return nil, phelixerr.New(phelixerr.CodeEncryption, "cannot read container credentials")
	}
	var container struct {
		State  struct{ Running bool }
		Config struct{ Env []string }
	}
	if err := json.Unmarshal([]byte(out), &container); err != nil {
		return nil, phelixerr.New(phelixerr.CodeEncryption, "cannot decode container environment")
	}
	if !container.State.Running {
		return secrets.FromRuntime(nil, nil)
	}
	return secrets.FromRuntime(container.Config.Env, names)
}

// RefreshSecretCoverage refreshes one app without letting another app's failure stop it.
func RefreshSecretCoverage(info app.AppListItem) (errRet error) {
	defer func() {
		if errRet != nil {
			secrets.MarkDegraded(info.ID, "cannot register stored or running credentials")
		}
	}()
	bindings, err := secrets.CommittedBindings(info.ID)
	if err != nil {
		return err
	}
	state, err := Load(info.Name)
	if len(bindings) == 0 && len(info.SecretEnvs) == 0 && !hasInstanceSecrets(state) {
		secrets.ReleaseCoverage(info.ID)
		secrets.MarkHealthy(info.ID)
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	release, err := AcquireDeployLock(info.Name, "secrets-coverage")
	if err != nil {
		return err
	}
	defer release()
	bindings, err = secrets.CommittedBindings(info.ID)
	if err != nil {
		return err
	}
	state, err = Load(info.Name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var values []string
	for _, binding := range bindings {
		resolved, err := secrets.Resolve(context.Background(), info.ID, binding.Secret)
		if err != nil {
			return err
		}
		values = append(values, resolved.Value)
	}
	var instances []*Instance
	if state != nil && state.Mode != "classic" {
		instances = append(instances, state.Retiring...)
		for _, inst := range state.Slots {
			if inst != nil && (inst.ContainerID != "" || InstanceAlive(inst)) {
				instances = append(instances, inst)
			}
		}
		for _, inst := range state.Replicas {
			if inst != nil && (inst.ContainerID != "" || InstanceAlive(inst)) {
				instances = append(instances, inst)
			}
		}
	} else if info.PID > 0 && info.Status == "running" {
		instances = append(instances, &Instance{PID: info.PID, SecretEnvs: info.SecretEnvs})
	}
	live := false
	for _, inst := range instances {
		if inst == nil || inst.ContainerID == "" && !InstanceAlive(inst) {
			continue
		}
		set, err := RuntimeSecrets(inst, bindings)
		if err != nil {
			return err
		}
		defer set.Close()
		live = live || len(set.Bindings) > 0
		values = append(values, set.Values()...)
	}
	if !live {
		values = nil
	}
	secrets.AcquireCoverage(info.ID, values)
	secrets.MarkHealthy(info.ID)
	return nil
}

// hasInstanceSecrets detects credentials still in use after bindings change.
func hasInstanceSecrets(state *DeployState) bool {
	if state != nil {
		for _, inst := range state.Retiring {
			if inst != nil && len(inst.SecretEnvs) > 0 {
				return true
			}
		}
		for _, group := range []map[string]*Instance{state.Slots, state.Replicas} {
			for _, inst := range group {
				if inst != nil && len(inst.SecretEnvs) > 0 {
					return true
				}
			}
		}
	}
	return false
}
