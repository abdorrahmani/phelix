package project

import (
	"regexp"
	"strings"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// WebhookConfig is the phelix.yaml shape of the Git-push webhook trigger
// (internal/webhook). The shared secret itself is never stored in the file —
// only the name of the environment variable that holds it, so a committed
// phelix.yaml never leaks the secret.
type WebhookConfig struct {
	// Enabled turns the webhook trigger on for this app. Defaults to false;
	// an app without a webhook section never accepts webhook deliveries.
	Enabled bool `yaml:"enabled"`
	// Branch is the bare branch name whose pushes trigger a rebuild
	// (e.g. "main", not "refs/heads/main").
	Branch string `yaml:"branch"`
	// SecretEnv names the environment variable holding the HMAC-SHA256
	// shared secret (e.g. PHELIX_WEBHOOK_SECRET). `phelix webhook` fails at
	// startup when it is unset for an app that enables the webhook.
	SecretEnv string `yaml:"secret_env"`
}

// envNamePattern matches a valid environment variable name.
var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validate checks the webhook section. Presence-dependent fields (branch,
// secret_env) are required when enabled; declared values are always checked
// so an invalid file can never silently disable or mis-route the trigger.
// Every error names the yaml path, matching the other sections' style.
func (w *WebhookConfig) validate() error {
	if w == nil {
		return nil
	}

	if strings.TrimSpace(w.SecretEnv) != "" && !envNamePattern.MatchString(strings.TrimSpace(w.SecretEnv)) {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: invalid webhook.secret_env %q\nHint: name an environment variable like PHELIX_WEBHOOK_SECRET (letters, digits, underscore; must not start with a digit)", w.SecretEnv)
	}

	branch := strings.TrimSpace(w.Branch)
	if branch != "" {
		if strings.HasPrefix(branch, "refs/") {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: invalid webhook.branch %q\nHint: use the bare branch name (e.g. \"main\"), not a full ref", w.Branch)
		}
		if strings.Contains(branch, "..") || strings.HasPrefix(branch, "-") ||
			strings.HasPrefix(branch, "/") || strings.HasSuffix(branch, "/") ||
			strings.ContainsAny(branch, " \t\r\n~^:?*[\\") {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: invalid webhook.branch %q\nHint: use a plain branch name like \"main\" or \"release/2.x\"", w.Branch)
		}
	}

	if !w.Enabled {
		return nil
	}
	if branch == "" {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: webhook.enabled requires webhook.branch\nHint: set the branch whose pushes trigger the rebuild, e.g. branch: main")
	}
	if strings.TrimSpace(w.SecretEnv) == "" {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: webhook.enabled requires webhook.secret_env\nHint: name the environment variable holding the shared secret, e.g. secret_env: PHELIX_WEBHOOK_SECRET (never put the secret itself in phelix.yaml)")
	}
	return nil
}
