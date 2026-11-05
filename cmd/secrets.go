package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/abdorrahmani/phelix/internal/secrets"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var secretsJSON bool
var secretsFromFile string
var secretsNoApply bool
var secretsNoRestart bool

// SecretsCmd groups the local secret management commands. Secrets live in the
// app's encrypted store, are referenced from phelix.yaml by name only, and
// are never printed by any of these commands.
var SecretsCmd = &cobra.Command{
	Use:   "secrets",
	Short: "Manage application secrets: encrypted, versioned, resolved at launch",
	Long: `Manage application secrets.

Secrets are stored encrypted per application, versioned, and resolved when
the application starts or restarts. phelix.yaml declares which secret is
exposed under which environment variable name; values never appear in the
file, in output, or in logs.

Examples:
  phelix secrets list
  phelix secrets set database_url < value.txt
  phelix secrets set api_key --from-file key.txt
  phelix secrets get database_url
  phelix secrets history database_url
  phelix secrets remove database_url
  phelix secrets apply`,
	Args: cobra.NoArgs,
}

func init() {
	secretsList := &cobra.Command{
		Use:           "list [ID|AppName]",
		Short:         "List secrets and their bindings (metadata only, never values)",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			restore := enterJSONIfRequested()
			defer restore()
			ref, err := resolveSecretsApp(args)
			if err != nil {
				return err
			}
			return runSecretsList(ref)
		},
	}
	secretsGet := &cobra.Command{
		Use:           "get NAME [ID|AppName]",
		Short:         "Show a secret's metadata: version, fingerprint, bindings (never the value)",
		Args:          cobra.RangeArgs(1, 2),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			restore := enterJSONIfRequested()
			defer restore()
			ref, err := resolveSecretsApp(args[1:])
			if err != nil {
				return err
			}
			return runSecretsGet(ref, args[0])
		},
	}
	secretsSet := &cobra.Command{
		Use:           "set NAME [ID|AppName]",
		Short:         "Store a new version of a secret from stdin or --from-file; values are never passed as arguments",
		Args:          cobra.RangeArgs(1, 2),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			restore := enterJSONIfRequested()
			defer restore()
			ref, err := resolveSecretsApp(args[1:])
			if err != nil {
				return err
			}
			return runSecretsSet(ref, args[0])
		},
	}
	secretsSet.Flags().StringVar(&secretsFromFile, "from-file", "", "Read the value from this file, byte for byte")
	secretsSet.Flags().BoolVar(&secretsNoApply, "no-apply", false, "Store only; do not restart the application")

	secretsHistory := &cobra.Command{
		Use:           "history NAME [ID|AppName]",
		Short:         "List retained versions of a secret (fingerprints only, never values)",
		Args:          cobra.RangeArgs(1, 2),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			restore := enterJSONIfRequested()
			defer restore()
			ref, err := resolveSecretsApp(args[1:])
			if err != nil {
				return err
			}
			return runSecretsHistory(ref, args[0])
		},
	}
	secretsRemove := &cobra.Command{
		Use:           "remove NAME [ID|AppName]",
		Short:         "Delete a secret and all retained versions; refused while still imported",
		Args:          cobra.RangeArgs(1, 2),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			restore := enterJSONIfRequested()
			defer restore()
			ref, err := resolveSecretsApp(args[1:])
			if err != nil {
				return err
			}
			return runSecretsRemove(ref, args[0])
		},
	}
	secretsApply := &cobra.Command{
		Use:           "apply [ID|AppName]",
		Short:         "Make the imports in phelix.yaml the active bindings and restart the app if it is running",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			restore := enterJSONIfRequested()
			defer restore()
			ref, err := resolveSecretsApp(args)
			if err != nil {
				return err
			}
			return runSecretsApply(ref)
		},
	}
	secretsApply.Flags().BoolVar(&secretsNoRestart, "no-restart", false, "Commit the bindings only; do not restart the application")

	for _, c := range []*cobra.Command{secretsList, secretsGet, secretsSet, secretsHistory, secretsRemove, secretsApply} {
		c.Flags().BoolVar(&secretsJSON, "json", false, "Emit the machine-readable envelope")
	}

	SecretsCmd.AddCommand(secretsList, secretsGet, secretsSet, secretsHistory, secretsRemove, secretsApply)
}

// enterJSONIfRequested switches output to the machine envelope when --json is
// set and returns the restore function.
func enterJSONIfRequested() func() {
	if secretsJSON {
		return machine.EnterJSON()
	}
	return func() {}
}

// appInfoRef pairs the resolved application record with the convenience
// accessors every subcommand shares.
type appInfoRef struct {
	AppInfo *app.AppInfo
}

