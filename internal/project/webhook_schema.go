package project

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"go.yaml.in/yaml/v3"
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

// Validate is the exported entry to the webhook section's validation — the
// same rules project.Load enforces. Remote webhook mutations validate the
// configuration they are about to persist through it, so a remote change can
// never write a config the local webhook server would reject at load time.
func (w *WebhookConfig) Validate() error { return w.validate() }

// SaveWebhookConfig writes the webhook section into dir/phelix.yaml while
// preserving every other key, comment, and the file's overall formatting:
// the existing document is edited as a yaml.Node and only the `webhook`
// mapping is replaced (or appended). A missing file is created with just the
// webhook section; a malformed existing file is an error, never overwritten.
// A nil config removes nothing — use a zero-value config with enabled=false
// to persist an explicit disabled state.
func SaveWebhookConfig(dir string, wc *WebhookConfig) error {
	if wc == nil {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "cannot save a nil webhook config")
	}
	if err := wc.validate(); err != nil {
		return err
	}
	path := filepath.Join(dir, FileName)

	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to read %s", path)
		}
		// New file: the standard header plus just the webhook section.
		body, merr := yaml.Marshal(map[string]*WebhookConfig{"webhook": wc})
		if merr != nil {
			return phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to encode webhook config", merr)
		}
		return os.WriteFile(path, append([]byte(webhookConfigHeader()), body...), 0o644)
	}

	var doc yaml.Node
	if uerr := yaml.Unmarshal(raw, &doc); uerr != nil {
		return phelixerr.Wrapf(phelixerr.CodeConfiguration, uerr, "%s is malformed; not modifying it", path)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return phelixerr.Newf(phelixerr.CodeConfiguration, "%s is malformed: top level is not a mapping; not modifying it", path)
	}
	root := doc.Content[0]

	newValue := &yaml.Node{}
	if eerr := newValue.Encode(wc); eerr != nil {
		return phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to encode webhook config", eerr)
	}

	replaced := false
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Kind == yaml.ScalarNode && root.Content[i].Value == "webhook" {
			// Keep the key node (its comments survive); swap only the value.
			*root.Content[i+1] = *newValue
			replaced = true
			break
		}
	}
	if !replaced {
		key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "webhook"}
		root.Content = append(root.Content, key, newValue)
	}

	// Marshal the document node (not the root mapping): the file's leading
	// comments live on the document node and would be dropped otherwise.
	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if eerr := enc.Encode(&doc); eerr != nil {
		return phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to encode config", eerr)
	}
	if cerr := enc.Close(); cerr != nil {
		return phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to encode config", cerr)
	}
	return os.WriteFile(path, []byte(buf.String()), 0o644)
}

func webhookConfigHeader() string {
	return fmt.Sprintf("# Phelix project configuration.\n# CLI flags override these values (e.g. --port).\n")
}

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
