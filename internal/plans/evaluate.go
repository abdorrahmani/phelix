package plans

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/builder"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/abdorrahmani/phelix/internal/toolchain"
)

// Precondition types. Each one is re-derived from current state at apply
// time and compared with the value captured at plan-creation time.
const (
	PreconditionAppExists      = "app_exists"
	PreconditionCurrentVersion = "current_version"
	PreconditionVersionExists  = "version_exists"
	PreconditionDeployMode     = "deploy_mode"
	PreconditionConfigMatch    = "config_fingerprint"
	PreconditionSourceCommit   = "source_commit"
	PreconditionToolchain      = "toolchain_available"
)

// FailedPrecondition reports one precondition that no longer holds, with the
// expected (planned) and actual (current) canonical values.
type FailedPrecondition struct {
	Type     string `json:"type"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
}

// versionLabel is the canonical string form of a version number ("v17"), or
// "none" when no version is deployed.
func versionLabel(v int) string {
	if v <= 0 {
		return "none"
	}
	return fmt.Sprintf("v%d", v)
}

// RebuildConfigFingerprint hashes the execution-relevant configuration a
// rebuild runs under: the effective deploy block (strategy, replicas,
// runtime, network), resource limits, and the resolved port. It is captured
// at plan creation and re-derived at apply time — identical inputs, identical
// function, single source of truth for "which configuration can invalidate a
// plan". Deliberately excludes everything volatile (health, logs, metrics,
// timestamps): a plan must go stale because execution semantics changed, not
// because an unrelated log line appeared.
func RebuildConfigFingerprint(appName, sourceDir string) (string, error) {
	cfg, err := project.Load(sourceDir)
	if err != nil && phelixerr.CodeOf(err) != phelixerr.CodeNotFound {
		return "", err
	}
	state, stateErr := deploy.Load(appName)
	if stateErr != nil && !os.IsNotExist(stateErr) {
		return "", stateErr
	}

	type fingerprint struct {
		Strategy  string `json:"strategy,omitempty"`
		Replicas  int    `json:"replicas,omitempty"`
		Runtime   string `json:"runtime,omitempty"`
		Network   string `json:"network,omitempty"`
		Resources struct {
			CPU    string `json:"cpu,omitempty"`
			Memory string `json:"memory,omitempty"`
		} `json:"resources,omitempty"`
		RecordedPort   int    `json:"recorded_port,omitempty"`
		DeployedMode   string `json:"deployed_mode,omitempty"`
		DeployedPort   int    `json:"deployed_port,omitempty"`
		DeployedActive int    `json:"deployed_active_version,omitempty"`
	}
	var fp fingerprint
	if cfg != nil {
		if cfg.Deploy != nil {
			fp.Strategy = cfg.Deploy.Strategy
			fp.Replicas = cfg.Deploy.Replicas
			fp.Runtime = cfg.DeployRuntime()
			fp.Network = cfg.DeployNetwork()
		}
		fp.Resources.CPU = cfg.Resources.CPU
		fp.Resources.Memory = cfg.Resources.Memory
	}
	if state != nil {
		fp.DeployedMode = string(state.Mode)
		fp.DeployedPort = state.PublicPort
		fp.DeployedActive = state.ActiveVersion
	}
	if info := lookupApp(appName); info != nil {
		fp.RecordedPort = info.Port
	}
	data, err := json.Marshal(fp)
	if err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeInvalidArgument, "canonicalize config fingerprint", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// lookupApp resolves an app by name or ID through the app manager (the same
// state file every command reads).
func lookupApp(identifier string) *app.AppInfo {
	if err := app.Manager.LoadState(); err != nil {
		return nil
	}
	if m, ok := app.Manager.(*app.AppManager); ok {
		if info, exists := m.Apps[identifier]; exists && info != nil {
			return info
		}
		for _, info := range m.Apps {
			if info != nil && info.Name == identifier {
				return info
			}
		}
	}
	return nil
}

// Evaluate re-derives every precondition from current state and returns the
// failures. An empty result means the plan still describes exactly what
// execution would do.
func (p *Plan) Evaluate() []FailedPrecondition {
	failures := make([]FailedPrecondition, 0)
	for _, pre := range p.Preconditions {
		actual, ok := evaluatePrecondition(p, pre)
		if !ok {
			failures = append(failures, FailedPrecondition{Type: pre.Type, Expected: pre.Expected, Actual: actual})
		}
	}
	return failures
}

// evaluatePrecondition re-derives one precondition from current state. ok is
// true when the current value equals the expected one; actual always carries
// the canonical current value.
func evaluatePrecondition(p *Plan, pre Precondition) (actual string, ok bool) {
	appName := p.Action.Application
	switch pre.Type {
	case PreconditionAppExists:
		info := lookupApp(appName)
		if info == nil {
			return "missing", false
		}
		// The app identity must not have been recreated under the same name.
		current := info.ID + "/" + info.Name
		expected := pre.Expected + "/" + appName
		return current, current == expected

	case PreconditionCurrentVersion:
		cur, err := deploy.CurrentVersion(appName)
		if err != nil {
			return "none", pre.Expected == "none"
		}
		return versionLabel(cur), versionLabel(cur) == pre.Expected

	case PreconditionVersionExists:
		if _, _, err := deploy.VersionPaths(appName, p.Inputs.TargetVersion); err != nil {
			return "missing", false
		}
		return versionLabel(p.Inputs.TargetVersion), true

	case PreconditionDeployMode:
		state, err := deploy.Load(appName)
		mode := ""
		if err == nil && state != nil {
			mode = string(state.Mode)
		}
		if mode == "" {
			mode = "classic"
		}
		return mode, mode == pre.Expected

	case PreconditionConfigMatch:
		fp, err := RebuildConfigFingerprint(appName, p.Inputs.SourceDir)
		if err != nil {
			return "unreadable", false
		}
		return fp, fp == pre.Expected

	case PreconditionSourceCommit:
		commit := deploy.DetectGitCommit(p.Inputs.SourceDir)
		if commit == "" {
			// The source stopped being a git work tree entirely — the source
			// the plan was created from is no longer verifiably the same.
			return "none", pre.Expected == "none"
		}
		return commit, commit == pre.Expected

	case PreconditionToolchain:
		lang := builder.ParseLanguage(p.Target.Language)
		if toolchain.IsInstalled(lang) {
			return "installed", true
		}
		return "missing", false

	default:
		// Unknown precondition types fail closed: a plan written by a newer
		// schema must not execute under semantics that cannot check it.
		return "unchecked", false
	}
}

// EvaluateCapabilities returns the plan's required capabilities that are not
// present in the given available set (the agent build's registry). Capabilities
// are technical ability, never authorization.
func EvaluateCapabilities(p *Plan, available []string) []string {
	have := make(map[string]bool, len(available))
	for _, c := range available {
		have[c] = true
	}
	missing := make([]string, 0)
	for _, c := range p.Capabilities {
		if !have[c] {
			missing = append(missing, c)
		}
	}
	return missing
}