func (a *appInfoRef) ID() string        { return a.AppInfo.ID }
func (a *appInfoRef) Name() string      { return a.AppInfo.Name }
func (a *appInfoRef) Directory() string { return a.AppInfo.Directory }

// resolveSecretsApp finds the application the command operates on: an
// explicit ID or name argument, or the project in the current directory.
func resolveSecretsApp(args []string) (*appInfoRef, error) {
	if err := app.Manager.LoadState(); err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load state", err)
	}
	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		info, err := GetAppInfo(strings.TrimSpace(args[0]))
		if err != nil {
			return nil, err
		}
		return &appInfoRef{AppInfo: info}, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to get current directory", err)
	}
	cfg, err := project.Load(dir)
	if err != nil {
		return nil, phelixerr.New(phelixerr.CodeInvalidArgument,
			"no application given and no phelix.yaml in the current directory\nHint: run from the project directory or pass an app: phelix secrets list MyApp")
	}
	if strings.TrimSpace(cfg.Name) == "" {
		return nil, phelixerr.New(phelixerr.CodeInvalidArgument,
			"phelix.yaml has no name; pass an application explicitly: phelix secrets list MyApp")
	}
	info, err := GetAppInfo(cfg.Name)
	if err != nil {
		return nil, err
	}
	return &appInfoRef{AppInfo: info}, nil
}

// runSecretsList prints every secret with its metadata and bindings, plus
// declared imports that have no value in the store yet.
func runSecretsList(ref *appInfoRef) error {
	st, err := secrets.Open(ref.ID())
	if err != nil {
		return err
	}
	metas := st.List()
	sortSecretMetas(metas)
	missing := missingImportsFor(ref, st)

	fmt.Printf("Secrets for '%s' (ID: %s):\n", ref.Name(), ref.ID())
	if len(metas) == 0 && len(missing) == 0 {
		fmt.Printf("  none stored; declare imports in phelix.yaml and create them with `phelix secrets set`\n")
	}
	for _, m := range metas {
		bound := strings.Join(m.BoundEnvs, ", ")
		if bound == "" {
			bound = "-"
		}
		fmt.Printf("  %s  v%d  %s  updated %s  imported as %s\n",
			color.CyanString(m.Name), m.Version, m.Fingerprint, m.UpdatedAt.Format(time.RFC3339), bound)
	}
	for _, imp := range missing {
		fmt.Printf("  %s  MISSING  expected as %s (create it with `phelix secrets set %s`)\n",
			color.YellowString(imp.Secret), imp.Env, imp.Secret)
	}
	if st.LastApply != nil {
		fmt.Printf("  last apply: %s (restart: %s)\n", st.LastApply.At.Format(time.RFC3339), st.LastApply.Restart)
	}
	if secretsJSON {
		type secretJSON struct {
			Name        string   `json:"name"`
			Version     int      `json:"version"`
			Fingerprint string   `json:"fingerprint"`
			UpdatedAt   string   `json:"updated_at"`
			BoundEnvs   []string `json:"bound_envs"`
		}
		type listResult struct {
			App            string       `json:"app"`
			AppID          string       `json:"app_id"`
			Secrets        []secretJSON `json:"secrets"`
			MissingImports []string     `json:"missing_imports"`
			LastApply      string       `json:"last_apply,omitempty"`
		}
		res := listResult{App: ref.Name(), AppID: ref.ID(), Secrets: []secretJSON{}, MissingImports: []string{}}
		for _, m := range metas {
			res.Secrets = append(res.Secrets, secretJSON{Name: m.Name, Version: m.Version, Fingerprint: m.Fingerprint, UpdatedAt: m.UpdatedAt.Format(time.RFC3339), BoundEnvs: m.BoundEnvs})
		}
		for _, imp := range missing {
			res.MissingImports = append(res.MissingImports, imp.Secret)
		}
		if st.LastApply != nil {
			res.LastApply = st.LastApply.Restart + " at " + st.LastApply.At.Format(time.RFC3339)
		}
		return writeEnvelopeResult(machine.Success("", res))
	}
	return nil
}

