package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
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
	if inst.ContainerID == "" {
		return secrets.RuntimeForPID(inst.PID, names)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := execDockerRunner(ctx, "inspect", "--format", "{{json .Config.Env}}", inst.ContainerID)
	if err != nil {
		return nil, phelixerr.New(phelixerr.CodeEncryption, "cannot read container credentials")
	}
	var env []string
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		return nil, phelixerr.New(phelixerr.CodeEncryption, "cannot decode container environment")
	}
	return secrets.FromRuntime(env, names)
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
	var values []string
	for _, binding := range bindings {
		resolved, err := secrets.Resolve(context.Background(), info.ID, binding.Secret)
		if err != nil {
			return err
		}
		values = append(values, resolved.Value)
	}
	state, err := Load(info.Name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var instances []*Instance
	if state != nil && state.Mode != "classic" {
		for _, inst := range state.Slots {
			if inst != nil && InstanceAlive(inst) {
				instances = append(instances, inst)
			}
		}
		for _, inst := range state.Replicas {
			if inst != nil && InstanceAlive(inst) {
				instances = append(instances, inst)
			}
		}
	} else if info.PID > 0 && info.Status == "running" {
		instances = append(instances, &Instance{PID: info.PID, SecretEnvs: info.SecretEnvs})
	}
	for _, inst := range instances {
		set, err := RuntimeSecrets(inst, bindings)
		if err != nil {
			return err
		}
		defer set.Close()
		values = append(values, set.Values()...)
	}
	secrets.AcquireCoverage(info.ID, values)
	secrets.MarkHealthy(info.ID)
	return nil
}

// overlayWithRuntime restores an instance's old credentials over its normal environment.
func overlayWithRuntime(envPath, appID string, set *secrets.PinnedSet, desired []secrets.Binding) ([]string, error) {
	base, err := snapshotOverlay(envPath, appID)
	if err != nil {
		return nil, err
	}
	managed := make(map[string]bool)
	for _, b := range desired {
		managed[b.Env] = true
	}
	clean := base[:0]
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if !managed[name] {
			clean = append(clean, kv)
		}
	}
	return mergeByKey(clean, set.Overlay()), nil
}
