package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/hostsecurity"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/server"
	"github.com/spf13/cobra"
)

func init() { SecurityCmd.AddCommand(newServerSecurityCommand()) }

func newServerSecurityCommand() *cobra.Command {
	var jsonOutput bool
	root := &cobra.Command{Use: "server", Short: "Inspect this server's operating system packages and security", Args: cobra.NoArgs}
	root.PersistentFlags().BoolVar(&jsonOutput, "json", false, "emit a structured result")
	root.PersistentFlags().Duration("timeout", 5*time.Minute, "maximum operation duration")
	for _, name := range []string{"scan", "status", "inventory", "history"} {
		name := name
		sub := &cobra.Command{Use: name, Short: map[string]string{"scan": "Collect a read-only host package snapshot", "status": "Show the latest host scan and scanner availability", "inventory": "List installed packages from a recorded host snapshot", "history": "List immutable host scan records"}[name], Args: cobra.NoArgs, SilenceUsage: true, SilenceErrors: true}
		sub.Flags().Int("limit", 100, "maximum results (1..500)")
		sub.Flags().Int("offset", 0, "results to skip")
		sub.Flags().String("scan-id", "", "inspect a specific historical scan")
		sub.Flags().String("package", "", "filter package names")
		if name == "scan" {
			sub.Flags().Bool("full", false, "collect a full inventory (the default)")
		}
		sub.RunE = func(cmd *cobra.Command, args []string) error {
			if jsonOutput {
				defer machine.EnterJSON()()
			}
			limit, _ := cmd.Flags().GetInt("limit")
			offset, _ := cmd.Flags().GetInt("offset")
			if err := hostsecurity.ValidatePage(limit, offset); err != nil {
				return phelixerr.New(phelixerr.CodeInvalidArgument, err.Error())
			}
			duration, _ := cmd.Flags().GetDuration("timeout")
			if duration <= 0 || duration > 30*time.Minute {
				return phelixerr.New(phelixerr.CodeInvalidArgument, "timeout must be positive and at most 30m")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), duration)
			defer cancel()
			store, err := hostsecurity.OpenStore(server.DataDir())
			if err != nil {
				return err
			}
			defer store.Close()
			id, err := server.EnsureAgentID()
			if err != nil {
				return err
			}
			scanID, _ := cmd.Flags().GetString("scan-id")
			search, _ := cmd.Flags().GetString("package")
			var result any
			switch name {
			case "scan":
				progress := func(stage string) {
					if !jsonOutput {
						fmt.Fprintln(cmd.ErrOrStderr(), stage)
					}
				}
				result, err = hostsecurity.Collect(ctx, hostsecurity.LocalSystem(), store, id, progress)
			case "status":
				result, err = store.Scan(ctx, id, scanID)
			case "inventory":
				result, err = store.Inventory(ctx, id, scanID, search, limit, offset)
			case "history":
				result, err = store.History(ctx, id, limit, offset)
			}
			if errors.Is(err, sql.ErrNoRows) {
				return phelixerr.New(phelixerr.CodeNotFound, "no host snapshot found; run phelix security server scan")
			}
			if err != nil {
				return err
			}
			if jsonOutput {
				return machine.WriteEnvelope(machine.Success("", result))
			}
			return printServerSecurity(cmd, result)
		}
		root.AddCommand(sub)
	}
	return root
}

func printServerSecurity(cmd *cobra.Command, result any) error {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	switch value := result.(type) {
	case hostsecurity.Scan:
		fmt.Fprintf(w, "Scan\t%s\nStatus\t%s\nOS\t%s\nPackage manager\t%s\nPackages\t%d\nCoverage\t%s\nFinished\t%s\n", value.ID, value.Status, value.Host.Name, value.Host.Manager, value.PackageCount, value.Coverage, value.FinishedAt.Format(time.RFC3339))
		for _, warning := range value.Warnings {
			fmt.Fprintf(w, "Warning\t%s\n", warning)
		}
		if value.Host.Reason != "" {
			fmt.Fprintf(w, "Availability\t%s\n", value.Host.Reason)
		}
	case hostsecurity.Page[hostsecurity.Package]:
		fmt.Fprintln(w, "PACKAGE\tVERSION\tARCH\tSOURCE\tHELD")
		for _, p := range value.Items {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", p.Name, p.Version, p.Architecture, p.Source, p.Held)
		}
		fmt.Fprintf(w, "Showing %d of %d packages\n", len(value.Items), value.Total)
	case hostsecurity.Page[hostsecurity.Scan]:
		fmt.Fprintln(w, "SCAN\tSTATUS\tPACKAGES\tFINISHED")
		for _, s := range value.Items {
			fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", s.ID, s.Status, s.PackageCount, s.FinishedAt.Format(time.RFC3339))
		}
		fmt.Fprintf(w, "Showing %d of %d scans\n", len(value.Items), value.Total)
	default:
		return fmt.Errorf("unsupported result %s", strings.TrimSpace(fmt.Sprintf("%T", result)))
	}
	return w.Flush()
}
