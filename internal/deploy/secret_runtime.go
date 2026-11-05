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
	if inst == nil {
		return secrets.FromRuntime(nil, nil)
	}
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

// RuntimeEnvOverlay gives probes the credentials of the instance they are checking.
func RuntimeEnvOverlay(inst *Instance, appID string) ([]string, func(), error) {
	base, err := snapshotOverlay(inst.EnvPath, appID)
	if err != nil {
		return nil, func() {}, err
	}
	bindings, err := secrets.CommittedBindings(appID)
	if err != nil {
		return nil, func() {}, err
	}
	set, err := RuntimeSecrets(inst, bindings)
	if err != nil {
		return nil, func() {}, err
	}
	return mergeByKey(base, set.Overlay()), set.Close, nil
}

// RefreshSecretCoverage refreshes one app without letting another app's failure stop it.
func RefreshSecretCoverage(info app.AppListItem) (errRet error) {
	return inspectSecretCoverage(info, true)
}

// InspectSecretCoverage checks stored and running values without changing deployment state.
func InspectSecretCoverage(info app.AppListItem) error {
	return inspectSecretCoverage(info, false)
}

// inspectSecretCoverage keeps runtime inspection separate from deployment recovery.
func inspectSecretCoverage(info app.AppListItem, reconcile bool) (errRet error) {
	if !app.ValidAppID(info.ID) {
		return nil
	}
	defer func() {
		if errRet != nil {
			secrets.MarkRuntimeDegraded(info.ID, "cannot register stored or running credentials")
		}
	}()
	store, err := secrets.Open(info.ID)
	if err != nil {
		return err
	}
	bindings := store.Bindings
	state, err := Load(info.Name)
	if store.Pending == nil && len(bindings) == 0 && len(info.SecretEnvs) == 0 && !hasInstanceSecrets(state) {
		secrets.ReleaseCoverage(info.ID)
		secrets.MarkHealthy(info.ID)
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if store.Pending != nil && state != nil && (state.AppName != info.Name || state.AppID != "" && state.AppID != info.ID || state.Mode != ModeRolling && state.Mode != ModeBlueGreen && state.Mode != ModeWorker && state.Mode != "classic") {
		return phelixerr.New(phelixerr.CodeConfiguration, "pending secrets have an invalid deployment journal")
	}
	if reconcile {
		release, err := AcquireDeployLock(info.Name, "secrets-coverage")
		if err != nil {
			if phelixerr.IsCode(err, phelixerr.CodeDeployLocked) {
				return nil
			}
			return err
		}
		defer release()
		if _, err := ReconcileSecretTxn(info.Name, info.ID); err != nil {
			return err
		}
	}
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
		instances = append(instances, &Instance{PID: info.PID, StartTicks: info.StartTicks, SecretEnvs: info.SecretEnvs})
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
		secrets.ReleaseCoverage(info.ID)
	} else {
		secrets.AcquireCoverage(info.ID, values)
	}
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