// runSecretsGet prints one secret's metadata. There is deliberately no code
// path in this command that decrypts the value.
func runSecretsGet(ref *appInfoRef, name string) error {
	st, err := secrets.Open(ref.ID())
	if err != nil {
		return err
	}
	meta, err := st.Peek(name)
	if err != nil {
		return err
	}
	fmt.Printf("Secret '%s' for '%s' (metadata only; the value is never shown):\n", name, ref.Name())
	fmt.Printf("  version:     %d\n", meta.Version)
	fmt.Printf("  fingerprint: %s\n", meta.Fingerprint)
	fmt.Printf("  created:     %s\n", meta.CreatedAt.Format(time.RFC3339))
	fmt.Printf("  updated:     %s\n", meta.UpdatedAt.Format(time.RFC3339))
	fmt.Printf("  imported as: %s\n", strings.Join(meta.BoundEnvs, ", "))
	if secretsJSON {
		getResult := struct {
			App         string   `json:"app"`
			Name        string   `json:"name"`
			Version     int      `json:"version"`
			Fingerprint string   `json:"fingerprint"`
			CreatedAt   string   `json:"created_at"`
			UpdatedAt   string   `json:"updated_at"`
			BoundEnvs   []string `json:"bound_envs"`
		}{ref.Name(), name, meta.Version, meta.Fingerprint, meta.CreatedAt.Format(time.RFC3339), meta.UpdatedAt.Format(time.RFC3339), meta.BoundEnvs}
		return writeEnvelopeResult(machine.Success("", getResult))
	}
	return nil
}

// runSecretsSet stores a new version, then applies it: when the secret is
// imported by the running application, the application restarts through the
// same path `phelix restart` uses. A failed restart leaves the new value
// stored (the desired state) while the runtime keeps the previous values.
func runSecretsSet(ref *appInfoRef, name string) error {
	value, err := readSecretValue(secretsFromFile)
	if err != nil {
		return err
	}
	release, err := deploy.AcquireDeployLock(ref.Name(), "secrets-set")
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeDeployLocked, "could not acquire deploy lock", err)
	}
	defer release()

	res, err := secrets.Set(ref.ID(), name, value)
	if err != nil {
		return err
	}
	secrets.RegisterResolvedValue(value)
	defer secrets.UnregisterResolvedValue(value)

	fmt.Printf("✓ Stored '%s' version %d (fingerprint %s)\n", name, res.Version, res.Fingerprint)
	phelixgrpc.ReportEvent(ref.ID(), ref.Name(), "secret_change", true, "", 0, "", "")

	bound, err := secretIsBound(ref.ID(), name)
	if err != nil {
		return err
	}
	if !bound || secretsNoApply {
		return writeSetEnvelope(ref, name, res, "not_requested")
	}
	if !appIsRunning(ref) {
		if err := recordApply(ref.ID(), secrets.ApplyRestartSkipped); err != nil {
			return err
		}
		fmt.Printf("  application is not running; the value applies at the next start\n")
		return writeSetEnvelope(ref, name, res, secrets.ApplyRestartSkipped)
	}
	if rerr := restartAppInfoLocked(ref.AppInfo); rerr != nil {
		rerr = errors.Join(rerr, recordApply(ref.ID(), secrets.ApplyRestartFailed))
		return phelixerr.Wrapf(phelixerr.CodeOf(rerr), rerr,
			"the new value of %s is stored, but the restart failed\nHint: retry with `phelix restart %s` or `phelix secrets apply`",
			name, ref.Name())
	}
	if err := recordApply(ref.ID(), secrets.ApplyRestartSucceeded); err != nil {
		return err
	}
	return writeSetEnvelope(ref, name, res, secrets.ApplyRestartSucceeded)
}

func writeSetEnvelope(ref *appInfoRef, name string, res secrets.SetResult, restart string) error {
	if !secretsJSON {
		return nil
	}
	result := struct {
		App         string `json:"app"`
		Name        string `json:"name"`
		Version     int    `json:"version"`
		Fingerprint string `json:"fingerprint"`
		Created     bool   `json:"created"`
		Restart     string `json:"restart"`
	}{ref.Name(), name, res.Version, res.Fingerprint, res.Created, restart}
	return writeEnvelopeResult(machine.Success("", result))
}

func recordApply(appID, restart string) error {
	return secrets.RecordApply(appID, &secrets.ApplyStatus{Restart: restart})
}

// runSecretsHistory prints the retained versions of one secret.
func runSecretsHistory(ref *appInfoRef, name string) error {
	st, err := secrets.Open(ref.ID())
	if err != nil {
		return err
	}
	versions, err := st.History(name)
	if err != nil {
		return err
	}
	fmt.Printf("Versions of '%s' for '%s':\n", name, ref.Name())
	for _, v := range versions {
		fmt.Printf("  v%-4d %s  created %s\n", v.Version, v.Fingerprint, v.CreatedAt.Format(time.RFC3339))
	}
	if secretsJSON {
		type versionJSON struct {
			Version     int    `json:"version"`
			Fingerprint string `json:"fingerprint"`
			CreatedAt   string `json:"created_at"`
		}
		result := struct {
			App      string        `json:"app"`
			Name     string        `json:"name"`
			Versions []versionJSON `json:"versions"`
		}{ref.Name(), name, []versionJSON{}}
		for _, v := range versions {
			result.Versions = append(result.Versions, versionJSON{Version: v.Version, Fingerprint: v.Fingerprint, CreatedAt: v.CreatedAt.Format(time.RFC3339)})
		}
		return writeEnvelopeResult(machine.Success("", result))
	}
	return nil
}

