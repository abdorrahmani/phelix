package project

import (
	"regexp"
	"strconv"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// SecretsConfig is the phelix.yaml shape of the local secret management
// layer. It declares which secrets the application consumes and which
// environment variable names they are exposed under. The secrets themselves
// live only in the encrypted secret store (internal/secrets); the file carries
// names, never values, so a committed phelix.yaml cannot leak a credential.
//
// Secrets are resolved from the store when the application starts or
// restarts, not when it is built. Rolling back to an earlier version therefore
// never brings back an earlier secret value.
type SecretsConfig struct {
	// Imports maps one secret from the app's secret store onto one
	// environment variable name that the application receives at launch.
	Imports []SecretImport `yaml:"imports,omitempty"`
}

// SecretImport is one secret-to-environment binding.
type SecretImport struct {
	// Secret is the name of the secret inside the app's secret store, for
	// example database_url. The store is managed with `phelix secrets`.
	Secret string `yaml:"secret"`
	// Env is the environment variable name the application receives, for
	// example DATABASE_URL. It must not be a name Phelix reserves.
	Env string `yaml:"env"`
}

// secretNamePattern matches a valid secret name in the store: lowercase
// letters, digits, hyphens and underscores, starting with a letter.
var secretNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// reservedEnvNames lists the environment variable names Phelix controls
// itself. An import may not target one of them: the launcher always writes
// its own value last, so an import bound to a reserved name would never be
// readable and only mislead.
var reservedEnvNames = map[string]bool{
	"PORT": true,
}

// Validate is the exported entry to the secrets section's validation, the
// same rules project.Load enforces. Deploy and apply paths run it before they
// persist any binding, so an invalid import set can never reach the store.
func (s *SecretsConfig) Validate() error { return s.validate() }

// validate checks every import: both fields must be present and well-formed,
// no import may target a reserved name, and each environment variable name
// may be targeted only once. The same secret may feed several variable names.
// Every error names the yaml path and the offending entry, matching the other
// sections' style.
func (s *SecretsConfig) validate() error {
	if s == nil {
		return nil
	}
	seenEnv := make(map[string]int, len(s.Imports))
	for i, imp := range s.Imports {
		where := func(field string) string {
			return "secrets.imports[" + strconv.Itoa(i) + "]." + field
		}
		secret := imp.Secret
		if secret == "" {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: %s is required\nHint: name the secret as it exists in the store, e.g. secret: database_url", where("secret"))
		}
		if !secretNamePattern.MatchString(secret) {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: invalid %s %q\nHint: use a lowercase name like database_url (letters, digits, hyphen, underscore; must start with a letter)", where("secret"), imp.Secret)
		}
		env := imp.Env
		if env == "" {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: %s is required\nHint: name the environment variable the app receives, e.g. env: DATABASE_URL", where("env"))
		}
		if !envNamePattern.MatchString(env) {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: invalid %s %q\nHint: use an environment variable name like DATABASE_URL (letters, digits, underscore; must not start with a digit)", where("env"), imp.Env)
		}
		if reservedEnvNames[env] {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: %s targets the reserved name %s\nHint: Phelix manages this variable itself; choose a different name for the import", where("env"), env)
		}
		if prev, dup := seenEnv[env]; dup {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: %s targets %s, already targeted by secrets.imports[%d]\nHint: each environment variable name may be declared once; the same secret can feed several names", where("env"), env, prev)
		}
		seenEnv[env] = i
	}
	return nil
}

// IsReservedEnvName reports whether an environment variable name is managed
// by Phelix itself and can never be a secret import target.
func IsReservedEnvName(env string) bool {
	return reservedEnvNames[env]
}
