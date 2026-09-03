package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

		if IsInteractive() && rollbackTo == "" && !rollbackList {
			if v, err := promptRollbackVersion(name); err == nil && v != "" {
				rollbackTo = v
			}
		}

		if err := app.Manager.LoadState(); err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load state", err)
		}

		appInfo, err := GetAppInfo(name)
		if err != nil {
			return err
		}
		name = appInfo.Name

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

		// Check whether a deploy state exists (blue-green / rolling).
		state, deployErr := deploy.Load(name)
		if deployErr != nil || state == nil || state.Mode == "" {
			return rollbackClassic(appInfo, name, target)
		}

		// --- Zero-downtime rollback path (blue-green / rolling) ---
		return rollbackZeroDowntime(appInfo, name, target, state)
	},
}

func init() {
	RollbackCmd.Flags().StringVar(&rollbackTo, "to", "", "Roll back to a specific version (e.g. v3, 3, or a tag name)")
	RollbackCmd.Flags().BoolVar(&rollbackList, "list", false, "List retained versions with metadata")
}

// promptRollbackVersion offers an interactive choice of which retained version
// to roll back to. Returns "" (and no error) when the user declines or no
// versions are available, so callers fall back to the previous-version default.
func promptRollbackVersion(appName string) (string, error) {
	policy := deploy.DefaultRetention{Max: 5}
	vers, err := deploy.ListVersionsForDisplay(appName, policy)
	if err != nil || len(vers) == 0 {
		return "", err
	}

	opts := make([]string, 0, len(vers))
	for _, v := range vers {
		label := fmt.Sprintf("v%d", v.Version)
		if v.Tag != "" {
			label = fmt.Sprintf("%s (%s)", label, v.Tag)
		}
		if v.IsCurrent {
			label += " — current"
		}
		opts = append(opts, label)
	}

	chosen, err := PromptSelect("Roll back to which version?", opts)
	if err != nil {
		return "", err
	}
	// Map "v3 (hotfix-auth)" / "v3 — current" back to the bare version string.
	idx := strings.Index(chosen, " ")
	if idx > 0 {
		chosen = chosen[:idx]
	}
	return strings.TrimPrefix(chosen, "v"), nil
}

// rollbackZeroDowntime handles rollback through the blue-green or rolling
// deploy path with full lifecycle event reporting.
func rollbackZeroDowntime(appInfo *app.AppInfo, appName string, target int, state *deploy.DeployState) error {
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
	err = deploy.ExecuteRollback(context.Background(), deploy.RollbackOptions{
		AppName:        appName,
		AppID:          appInfo.ID,
		PublicPort:     publicPort,
		TargetVersion:  target,
		Launcher:       deploy.DefaultLauncher,
		ProxyClient:    proxyClient,
		HealthProvider: deploy.DefaultHealthProvider(),
		Logger:         &colorLogger{},
	})
	rollbackDuration := time.Since(stepStart)
	r.SetMetadata("rollback_duration_ms", fmt.Sprintf("%d", rollbackDuration.Milliseconds()))
	if err != nil {
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
func rollbackClassic(appInfo *app.AppInfo, appName string, target int) error {
	totalStart := time.Now()
	strategy := "classic"

	// Resolve current version for the reporter.
	currentVer, _ := deploy.CurrentVersion(appName)
	currentVerStr := fmt.Sprintf("v%d", currentVer)
	targetVerStr := fmt.Sprintf("v%d", target)

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
		return phelixerr.Wrap(phelixerr.CodeRollbackTargetNotFound, "failed to resolve version paths", err)
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
		return phelixerr.Wrap(phelixerr.CodeRollbackFailed, "failed to copy versioned binary", err)
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
		return phelixerr.Wrapf(
			phelixerr.CodeRollbackFailed,
			err,
			"failed to start rolled-back application %q (ID: %s)",
			appName,
			appInfo.ID,
		)
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
