package project

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

const (
	HookPreBuild     = "pre_build"
	HookPostBuild    = "post_build"
	HookPreDeploy    = "pre_deploy"
	HookPostStart    = "post_start"
	HookPreTraffic   = "pre_traffic"
	HookPostDeploy   = "post_deploy"
	HookPreStop      = "pre_stop"
	HookPostRollback = "post_rollback"
)

var HookNames = []string{
	HookPreBuild, HookPostBuild, HookPreDeploy, HookPostStart,
	HookPreTraffic, HookPostDeploy, HookPreStop, HookPostRollback,
}

const (
	DefaultHookTimeout       = 60 * time.Second
	DefaultHookRetryInterval = 2 * time.Second
	MaxHookRetries           = 10
	MaxHookTimeout           = 30 * time.Minute
)

type HookConfig struct {
	Command         []string          `yaml:"command"`
	Timeout         string            `yaml:"timeout,omitempty"`
	Retries         int               `yaml:"retries,omitempty"`
	RetryInterval   string            `yaml:"retry_interval,omitempty"`
	ContinueOnError *bool             `yaml:"continue_on_error,omitempty"`
	WorkingDir      string            `yaml:"working_dir,omitempty"`
	Env             map[string]string `yaml:"env,omitempty"`
	Enabled         *bool             `yaml:"enabled,omitempty"`
}

type HooksConfig struct {
	PreBuild     *HookConfig `yaml:"pre_build,omitempty"`
	PostBuild    *HookConfig `yaml:"post_build,omitempty"`
	PreDeploy    *HookConfig `yaml:"pre_deploy,omitempty"`
	PostStart    *HookConfig `yaml:"post_start,omitempty"`
	PreTraffic   *HookConfig `yaml:"pre_traffic,omitempty"`
	PostDeploy   *HookConfig `yaml:"post_deploy,omitempty"`
	PreStop      *HookConfig `yaml:"pre_stop,omitempty"`
	PostRollback *HookConfig `yaml:"post_rollback,omitempty"`
}

// Get returns the configured hook by name, or nil when it is absent.
func (h *HooksConfig) Get(name string) *HookConfig {
	if h == nil {
		return nil
	}
	switch name {
	case HookPreBuild:
		return h.PreBuild
	case HookPostBuild:
		return h.PostBuild
	case HookPreDeploy:
		return h.PreDeploy
	case HookPostStart:
		return h.PostStart
	case HookPreTraffic:
		return h.PreTraffic
	case HookPostDeploy:
		return h.PostDeploy
	case HookPreStop:
		return h.PreStop
	case HookPostRollback:
		return h.PostRollback
	}
	return nil
}

// Set stores a hook by name. Unknown names are ignored.
func (h *HooksConfig) Set(name string, cfg *HookConfig) {
	if h == nil {
		return
	}
	switch name {
	case HookPreBuild:
		h.PreBuild = cfg
	case HookPostBuild:
		h.PostBuild = cfg
	case HookPreDeploy:
		h.PreDeploy = cfg
	case HookPostStart:
		h.PostStart = cfg
	case HookPreTraffic:
		h.PreTraffic = cfg
	case HookPostDeploy:
		h.PostDeploy = cfg
	case HookPreStop:
		h.PreStop = cfg
	case HookPostRollback:
		h.PostRollback = cfg
	}
}

// Configured returns the hooks declared in the file, in execution order.
func (h *HooksConfig) Configured() []string {
	if h == nil {
		return nil
	}
	var out []string
	for _, name := range HookNames {
		if h.Get(name) != nil {
			out = append(out, name)
		}
	}
	return out
}

// Enabled reports whether name is configured and not explicitly disabled.
func (h *HooksConfig) Enabled(name string) bool {
	cfg := h.Get(name)
	if cfg == nil {
		return false
	}
	return cfg.IsEnabled()
}

// IsEnabled reports whether the hook is enabled. An absent enabled key means
// enabled.
func (c *HookConfig) IsEnabled() bool {
	return c != nil && (c.Enabled == nil || *c.Enabled)
}

// TimeoutDuration returns the resolved timeout.
func (c *HookConfig) TimeoutDuration() time.Duration {
	if c == nil {
		return DefaultHookTimeout
	}
	d, err := time.ParseDuration(strings.TrimSpace(c.Timeout))
	if err != nil || d <= 0 {
		return DefaultHookTimeout
	}
	return d
}

// RetryIntervalDuration returns the resolved first-retry delay.
func (c *HookConfig) RetryIntervalDuration() time.Duration {
	if c == nil {
		return DefaultHookRetryInterval
	}
	d, err := time.ParseDuration(strings.TrimSpace(c.RetryInterval))
	if err != nil || d < 0 {
		return DefaultHookRetryInterval
	}
	return d
}

