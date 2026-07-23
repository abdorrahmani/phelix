package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	"github.com/abdorrahmani/phelix/internal/proxy"
	"github.com/fatih/color"
	"github.com/olekukonko/tablewriter"
	"github.com/spf13/cobra"
)

var rollbackTo string
var rollbackList bool

var RollbackCmd = &cobra.Command{
	Use:           "rollback <AppName>",
	Short:         "Roll back to a previous versioned build",
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		if err := app.Manager.LoadState(); err != nil {
			return fmt.Errorf("%s failed to load state: %v", color.RedString("✗"), err)
		}

		appInfo, err := GetAppInfo(name)
		if err != nil {
			return err
		}
		name = appInfo.Name

		policy := deploy.DefaultRetention{Max: 5}

		if rollbackList {
			return listRollbackVersions(name, policy)
		}

		target := 0
		if rollbackTo != "" {
			// Accept either a version ID (v3, 3) or a unique tag name.
			target, err = deploy.ResolveVersionOrTag(name, rollbackTo)
			if err != nil {
				return fmt.Errorf("%s %v", color.RedString("✗"), err)
			}
		} else {
			target, err = deploy.PreviousVersion(name)
			if err != nil {
				return fmt.Errorf("%s %v", color.RedString("✗"), err)
			}
		}

		// Check whether a deploy state exists (blue-green / rolling).
		// If not, fall back to the classic rollback path: stop → copy
		// versioned binary → start → promote.
		state, deployErr := deploy.Load(name)
		if deployErr != nil || state == nil || state.Mode == "" {
			return rollbackClassic(appInfo, name, target)
		}

		// --- Zero-downtime rollback path (blue-green / rolling) ---
		socket, err := proxy.DefaultSocketPath()
		if err != nil {
			return fmt.Errorf("%s could not determine proxy socket path: %v", color.RedString("✗"), err)
		}
		fmt.Printf("  %s Ensuring proxy daemon is running...\n", color.BlueString("→"))
		if err := proxy.EnsureDaemon(context.Background(), "", 5*time.Second); err != nil {
			return fmt.Errorf("%s %v\n  Start it manually with: %s",
				color.RedString("✗"), err, color.CyanString("phelix proxy"))
		}
		proxyClient := proxy.NewClient(socket)
		if err := proxyClient.Ping(context.Background()); err != nil {
			return fmt.Errorf("%s %v\n  Start it first with: %s",
				color.RedString("✗"), err, color.CyanString("phelix proxy"))
		}

		publicPort := state.PublicPort

		fmt.Printf("%s Rolling back %s to v%d (zero-downtime via %s)\n",
			color.BlueString("→"), color.CyanString("'%s'", name), target,
			color.MagentaString(string(state.Mode)))

		err = deploy.ExecuteRollback(context.Background(), deploy.RollbackOptions{
			AppName:        name,
			AppID:          appInfo.ID,
			PublicPort:     publicPort,
			TargetVersion:  target,
			Launcher:       deploy.DefaultLauncher,
			ProxyClient:    proxyClient,
			HealthProvider: deploy.DefaultHealthProvider(),
			Logger:         &colorLogger{},
		})
		if err != nil {
			return err
		}
		fmt.Printf("%s Rollback of %s to v%d complete\n", color.GreenString("✓"), color.CyanString("'%s'", name), target)
		return nil
	},
}

func init() {
	RollbackCmd.Flags().StringVar(&rollbackTo, "to", "", "Roll back to a specific version (e.g. v3, 3, or a tag name)")
	RollbackCmd.Flags().BoolVar(&rollbackList, "list", false, "List retained versions with metadata")
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
	// Resolve versioned binary path.
	binPath, _, err := deploy.VersionPaths(appName, target)
	if err != nil {
		return fmt.Errorf("%s %v", color.RedString("✗"), err)
	}

	fmt.Printf("%s Rolling back %s to v%d (classic stop→start)\n",
		color.BlueString("→"), color.CyanString("'%s'", appName), target)

	// Stop the current instance if running.
	if appInfo.Status == "running" {
		fmt.Printf("  %s Stopping current instance (PID %d)...\n", color.BlueString("→"), appInfo.PID)
		if err := app.Manager.StopApplication(appInfo.ID); err != nil {
			// Non-fatal: the process may have already exited.
			fmt.Printf("  %s Warning: stop returned: %v\n", color.YellowString("⚠"), err)
		}
	}

	// Copy the versioned binary to the expected app location so
	// app.Manager.StartApplication can find it.
	destBin := filepath.Join(appInfo.Directory, fmt.Sprintf("app_%s", appInfo.ID))
	fmt.Printf("  %s Copying v%d binary to %s...\n", color.BlueString("→"), target, destBin)
	if err := copyFileForRollback(binPath, destBin); err != nil {
		return fmt.Errorf("%s failed to copy versioned binary: %v", color.RedString("✗"), err)
	}

	// Determine the port: use the existing app port, or fall back to default.
	port := appInfo.Port
	if port == 0 {
		port = defaultPort
	}

	// Start the app with the rolled-back binary.
	fmt.Printf("  %s Starting rolled-back binary on port %d...\n", color.BlueString("→"), port)
	if err := app.Manager.StartApplication(appInfo.ID, port, appName); err != nil {
		// Deploy failed. The version exists on disk but is_current was
		// never set and PromoteVersion was never called, so the user can
		// retry without a broken "current" pointer.
		return fmt.Errorf("%s failed to start rolled-back application: %v", color.RedString("✗"), err)
	}

	// Deploy succeeded — promote the version.
	if err := deploy.PromoteVersion(appName, target, "classic"); err != nil {
		fmt.Printf("  %s Warning: could not promote version: %v\n", color.YellowString("⚠"), err)
	}

	fmt.Printf("%s Rollback of %s to v%d complete\n", color.GreenString("✓"), color.CyanString("'%s'", appName), target)
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

func listRollbackVersions(appName string, policy deploy.RetentionPolicy) error {
	vers, err := deploy.ListVersionsForDisplay(appName, policy)
	if err != nil {
		return fmt.Errorf("%s %v", color.RedString("✗"), err)
	}
	if len(vers) == 0 {
		fmt.Printf("  No versioned builds recorded for %s yet.\n", color.CyanString("'%s'", appName))
		return nil
	}

	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"Version", "Tag", "Commit", "Built", "Size", "Current", "Prune soon"})
	table.SetBorder(true)
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
	return nil
}