// runSecretsRemove deletes a secret; the store refuses while any binding
// still references it.
func runSecretsRemove(ref *appInfoRef, name string) error {
	release, err := deploy.AcquireDeployLock(ref.Name(), "secrets-remove")
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeDeployLocked, "could not acquire deploy lock", err)
	}
	defer release()
	cfg, err := loadProjectConfigFrom(ref.Directory())
	if err != nil {
		return err
	}
	if cfg != nil && cfg.Secrets != nil {
		for _, imp := range cfg.Secrets.Imports {
			if imp.Secret == name {
				return phelixerr.Newf(phelixerr.CodeInvalidArgument, "secrets: %s is imported by phelix.yaml; remove that import first", name)
			}
		}
	}
	if err := secrets.Remove(ref.ID(), name); err != nil {
		return err
	}
	fmt.Printf("✓ Removed '%s' and all retained versions\n", name)
	phelixgrpc.ReportEvent(ref.ID(), ref.Name(), "secret_change", true, "", 0, "", "")
	if secretsJSON {
		result := struct {
			App     string `json:"app"`
			Name    string `json:"name"`
			Removed bool   `json:"removed"`
		}{ref.Name(), name, true}
		return writeEnvelopeResult(machine.Success("", result))
	}
	return nil
}

// runSecretsApply makes the imports in phelix.yaml the active binding set.
// It first settles any pending transaction through the deployment journal,
// then commits the declared imports, then restarts the running application
// so the new bindings take effect immediately.
func runSecretsApply(ref *appInfoRef) error {
	release, err := deploy.AcquireDeployLock(ref.Name(), "secrets-apply")
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeDeployLocked, "could not acquire deploy lock", err)
	}
	defer release()

	outcome, err := deploy.ReconcileSecretTxn(ref.Name(), ref.ID())
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err,
			"cannot settle the pending secret transaction for %s; nothing was changed", ref.Name())
	}
	switch outcome {
	case deploy.SecretTxnCommitted:
		fmt.Printf("  settled a pending secret transaction from the deployment journal (committed)\n")
	case deploy.SecretTxnDiscarded:
		fmt.Printf("  settled a pending secret transaction from the deployment journal (discarded)\n")
	case deploy.SecretTxnRetained:
		return phelixerr.Newf(phelixerr.CodeFilesystem, "the pending secret transaction for %s could not be settled; nothing was changed", ref.Name())
	}

	cfg, err := project.Load(ref.Directory())
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "cannot read %s from %s", project.FileName, ref.Directory())
	}
	var bindings []secrets.Binding
	if cfg.Secrets != nil {
		bindings = cfg.Secrets.Imports
	}
	if err := secrets.ReplaceBindings(ref.ID(), bindings); err != nil {
		return err
	}
	fmt.Printf("✓ Active bindings now come from phelix.yaml (%d imports)\n", len(bindings))
	phelixgrpc.ReportEvent(ref.ID(), ref.Name(), "secret_change", true, "", 0, "", "")

	if secretsNoRestart || !appIsRunning(ref) {
		if err := recordApply(ref.ID(), secrets.ApplyRestartSkipped); err != nil {
			return err
		}
		return writeApplyEnvelope(ref, len(bindings), secrets.ApplyRestartSkipped)
	}
	if rerr := restartAppInfoLocked(ref.AppInfo); rerr != nil {
		rerr = errors.Join(rerr, recordApply(ref.ID(), secrets.ApplyRestartFailed))
		return phelixerr.Wrapf(phelixerr.CodeOf(rerr), rerr,
			"the bindings were committed, but the restart failed\nHint: retry with `phelix restart %s`",
			ref.Name())
	}
	if err := recordApply(ref.ID(), secrets.ApplyRestartSucceeded); err != nil {
		return err
	}
	return writeApplyEnvelope(ref, len(bindings), secrets.ApplyRestartSucceeded)
}

func writeApplyEnvelope(ref *appInfoRef, count int, restart string) error {
	if !secretsJSON {
		return nil
	}
	result := struct {
		App     string `json:"app"`
		Imports int    `json:"imports"`
		Restart string `json:"restart"`
	}{ref.Name(), count, restart}
	return writeEnvelopeResult(machine.Success("", result))
}

