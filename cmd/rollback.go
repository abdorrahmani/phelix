package cmd

import (
	"context"
	"fmt"
	"os"
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
	Short:         "Roll back to a previous versioned build with zero downtime",
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

		logger := &colorLogger{}
		state, err := deploy.Load(name)
		publicPort := 8080
		if err == nil && state != nil {
			publicPort = state.PublicPort
		}

		fmt.Printf("%s Rolling back %s to v%d (zero-downtime via %s)\n",
			color.BlueString("→"), color.CyanString("'%s'", name), target,
			color.MagentaString("existing deploy path"))

		err = deploy.ExecuteRollback(context.Background(), deploy.RollbackOptions{
			AppName:        name,
			AppID:          appInfo.ID,
			PublicPort:     publicPort,
			TargetVersion:  target,
			Launcher:       deploy.DefaultLauncher,
			ProxyClient:    proxyClient,
			HealthProvider: deploy.DefaultHealthProvider(),
			Logger:         logger,
		})
		if err != nil {
			return err
		}
		fmt.Printf("%s Rollback of %s to v%d complete\n", color.GreenString("✓"), color.CyanString("'%s'", name), target)
		return nil
	},
}

func init() {
	RollbackCmd.Flags().StringVar(&rollbackTo, "to", "", "Roll back to a specific version (e.g. v3 or 3)")
	RollbackCmd.Flags().BoolVar(&rollbackList, "list", false, "List retained versions with metadata")
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
	table.SetHeader([]string{"Version", "Commit", "Built", "Size", "Current", "Prune soon"})
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
		prune := ""
		if deploy.WouldPruneOnNextBuild(appName, v.Version, policy) {
			prune = color.YellowString("yes")
		}
		table.Append([]string{
			fmt.Sprintf("v%d", v.Version),
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