// ContinuesOnError returns the resolved failure policy. post_deploy,
// post_rollback and pre_stop default to continuing: a notification or shutdown
// hook must not turn a completed deployment or a successful stop into a
// failure.
func (c *HookConfig) ContinuesOnError(name string) bool {
	if c != nil && c.ContinueOnError != nil {
		return *c.ContinueOnError
	}
	switch name {
	case HookPostDeploy, HookPostRollback, HookPreStop:
		return true
	}
	return false
}

var hookEnvKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Validate checks every field of every declared hook. Errors name the exact
// yaml path so a user can fix the file without reading source.
func (h *HooksConfig) Validate(projectDir string) error {
	if h == nil {
		return nil
	}
	for _, name := range HookNames {
		cfg := h.Get(name)
		if cfg == nil {
			continue
		}
		path := "hooks." + name
		if len(cfg.Command) == 0 {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: %s is missing required field: command\nHint: provide an argument array, e.g. command: [\"./bin/migrate\", \"up\"]", path)
		}
		if strings.TrimSpace(cfg.Command[0]) == "" {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: %s.command[0] must name an executable", path)
		}
		for i, arg := range cfg.Command {
			if strings.ContainsRune(arg, 0) {
				return phelixerr.Newf(phelixerr.CodeConfiguration,
					"configuration error: %s.command[%d] contains a NUL byte", path, i)
			}
		}
		if cfg.Timeout != "" {
			d, err := time.ParseDuration(strings.TrimSpace(cfg.Timeout))
			if err != nil || d <= 0 {
				return phelixerr.Newf(phelixerr.CodeConfiguration,
					"configuration error: %s has invalid timeout %q\nHint: use a positive duration like 30s or 2m", path, cfg.Timeout)
			}
			if d > MaxHookTimeout {
				return phelixerr.Newf(phelixerr.CodeConfiguration,
					"configuration error: %s.timeout %s exceeds the %s maximum", path, cfg.Timeout, MaxHookTimeout)
			}
		}
		if cfg.Retries < 0 || cfg.Retries > MaxHookRetries {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: %s.retries %d is out of range\nHint: retries must be between 0 and %d", path, cfg.Retries, MaxHookRetries)
		}
		if cfg.RetryInterval != "" {
			d, err := time.ParseDuration(strings.TrimSpace(cfg.RetryInterval))
			if err != nil || d < 0 {
				return phelixerr.Newf(phelixerr.CodeConfiguration,
					"configuration error: %s has invalid retry_interval %q\nHint: use a duration like 5s (0 is allowed)", path, cfg.RetryInterval)
			}
		}
		if cfg.WorkingDir != "" {
			if filepath.IsAbs(cfg.WorkingDir) {
				return phelixerr.Newf(phelixerr.CodeConfiguration,
					"configuration error: %s.working_dir must be relative to the project directory, got %q", path, cfg.WorkingDir)
			}
			clean := filepath.Clean(cfg.WorkingDir)
			if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
				return phelixerr.Newf(phelixerr.CodeConfiguration,
					"configuration error: %s.working_dir %q escapes the project directory", path, cfg.WorkingDir)
			}
			if projectDir != "" {
				root, err := filepath.Abs(projectDir)
				if err != nil {
					return phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to resolve the project directory", err)
				}
				target, err := filepath.Abs(filepath.Join(root, clean))
				if err != nil {
					return phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to resolve hooks working directory", err)
				}
				if rel, rerr := filepath.Rel(root, target); rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					return phelixerr.Newf(phelixerr.CodeConfiguration,
						"configuration error: %s.working_dir %q escapes the project directory", path, cfg.WorkingDir)
				}
			}
		}
		if (name == HookPostDeploy || name == HookPostRollback) && cfg.ContinueOnError != nil && !*cfg.ContinueOnError {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: %s cannot be critical because the deployment has already completed\nHint: remove continue_on_error or set it to true", path)
		}
		for key := range cfg.Env {
			if !hookEnvKey.MatchString(key) {
				return phelixerr.Newf(phelixerr.CodeConfiguration,
					"configuration error: %s.env has invalid variable name %q\nHint: names must match [A-Za-z_][A-Za-z0-9_]*", path, key)
			}
			if strings.HasPrefix(key, "PHELIX_") {
				return phelixerr.Newf(phelixerr.CodeConfiguration,
					"configuration error: %s.env must not override reserved name %q", path, key)
			}
		}
	}
	return nil
}

// Describe renders one hook for `phelix hooks list`.
func (c *HookConfig) Describe(name string) string {
	state := "enabled"
	if !c.IsEnabled() {
		state = "disabled"
	}
	attempts := 1 + c.Retries
	return fmt.Sprintf("%-14s %-8s timeout=%s retries=%s continue_on_error=%s command=%s",
		name, state, c.TimeoutDuration(), strconv.Itoa(attempts),
		strconv.FormatBool(c.ContinuesOnError(name)),
		strings.Join(c.Command, " "))
}

// HookTypes returns the supported hook names, sorted for stable display.
func HookTypes() []string {
	out := append([]string(nil), HookNames...)
	sort.Strings(out)
	return out
}
