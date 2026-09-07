package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/proxy"
	"github.com/fatih/color"
	"github.com/olekukonko/tablewriter"
	"github.com/spf13/cobra"
)

var rollbackTo string
var rollbackList bool
var rollbackDryRun bool
var rollbackReason string
var rollbackVerify string

var RollbackCmd = &cobra.Command{
	Use:           "rollback [AppName]",
	Short:         "Roll back to a previous versioned build",
	Args:          cobra.MaximumNArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Ensure all queued rollback events are flushed to the backend
		// before the CLI process exits. 5s is generous enough for a
		// single gRPC round-trip; if it times out we log and move on.
		defer phelixgrpc.StopRollbackSender(5 * time.Second)
		defer phelixgrpc.StopDeploymentSender(5 * time.Second)

		if len(args) == 0 {
			if !IsInteractive() {
				return phelixerr.Newf(phelixerr.CodeInvalidArgument, "missing required <AppName>; usage: phelix rollback <AppName> [--to VERSION] [--list]")
			}
			chosen, err := PromptApp(false, "Select application to roll back")
			if err != nil {
				return err
			}
			args = []string{chosen}
		}

		name := args[0]

		// Reason: validate once, up front, so both the interactive and
		// explicit paths share the same rules. Explicit-but-empty is an error;
		// absent is fine. The validated text is what gets persisted.
		reasonExplicit := cmd.Flags().Changed("reason")
		reason, err := deploy.ValidateRollbackReason(rollbackReason, reasonExplicit)
		if err != nil {
			return err
		}

		// Verify: parse strictly as a Go duration ("30s", "1m", "2m30s").
		// Bare numbers ("30") are rejected — Phelix has been bitten before by
		// unitless values silently meaning something else. The flag stays a
		// real time.Duration end to end.
		verifyExplicit := cmd.Flags().Changed("verify")
		var verifyDuration time.Duration
		if verifyExplicit {
			d, parseErr := time.ParseDuration(rollbackVerify)
			if parseErr != nil {
				return phelixerr.Newf(phelixerr.CodeInvalidArgument,
					"invalid --verify duration %q: use a Go duration like 30s, 1m or 2m30s", rollbackVerify)
			}
			if d <= 0 {
				return phelixerr.Newf(phelixerr.CodeInvalidArgument,
					"invalid --verify duration %q: must be positive", rollbackVerify)
			}
			verifyDuration = d
		}

		fromPicker := false
		if IsInteractive() && rollbackTo == "" && !rollbackList && !rollbackDryRun {
			target, picked, err := promptRollbackVersion(name)
			if err != nil {
				if errors.Is(err, errRollbackCancelled) {
					fmt.Println("Rollback cancelled.")
					return nil
				}
				return err
			}
			if !picked {
				// Nothing to choose from — message already printed.
				return nil
			}
			rollbackTo = fmt.Sprintf("%d", target)
			fromPicker = true
		}

		if err := app.Manager.LoadState(); err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load state", err)
		}

		appInfo, err := GetAppInfo(name)
		if err != nil {
			return err
		}
		name = appInfo.Name

		// Interactive picker path with no --reason: offer an optional reason
		// after the target is chosen. An explicitly supplied --reason never
		// re-prompts; Esc/empty keeps the rollback reason-free.
		if fromPicker && !reasonExplicit && IsInteractive() {
			entered, promptErr := PromptString("Reason (optional, Enter to skip):", "")
			if promptErr != nil {
				// Survey reports Esc/Ctrl-C as a prompt error — cancellation,
				// not a reason-free rollback.
				fmt.Println("Rollback cancelled.")
				return nil
			}
			reason, err = deploy.ValidateRollbackReason(entered, entered != "")
			if err != nil {
				return err
			}
		}

		policy := deploy.DefaultRetention{Max: 5}

		if rollbackList {
			return listRollbackVersions(name, policy, appInfo)
		}

		// Resolve target version.
		target := 0
		if rollbackTo != "" {
			target, err = deploy.ResolveVersionOrTag(name, rollbackTo)
			if err != nil {
				return phelixerr.Wrap(phelixerr.CodeRollbackTargetNotFound, "could not resolve rollback target", err)
			}
		} else {
			target, err = deploy.PreviousVersion(name)
			if err != nil {
				return phelixerr.Wrap(phelixerr.CodeRollbackTargetNotFound, "could not resolve previous version", err)
			}
		}

		// Same guard ExecuteRollback and PlanRollback apply: rolling back to
		// the version already serving is a no-op request, and executing it
		// would restart the instance while claiming a version change that
		// never happened (history would record vN -> vN).
		if cur, _ := deploy.CurrentVersion(name); cur > 0 && target == cur {
			return phelixerr.Newf(phelixerr.CodeRollbackTargetNotFound,
				"already running v%d; nothing to roll back to", target)
		}

		if rollbackDryRun {
			return renderRollbackPreview(appInfo, name, target, reason, verifyDuration)
		}

		// Interactive picker path: show the rollback preview and require an
		// explicit confirmation before anything executes. Explicit --to and
		// non-interactive invocations keep their immediate behavior.
		if fromPicker {
			if err := renderRollbackPreview(appInfo, name, target, reason, verifyDuration); err != nil {
				return err
			}
			ok, err := PromptConfirm(fmt.Sprintf("Proceed with rollback to v%d?", target), false)
			if err != nil {
				fmt.Println("Rollback cancelled.")
				return nil
			}
			if !ok {
				fmt.Println("Rollback cancelled.")
				return nil
			}
		}

		// Check whether a deploy state exists (blue-green / rolling).
		state, deployErr := deploy.Load(name)
		if deployErr != nil || state == nil || state.Mode == "" {
			return rollbackClassic(appInfo, name, target, reason, verifyDuration)
		}

		// --- Zero-downtime rollback path (blue-green / rolling) ---
		return rollbackZeroDowntime(appInfo, name, target, state, reason, verifyDuration)
	},
}