// appIsRunning reports whether the application currently has instances to
// restart.
func appIsRunning(ref *appInfoRef) bool {
	state, err := deploy.Load(ref.Name())
	if err == nil && state.Mode != "classic" {
		for _, inst := range state.Slots {
			if inst != nil && deploy.InstanceAlive(inst) {
				return true
			}
		}
		for _, inst := range state.Replicas {
			if inst != nil && deploy.InstanceAlive(inst) {
				return true
			}
		}
		return false
	}
	return ref.AppInfo.Status == "running"
}

// secretIsBound reports whether the active bindings reference the secret.
func secretIsBound(appID, name string) (bool, error) {
	bindings, err := secrets.CommittedBindings(appID)
	if err != nil {
		return false, err
	}
	for _, b := range bindings {
		if b.Secret == name {
			return true, nil
		}
	}
	return false, nil
}

// missingImportsFor returns the phelix.yaml imports with no value in the
// store. Best effort: an unreadable file simply reports nothing missing.
func missingImportsFor(ref *appInfoRef, st *secrets.Store) []project.SecretImport {
	cfg, err := project.Load(ref.Directory())
	if err != nil || cfg.Secrets == nil {
		return nil
	}
	var missing []project.SecretImport
	for _, imp := range cfg.Secrets.Imports {
		if _, err := st.Peek(imp.Secret); err != nil {
			missing = append(missing, imp)
		}
	}
	return missing
}

func sortSecretMetas(metas []secrets.SecretMeta) {
	for i := 1; i < len(metas); i++ {
		for j := i; j > 0 && metas[j].Name < metas[j-1].Name; j-- {
			metas[j], metas[j-1] = metas[j-1], metas[j]
		}
	}
}

// readSecretValue reads the value the way the command contract promises:
// from a file byte for byte, or from stdin with exactly one trailing newline
// removed. A value is never taken from an argument, because arguments are
// visible to every process on the host.
func readSecretValue(fromFile string) (string, error) {
	var reader io.Reader = os.Stdin
	if fromFile != "" {
		f, err := os.Open(fromFile)
		if err != nil {
			return "", phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "secrets: cannot read %s", fromFile)
		}
		defer f.Close()
		reader = f
	} else if IsInteractive() {
		return "", phelixerr.New(phelixerr.CodeInvalidArgument,
			"read a secret from stdin or --from-file\nHint: phelix secrets set NAME < value.txt")
	}
	data, err := io.ReadAll(io.LimitReader(reader, secrets.MaxValueBytes+3))
	if err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeFilesystem, "secrets: cannot read value", err)
	}
	value := string(data)
	if fromFile == "" {
		if before, ok := strings.CutSuffix(value, "\r\n"); ok {
			value = before
		} else {
			value = strings.TrimSuffix(value, "\n")
		}
	}
	if err := secrets.ValidateSecretValue(value); err != nil {
		return "", err
	}
	return value, nil
}

// beginProjectSecrets prepares imports before a deployment changes any instance.
func beginProjectSecrets(info *app.AppInfo, cfg *project.Config, deploymentID string) (*deploy.SecretTxn, error) {
	bindings, err := secrets.CommittedBindings(info.ID)
	if err != nil {
		return nil, err
	}
	if (cfg == nil || cfg.Secrets == nil) && len(bindings) == 0 {
		return nil, nil
	}
	var imports []secrets.Binding
	if cfg != nil && cfg.Secrets != nil {
		imports = cfg.Secrets.Imports
	}
	return deploy.BeginSecretTxn(info.ID, info.Name, deploymentID, imports)
}

// startClassicWithSecrets passes deployment values to the existing process manager.
func startClassicWithSecrets(id string, port int, name string, txn *deploy.SecretTxn) error {
	if txn == nil {
		return app.Manager.StartApplication(id, port, name)
	}
	manager, ok := app.Manager.(*app.AppManager)
	if !ok {
		return phelixerr.New(phelixerr.CodeConfiguration, "process manager does not support secret deployments")
	}
	return manager.StartApplicationWithSecrets(id, port, name, txn.Set)
}

// acceptClassicSecrets records a successful classic start in the deployment journal.
func acceptClassicSecrets(info *app.AppInfo, port, version int, txn *deploy.SecretTxn) error {
	if txn == nil {
		return nil
	}
	state := &deploy.DeployState{AppName: info.Name, AppID: info.ID, Mode: "classic", PublicPort: port, ActiveVersion: version}
	txn.Stamp(state)
	return deploy.Store(state)
}
