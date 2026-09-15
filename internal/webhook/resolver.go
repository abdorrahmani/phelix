package webhook

import (
	"strings"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/project"
)

// AppInfo is the webhook layer's view of a managed application.
type AppInfo struct {
	ID        string
	Name      string
	Directory string
}

// ResolvedApp is an app together with its webhook configuration. Config is
// nil when the app's phelix.yaml declares no webhook section (the webhook is
// disabled for that app).
type ResolvedApp struct {
	Info   AppInfo
	Config *project.WebhookConfig
}

// AppResolver maps a webhook path identifier (<app>) to a managed
// application. Injected into the Server so tests can serve fixed registries.
type AppResolver interface {
	Resolve(identifier string) (*ResolvedApp, error)
}

// registryResolver resolves identifiers through the app manager's persisted
// state (refreshed from disk on every lookup, so apps added or removed while
// the server runs are picked up) and reads each app's phelix.yaml from its
// project directory.
type registryResolver struct{}

// NewRegistryResolver returns the production resolver (ID first, then name —
// the same convention CLI commands use).
func NewRegistryResolver() AppResolver { return &registryResolver{} }

func (r *registryResolver) Resolve(identifier string) (*ResolvedApp, error) {
	for _, it := range app.Manager.ListApplications() {
		if it.ID == identifier || it.Name == identifier {
			return resolveAppDir(AppInfo{ID: it.ID, Name: it.Name, Directory: it.Directory})
		}
	}
	return nil, phelixerr.Newf(phelixerr.CodeNotFound, "unknown application %q", identifier)
}

// resolveAppDir loads the app's webhook configuration from its project
// directory. A missing phelix.yaml means "webhook disabled", matching how the
// CLI treats a missing config as no-config; a present-but-invalid file is an
// error (an invalid configuration is never silently ignored).
func resolveAppDir(info AppInfo) (*ResolvedApp, error) {
	if info.Directory == "" {
		return nil, phelixerr.Newf(phelixerr.CodeConfiguration,
			"application %q has no project directory", info.Name)
	}
	cfg, err := project.Load(info.Directory)
	if err != nil {
		if phelixerr.CodeOf(err) == phelixerr.CodeNotFound {
			return &ResolvedApp{Info: info, Config: nil}, nil
		}
		return nil, phelixerr.Wrapf(phelixerr.CodeConfiguration, err,
			"application %q: invalid %s in %s", info.Name, project.FileName, info.Directory)
	}
	return &ResolvedApp{Info: info, Config: cfg.Webhook}, nil
}

// ValidateStartupSecrets fails clearly at startup when any app that enables
// the webhook does not have its secret environment variable set. Apps without
// the webhook enabled are skipped; an app whose phelix.yaml cannot be read is
// logged and skipped (its deliveries fail closed per-request) so unrelated
// breakage cannot block the server — the spec-required failure (enabled +
// missing secret) is the only hard failure here.
func ValidateStartupSecrets(lookupSecret func(string) (string, bool)) error {
	var apps []AppInfo
	for _, it := range app.Manager.ListApplications() {
		apps = append(apps, AppInfo{ID: it.ID, Name: it.Name, Directory: it.Directory})
	}
	return validateSecrets(apps, lookupSecret)
}

// validateSecrets is the testable core of ValidateStartupSecrets.
func validateSecrets(apps []AppInfo, lookupSecret func(string) (string, bool)) error {
	if lookupSecret == nil {
		lookupSecret = func(string) (string, bool) { return "", false }
	}
	for _, info := range apps {
		resolved, err := resolveAppDir(info)
		if err != nil {
			logs.Warning("webhook", "skipping startup webhook check for app %s: %v", info.Name, err)
			continue
		}
		if resolved.Config == nil || !resolved.Config.Enabled {
			continue
		}
		if v, ok := lookupSecret(resolved.Config.SecretEnv); !ok || strings.TrimSpace(v) == "" {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"application %q enables the webhook but the shared secret is not set: environment variable %s (webhook.secret_env) is empty\nHint: set it in the environment of 'phelix webhook' (e.g. systemd Environment= or an env file); the secret itself is never stored in phelix.yaml",
				info.Name, resolved.Config.SecretEnv)
		}
	}
	return nil
}