func init() {
	RollbackCmd.Flags().StringVar(&rollbackTo, "to", "", "Roll back to a specific version (e.g. v3, 3, or a tag name)")
	RollbackCmd.Flags().BoolVar(&rollbackList, "list", false, "List retained versions with metadata")
	RollbackCmd.Flags().BoolVar(&rollbackDryRun, "dry-run", false, "Preview the rollback without making any changes")
	RollbackCmd.Flags().StringVar(&rollbackReason, "reason", "",
		"Record why the rollback was performed (stored in rollback history)")
	RollbackCmd.Flags().StringVar(&rollbackVerify, "verify", "",
		"Observe rollback stability for a duration (e.g. 30s, 1m, 2m30s) after the rollback completes")
}

// errRollbackCancelled marks a user cancellation (Esc / Ctrl-C in the picker)
// so the command exits cleanly instead of falling back to an automatic
// previous-version rollback.
var errRollbackCancelled = phelixerr.New(phelixerr.CodeInvalidArgument, "rollback cancelled")

// promptRollbackVersion presents an interactive list of valid rollback
// candidates — newest → oldest, excluding the current version and versions
// whose binary no longer exists on disk — and returns the chosen version
// number. picked is false when the user cancelled or no candidates exist.
// With a single candidate the picker is still shown so the interactive flow
// stays consistent (details + confirmation always gate the rollback).
func promptRollbackVersion(appName string) (target int, picked bool, err error) {
	candidates, cur, err := rollbackCandidates(appName)
	if err != nil {
		return 0, false, err
	}
	if len(candidates) == 0 {
		fmt.Printf("Rollback %s\n\nNo previous versions are available for rollback.\n", color.CyanString("'%s'", appName))
		if cur > 0 {
			fmt.Printf("\nCurrent version: v%d\n", cur)
		}
		return 0, false, nil
	}

	fmt.Printf("Rollback %s\n\n", color.CyanString("'%s'", appName))
	if cur > 0 {
		fmt.Printf("Current: v%d\n\n", cur)
	}

	opts := make([]string, len(candidates))
	for i, v := range candidates {
		tag := v.Tag
		if tag == "" {
			tag = "—"
		}
		// %-20.20s keeps long tags from destroying the layout.
		opts[i] = fmt.Sprintf("v%-4d %-20.20s %s", v.Version, tag, relTime(v.BuiltAt))
	}

	// Details footer follows the cursor: rendered from stored versions.json
	// metadata only, so it updates instantly on every arrow key press.
	byIndex := make(map[int]deploy.VersionMeta, len(candidates))
	for i, v := range candidates {
		byIndex[i] = v
	}
	chosen, err := askSelectWithDetails("Select version to rollback to:", opts, func(value string, index int) string {
		if v, ok := byIndex[index]; ok {
			return pickerVersionDetails(v)
		}
		return ""
	})
	if err != nil {
		// Survey reports Esc / Ctrl-C as a prompt error; treat it as a
		// cancellation, never as a signal to roll back automatically.
		return 0, false, errRollbackCancelled
	}
	for i, o := range opts {
		if o == chosen {
			return candidates[i].Version, true, nil
		}
	}
	return 0, false, errRollbackCancelled
}

// rollbackCandidates returns rollback targets newest-first, excluding the
// current version and versions whose binary artifact is missing (stale
// versions.json entries cannot actually be rolled back to). Env snapshots are
// NOT required here — a missing snapshot is a warning in the rollback plan,
// matching the real rollback validation model.
func rollbackCandidates(appName string) ([]deploy.VersionMeta, int, error) {
	vers, err := deploy.ListVersionsForDisplay(appName, deploy.DefaultRetention{Max: 5})
	if err != nil {
		return nil, 0, err
	}
	cur, _ := deploy.CurrentVersion(appName)

	out := make([]deploy.VersionMeta, 0, len(vers))
	for _, v := range vers {
		if v.IsCurrent || v.Version == cur {
			continue
		}
		// Same artifact check the real rollback performs via
		// ExistingVersionSource → VersionPaths.
		if _, _, err := deploy.VersionPaths(appName, v.Version); err != nil {
			continue
		}
		out = append(out, v)
	}
	return out, cur, nil
}

// relTime renders a coarse human-readable age ("2 min ago", "3 days ago").
func relTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	n := int(d.Hours() / 24)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case n < 1:
		return fmt.Sprintf("%d hour%s ago", int(d.Hours()), plural(int(d.Hours())))
	case n < 7:
		return fmt.Sprintf("%d day%s ago", n, plural(n))
	default:
		return t.Format("Jan 2, 2006")
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// rollbackZeroDowntime handles rollback through the blue-green or rolling
// deploy path with full lifecycle event reporting.
func rollbackZeroDowntime(appInfo *app.AppInfo, appName string, target int, state *deploy.DeployState, reason string, verifyDuration time.Duration) error {
	totalStart := time.Now()
	strategy := string(state.Mode)

	// Resolve current version for the reporter.
	currentVer, _ := deploy.CurrentVersion(appName)
	currentVerStr := fmt.Sprintf("v%d", currentVer)
	targetVerStr := fmt.Sprintf("v%d", target)

	r := phelixgrpc.NewRollbackReporter("", appInfo.ID, appInfo.ID, appName, strategy, strategy, currentVerStr, targetVerStr, rollbackTo)

	// Populate metadata the backend needs for context.
	r.SetMetadata("public_port", fmt.Sprintf("%d", state.PublicPort))
	r.SetMetadata("app_directory", appInfo.Directory)
	r.SetMetadata("app_port", fmt.Sprintf("%d", appInfo.Port))
	r.SetMetadata("app_status", appInfo.Status)
	r.SetMetadata("app_pid", fmt.Sprintf("%d", appInfo.PID))
	r.SetMetadata("app_language", appInfo.Language)
	r.SetMetadata("active_slot", state.ActiveSlot)
	r.SetMetadata("replicas_count", fmt.Sprintf("%d", len(state.Replicas)))
	r.SetMetadata("grace_seconds", fmt.Sprintf("%d", state.GraceSeconds))
	if reason != "" {
		r.SetMetadata("rollback_reason", reason)
	}
	if verifyDuration > 0 {
		r.SetMetadata("verify_duration", verifyDuration.String())
	}
	if state.LastRollback != nil {
		r.SetMetadata("last_rollback_from", fmt.Sprintf("v%d", state.LastRollback.FromVersion))
		r.SetMetadata("last_rollback_to", fmt.Sprintf("v%d", state.LastRollback.ToVersion))
		r.SetMetadata("last_rollback_at", state.LastRollback.At.Format(time.RFC3339))
	}

	// Step: init
	r.Emit(phelixgrpc.RollbackStepInit, true, fmt.Sprintf("rollback %s to %s via %s", appName, targetVerStr, strategy), time.Since(totalStart), "")

	// Step: version_resolving
	stepStart := time.Now()
	// Version is already resolved by the caller, just emit the resolution.
	r.Emit(phelixgrpc.RollbackStepVersionResolved, true, fmt.Sprintf("target resolved to %s", targetVerStr), time.Since(stepStart), "")

	// Step: proxy_ensuring
	stepStart = time.Now()
	socket, err := proxy.DefaultSocketPath()
	if err != nil {
		r.Emit(phelixgrpc.RollbackStepProxyEnsuring, false, "failed to determine proxy socket path", time.Since(totalStart), err.Error())
		return phelixerr.Wrap(phelixerr.CodeProxy, "could not determine proxy socket path", err)
	}
	fmt.Printf("  %s Ensuring proxy daemon is running...\n", color.BlueString("→"))
	if err := proxy.EnsureDaemon(context.Background(), "", 5*time.Second); err != nil {
		r.Emit(phelixgrpc.RollbackStepProxyEnsuring, false, "failed to start proxy daemon", time.Since(totalStart), err.Error())
		return phelixerr.Wrapf(
			phelixerr.CodeProxy,
			err,
			"could not start proxy daemon\n  Start it manually with: %s",
			color.CyanString("phelix proxy"),
		)
	}
	r.Emit(phelixgrpc.RollbackStepProxyEnsuring, true, "proxy daemon running", time.Since(stepStart), "")

	// Step: proxy_ready
	stepStart = time.Now()
	proxyClient := proxy.NewClient(socket)
	if err := proxyClient.Ping(context.Background()); err != nil {
		r.Emit(phelixgrpc.RollbackStepProxyReady, false, "proxy not responding", time.Since(totalStart), err.Error())
		return phelixerr.Wrapf(
			phelixerr.CodeConnection,
			err,
			"proxy daemon did not respond\n  Start it first with: %s",
			color.CyanString("phelix proxy"),
		)
	}
	r.Emit(phelixgrpc.RollbackStepProxyReady, true, "proxy ping successful", time.Since(stepStart), "")

	publicPort := state.PublicPort
	fmt.Printf("%s Rolling back %s to v%d (zero-downtime via %s)\n",
		color.BlueString("→"), color.CyanString("'%s'", appName), target,
		color.MagentaString(string(state.Mode)))

	// Execute the zero-downtime rollback.
	// Note: ExecuteRollback internally handles lock acquisition, state loading,
	// health checks, proxy switching, and graceful stop. The CLI emits
	// pre/post events around the call; the deploy package emits its own
	// internal log lines via the Logger.
	stepStart = time.Now()
	// Deployment telemetry runs alongside the rollback reporter: the rollback
	// events describe the CLI's own steps, the deployment events describe the
	// resulting deployment transition (slots/replicas/proxy/versions) in the
	// same shape a forward deploy reports.
	tracker := deploy.NewTracker(phelixgrpc.NewDeploymentSink(), appInfo.ID, appName, strategy)
	// Signal cancellation is wired only when verification is requested: the
	// observation window is interruptible (Ctrl+C cancels verification, the
	// completed rollback stays active) while rollbacks without --verify keep
	// their existing signal behavior untouched.
	ctx := context.Background()
	var restoreSignals func()
	if verifyDuration > 0 {
		ctx, restoreSignals = verifyContext()
		defer restoreSignals()
	}
	verifyReq := (*deploy.VerifyRequest)(nil)
	if verifyDuration > 0 {
		verifyReq = &deploy.VerifyRequest{
			Duration: verifyDuration,
			OnTick:   verifyProgressPrinter(),
		}
	}
	err = deploy.ExecuteRollback(ctx, deploy.RollbackOptions{
		AppName:        appName,
		AppID:          appInfo.ID,
		PublicPort:     publicPort,
		TargetVersion:  target,
		Launcher:       deploy.DefaultLauncher,
		ProxyClient:    proxyClient,
		HealthProvider: deploy.DefaultHealthProvider(),
		Logger:         &colorLogger{},
		Telemetry:      tracker,
		Reason:         reason,
		Verify:         verifyReq,
	})
	rollbackDuration := time.Since(stepStart)
	r.SetMetadata("rollback_duration_ms", fmt.Sprintf("%d", rollbackDuration.Milliseconds()))
	if err != nil {
		// A verification failure/cancellation is NOT an execution failure: the
		// rollback committed and the target is serving. Return the typed
		// ROLLBACK_VERIFY_FAILED error untouched so exit code 23 and history
		// keep execution SUCCESS distinct from execution FAILURE.
		if phelixerr.IsCode(err, phelixerr.CodeRollbackVerifyFailed) {
			return err
		}
		r.Emit(phelixgrpc.RollbackStepFailed, false, "zero-downtime rollback failed", time.Since(totalStart), err.Error())
		return phelixerr.Wrap(phelixerr.CodeRollbackFailed, "zero-downtime rollback failed", err)
	}

	// The rollback swapped slots; bring the lifecycle record in line with the
	// new active instance.
	reconcileAppWithDeploy(appName)

	// Step: complete
	fmt.Printf("%s Rollback of %s to v%d complete\n", color.GreenString("✓"), color.CyanString("'%s'", appName), target)
	r.Emit(phelixgrpc.RollbackStepComplete, true, fmt.Sprintf("zero-downtime rollback %s -> %s complete", currentVerStr, targetVerStr), time.Since(totalStart), "")
	return nil
}

// rollbackClassic handles rollback for apps built with the classic
// build/rebuild path (no --blue-green / --replicas). There is no proxy or
// zero-downtime guarantee — it simply stops the current instance, copies the
// versioned binary to the expected location, and starts it.
//
// Ordering guarantee: the version is only promoted (is_current set to true,
// current symlink updated) after the start succeeds. If the start fails, the
// version exists on disk but the active instance is untouched.
func rollbackClassic(appInfo *app.AppInfo, appName string, target int, reason string, verifyDuration time.Duration) (err error) {
	totalStart := time.Now()
	strategy := "classic"

	// Resolve current version for the reporter.
	currentVer, _ := deploy.CurrentVersion(appName)
	currentVerStr := fmt.Sprintf("v%d", currentVer)
	targetVerStr := fmt.Sprintf("v%d", target)

	// History records the terminal outcome of the classic rollback: only
	// full completion (start + promotion done) is success; any error path
	// below lands here as FAILED. runErr deliberately tracks ONLY the
	// execution phase: verification (when requested) runs after the rollback
	// is committed and its outcome goes into the verification block — a
	// failed or cancelled window must never rewrite the execution status.
	var verification *deploy.RollbackVerification
	var runErr error
	// fail records an EXECUTION failure for the history deferred-write and
	// returns the error unchanged. Verification outcomes deliberately bypass
	// this helper so they never contaminate the execution status.
	fail := func(e error) error {
		runErr = e
		return e
	}
	defer func() {
		deploy.RecordRollbackResult(appName, currentVer, target, strategy, reason, verification, runErr)
	}()

	r := phelixgrpc.NewRollbackReporter("", appInfo.ID, appInfo.ID, appName, strategy, strategy, currentVerStr, targetVerStr, rollbackTo)

	// Populate metadata the backend needs for context.
	r.SetMetadata("app_directory", appInfo.Directory)
	r.SetMetadata("app_port", fmt.Sprintf("%d", appInfo.Port))
	r.SetMetadata("app_status", appInfo.Status)
	r.SetMetadata("app_pid", fmt.Sprintf("%d", appInfo.PID))
	r.SetMetadata("app_language", appInfo.Language)

	// Step: init
	r.Emit(phelixgrpc.RollbackStepInit, true, fmt.Sprintf("rollback %s to %s via classic", appName, targetVerStr), time.Since(totalStart), "")

	// Step: version_resolving
	stepStart := time.Now()
	binPath, _, err := deploy.VersionPaths(appName, target)
	if err != nil {
		r.Emit(phelixgrpc.RollbackStepVersionResolved, false, "failed to resolve version paths", time.Since(totalStart), err.Error())
		return fail(phelixerr.Wrap(phelixerr.CodeRollbackTargetNotFound, "failed to resolve version paths", err))
	}
	r.SetMetadata("binary_path", binPath)
	r.Emit(phelixgrpc.RollbackStepVersionResolved, true, fmt.Sprintf("binary path: %s", binPath), time.Since(stepStart), "")

	fmt.Printf("%s Rolling back %s to v%d (classic stop→start)\n",
		color.BlueString("→"), color.CyanString("'%s'", appName), target)

	// Step: stopping_old
	if appInfo.Status == "running" {
		stepStart = time.Now()
		fmt.Printf("  %s Stopping current instance (PID %d)...\n", color.BlueString("→"), appInfo.PID)
		r.SetMetadata("stopped_pid", fmt.Sprintf("%d", appInfo.PID))
		if err := app.Manager.StopApplication(appInfo.ID); err != nil {
			// Non-fatal: the process may have already exited.
			fmt.Printf("  %s Warning: stop returned: %v\n", color.YellowString("⚠"), err)
			r.Emit(phelixgrpc.RollbackStepOldStopped, true, fmt.Sprintf("stop returned warning: %v", err), time.Since(stepStart), "")
		} else {
			r.Emit(phelixgrpc.RollbackStepOldStopped, true, fmt.Sprintf("stopped PID %d", appInfo.PID), time.Since(stepStart), "")
		}
	}

	// Step: copying_binary
	stepStart = time.Now()
	destBin := filepath.Join(appInfo.Directory, fmt.Sprintf("app_%s", appInfo.ID))
	fmt.Printf("  %s Copying v%d binary to %s...\n", color.BlueString("→"), target, destBin)
	r.SetMetadata("dest_binary", destBin)
	if err := copyFileForRollback(binPath, destBin); err != nil {
		r.Emit(phelixgrpc.RollbackStepBinaryCopied, false, "failed to copy versioned binary", time.Since(totalStart), err.Error())
		return fail(phelixerr.Wrap(phelixerr.CodeRollbackFailed, "failed to copy versioned binary", err))
	}
	r.Emit(phelixgrpc.RollbackStepBinaryCopied, true, fmt.Sprintf("binary copied to %s", destBin), time.Since(stepStart), "")

	// Determine the port.
	port := appInfo.Port
	if port == 0 {
		port = defaultPort
	}
	r.SetMetadata("port", fmt.Sprintf("%d", port))

	// Step: starting_new
	stepStart = time.Now()
	fmt.Printf("  %s Starting rolled-back binary on port %d...\n", color.BlueString("→"), port)
	if err := app.Manager.StartApplication(appInfo.ID, port, appName); err != nil {
		r.Emit(phelixgrpc.RollbackStepFailed, false, "failed to start rolled-back application", time.Since(totalStart), err.Error())
		return fail(phelixerr.Wrapf(
			phelixerr.CodeRollbackFailed,
			err,
			"failed to start rolled-back application %q (ID: %s)",
			appName,
			appInfo.ID,
		))
	}
	r.Emit(phelixgrpc.RollbackStepNewStarted, true, fmt.Sprintf("started on port %d", port), time.Since(stepStart), "")

	// Step: promoting_version
	stepStart = time.Now()
	if err := deploy.PromoteVersion(appName, target, "classic"); err != nil {
		fmt.Printf("  %s Warning: could not promote version: %v\n", color.YellowString("⚠"), err)
		r.Emit(phelixgrpc.RollbackStepVersionPromoted, false, "version promotion warning", time.Since(stepStart), err.Error())
	} else {
		r.Emit(phelixgrpc.RollbackStepVersionPromoted, true, fmt.Sprintf("version %s promoted", targetVerStr), time.Since(stepStart), "")
	}

	// Step: complete
	fmt.Printf("%s Rollback of %s to v%d complete\n", color.GreenString("✓"), color.CyanString("'%s'", appName), target)
	r.Emit(phelixgrpc.RollbackStepComplete, true, fmt.Sprintf("classic rollback %s -> %s complete", currentVerStr, targetVerStr), time.Since(totalStart), "")

	// Stability verification runs only after the rollback is fully committed
	// (start + promotion), so it observes the version actually serving.
	// Verification failure/cancellation is returned as-is (exit 23,
	// ROLLBACK_VERIFY_FAILED) but never flows into runErr — history keeps the
	// execution SUCCESS with the verification block telling the rest.
	if verifyDuration > 0 {
		var verr error
		verification, verr = runStabilityVerification(appInfo, verifyDuration)
		if verr != nil {
			return verr
		}
	}
	return nil
}

// copyFileForRollback copies src to dst, creating parent directories as needed.
func copyFileForRollback(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()

	buf := make([]byte, 32*1024)
	for {
		n, readErr := in.Read(buf)
		if n > 0 {
			if _, writeErr := out.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
		}
		if readErr != nil {
			break
		}
	}
	return out.Close()
}

// renderRollbackPreview builds the read-only rollback plan and prints it.
// It performs no mutations: no process, proxy, versions.json, deploy.json,
// symlink or audit-log changes — the plan builder only reads state, and the
// gRPC rollback reporters are deliberately not constructed here so no
// rollback telemetry events are emitted for a preview. reason and
// verifyDuration are display-only: shown in the plan, never persisted or
// acted upon.
func renderRollbackPreview(appInfo *app.AppInfo, appName string, target int, reason string, verifyDuration time.Duration) error {
	// Classic-path context mirrors what rollbackClassic would use.
	port := appInfo.Port
	if port == 0 {
		port = defaultPort
	}
	destBin := filepath.Join(appInfo.Directory, fmt.Sprintf("app_%s", appInfo.ID))

	plan, err := deploy.PlanRollback(appName, deploy.RollbackPlanInput{
		AppID:          appInfo.ID,
		Target:         target,
		ClassicRunning: appInfo.Status == "running",
		ClassicPort:    port,
		ClassicDestBin: destBin,
	})
	if err != nil {
		return err
	}

	fmt.Printf("%s Rollback Preview\n\n", color.BlueString("→"))

	fmt.Printf("  %-14s %s\n", "Application", appName)
	fmt.Printf("  %-14s %s\n", "Current", versionLabel(plan.CurrentVersion, plan.CurrentTag))
	fmt.Printf("  %-14s %s\n", "Target", versionLabel(plan.TargetVersion, plan.TargetTag))
	if !plan.TargetBuiltAt.IsZero() {
		fmt.Printf("  %-14s %s\n", "Built", plan.TargetBuiltAt.Format("2006-01-02 15:04:05"))
	}
	if plan.TargetCommit != "" {
		commit := plan.TargetCommit
		if len(commit) > 12 {
			commit = commit[:12]
		}
		fmt.Printf("  %-14s %s\n", "Commit", commit)
	}
	fmt.Printf("  %-14s %s\n", "Deploy Mode", plan.Strategy)
	fmt.Printf("  %-14s %s\n", "Health Check", plan.HealthCheck)
	fmt.Printf("  %-14s %s\n", "Environment", plan.EnvSummary)
	if reason != "" {
		fmt.Printf("  %-14s %s\n", "Reason", reason)
	}
	if verifyDuration > 0 {
		fmt.Printf("  %-14s %s\n", "Verification", fmt.Sprintf("%s of stability after the rollback completes", verifyDuration))
	}

	if len(plan.Warnings) > 0 {
		fmt.Printf("\nWarnings:\n")
		for _, w := range plan.Warnings {
			fmt.Printf("  %s %s\n", color.YellowString("!"), w)
		}
	}

	// Changes: only rows where both sides are known.
	fmt.Printf("\nChanges:\n")
	if plan.CurrentVersion > 0 {
		fmt.Printf("  %-14s v%d → v%d\n", "Version", plan.CurrentVersion, plan.TargetVersion)
		if plan.CurrentSize > 0 && plan.TargetSize > 0 {
			fmt.Printf("  %-14s %.1f MB → %.1f MB\n", "Binary",
				float64(plan.CurrentSize)/(1024*1024), float64(plan.TargetSize)/(1024*1024))
		}
		if plan.CurrentCommit != "" && plan.TargetCommit != "" && plan.CurrentCommit != plan.TargetCommit {
			cur, tgt := plan.CurrentCommit, plan.TargetCommit
			if len(cur) > 12 {
				cur = cur[:12]
			}
			if len(tgt) > 12 {
				tgt = tgt[:12]
			}
			fmt.Printf("  %-14s %s → %s\n", "Commit", cur, tgt)
		}
		if plan.Strategy != "classic" {
			fmt.Printf("  %-14s %s\n", "Environment", plan.EnvSummary)
		}
	}

	if plan.Strategy == string(deploy.ModeBlueGreen) {
		fmt.Printf("\nTraffic:\n")
		fmt.Printf("  %-14s :%d\n", "Public", plan.PublicPort)
		fmt.Printf("  %-14s %s\n", "Current", slotOrNone(plan.CurrentSlot))
		fmt.Printf("  %-14s %s\n", "Target", plan.TargetSlot)
		if plan.CurrentPort > 0 {
			fmt.Printf("  %-14s :%d → assigned at startup\n", "Backend", plan.CurrentPort)
		}
	}
	if plan.Strategy == string(deploy.ModeRolling) {
		fmt.Printf("\n  %-13s %d\n", "Replicas", plan.Replicas)
	}
	if plan.Downtime {
		fmt.Printf("\nDowntime:\n  %-14s %s\n", "Expected", color.YellowString("yes (stop → start)"))
	}

	fmt.Printf("\nRollback Plan:\n")
	for i, step := range plan.Steps {
		fmt.Printf("  %2d. %s\n", i+1, step)
	}

	fmt.Printf("\n%s No changes will be made.\n", color.GreenString("✓"))
	return nil
}

// versionLabel renders "v12" or "v12 (stable)".
func versionLabel(ver int, tag string) string {
	if tag == "" {
		return fmt.Sprintf("v%d", ver)
	}
	return fmt.Sprintf("v%d (%s)", ver, tag)
}

func slotOrNone(slot string) string {
	if slot == "" {
		return "—"
	}
	return slot
}

func listRollbackVersions(appName string, policy deploy.RetentionPolicy, appInfo *app.AppInfo) error {
	totalStart := time.Now()

	r := phelixgrpc.NewRollbackReporter("", appInfo.ID, appInfo.ID, appName, "list", "list", "", "", rollbackTo)
	r.SetMetadata("app_directory", appInfo.Directory)
	r.SetMetadata("app_status", appInfo.Status)
	r.SetMetadata("app_language", appInfo.Language)

	vers, err := deploy.ListVersionsForDisplay(appName, policy)
	if err != nil {
		r.Emit(phelixgrpc.RollbackStepListVersions, false, "failed to list versions", time.Since(totalStart), err.Error())
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to list versions", err)
	}

	// Step: init
	r.Emit(phelixgrpc.RollbackStepInit, true, "listing retained versions", time.Since(totalStart), "")

	if len(vers) == 0 {
		fmt.Printf("  No versioned builds recorded for %s yet.\n", color.CyanString("'%s'", appName))
		r.Emit(phelixgrpc.RollbackStepListVersions, true, "no versions found", time.Since(totalStart), "")
		return nil
	}

	// Build version list for the event.
	pbVersions := make([]*pb.RollbackVersionEntry, 0, len(vers))
	for _, v := range vers {
		pbVersions = append(pbVersions, &pb.RollbackVersionEntry{
			Version:        int32(v.Version),
			Tag:            v.Tag,
			GitCommit:      v.GitCommit,
			BuildTimestamp: v.BuiltAt.UnixMilli(),
			BinarySize:     v.SizeBytes,
			Current:        v.IsCurrent,
			PruneSoon:      deploy.WouldPruneOnNextBuild(appName, v.Version, policy),
		})
	}
	r.SetVersionList(pbVersions)
	r.SetMetadata("version_count", fmt.Sprintf("%d", len(vers)))

	// Render table.
	table := tablewriter.NewTable(os.Stdout)
	table.Header([]string{"Version", "Tag", "Commit", "Built", "Size", "Current", "Prune soon"})
	for _, v := range vers {
		commit := v.GitCommit
		if commit == "" {
			commit = "—"
		} else if len(commit) > 12 {
			commit = commit[:12]
		}
		size := fmt.Sprintf("%.1f MB", float64(v.SizeBytes)/(1024*1024))
		cur := ""
		if v.IsCurrent {
			cur = color.GreenString("yes")
		}
		tag := v.Tag
		if tag == "" {
			tag = "—"
		}
		prune := ""
		if deploy.WouldPruneOnNextBuild(appName, v.Version, policy) {
			prune = color.YellowString("yes")
		}
		table.Append([]string{
			fmt.Sprintf("v%d", v.Version),
			tag,
			commit,
			v.BuiltAt.Format(time.RFC3339),
			size,
			cur,
			prune,
		})
	}
	table.Render()

	r.Emit(phelixgrpc.RollbackStepListVersions, true, fmt.Sprintf("listed %d versions", len(vers)), time.Since(totalStart), "")
	return nil
}
