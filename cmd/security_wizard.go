package cmd

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/abdorrahmani/phelix/internal/security"
	"github.com/abdorrahmani/phelix/internal/server"
	"github.com/abdorrahmani/phelix/internal/table"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var securityHistoryLimit int

// securityMenuActions are the bare `phelix security` choices.
var securityMenuActions = []string{
	"Scan an application",
	"View the latest report",
	"View scan history",
	"Inspect an advisory",
	"View or change security configuration",
	"Exit",
}

// securityTargetChoice is one interactive target option: the label shown
// and the argument it feeds the existing resolver (empty means the
// current directory project).
type securityTargetChoice struct {
	label string
	arg   string
}

// buildSecurityTargetChoices builds the interactive selection list from
// registered applications plus, when the working directory is a project,
// the current directory itself. Pure so tests can pin the ordering.
func buildSecurityTargetChoices(apps []app.AppListItem, cwdProjectName string) []securityTargetChoice {
	choices := make([]securityTargetChoice, 0, len(apps)+2)
	if cwdProjectName != "" {
		choices = append(choices, securityTargetChoice{
			label: "Current directory (" + cwdProjectName + ")",
			arg:   "",
		})
	}
	for _, a := range apps {
		label := a.Name
		if label == "" {
			label = a.ID
		}
		choices = append(choices, securityTargetChoice{label: label + " (" + a.ID + ")", arg: label})
	}
	return choices
}

// promptSecurityTarget asks which application to operate on. Cancellation
// propagates as an input-cancelled error, ending the command cleanly.
func promptSecurityTarget(verb string) ([]string, error) {
	if err := app.Manager.LoadState(); err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load application state", err)
	}
	cwdName := ""
	dir, err := os.Getwd()
	if err == nil && project.Exists(dir) {
		if cfg, err := project.Load(dir); err == nil {
			cwdName = cfg.Name
		}
	}
	choices := buildSecurityTargetChoices(app.Manager.ListApplications(), cwdName)
	if len(choices) == 0 {
		return nil, phelixerr.New(phelixerr.CodeNotFound,
			"no applications are registered and the current directory is not a Phelix project")
	}
	labels := make([]string, 0, len(choices)+1)
	for _, choice := range choices {
		labels = append(labels, choice.label)
	}
	labels = append(labels, "Cancel")
	selected, err := PromptSelect(fmt.Sprintf("Which application do you want to %s?", verb), labels)
	if err != nil {
		return nil, err
	}
	for _, choice := range choices {
		if choice.label == selected {
			if choice.arg == "" {
				return nil, nil
			}
			return []string{choice.arg}, nil
		}
	}
	return nil, phelixerr.New(phelixerr.CodeInvalidArgument, "cancelled")
}

// runSecurityMenu is the bare `phelix security` experience: an interactive
// menu in a terminal, help text everywhere else. Explicit subcommands are
// unaffected.
func runSecurityMenu(cmd *cobra.Command, args []string) error {
	if !IsInteractive() {
		return cmd.Help()
	}
	for {
		selected, err := PromptSelect("What do you want to do?", securityMenuActions)
		if err != nil {
			return nil
		}
		switch selected {
		case "Exit":
			return nil
		case "Scan an application":
			if err := securityMenuDispatch(runSecurityScan, "scan"); err != nil {
				printMenuError(err)
			}
		case "View the latest report":
			if err := securityMenuDispatch(runSecurityReport, "view the report for"); err != nil {
				printMenuError(err)
			}
		case "View scan history":
			if err := securityMenuDispatch(runSecurityHistory, "view the history of"); err != nil {
				printMenuError(err)
			}
		case "Inspect an advisory":
			id, err := PromptString("Advisory identifier (e.g. GHSA-xxxx-xxxx-xxxx):", "")
			if err != nil {
				continue
			}
			if id == "" {
				printMenuError(phelixerr.New(phelixerr.CodeInvalidArgument, "an advisory identifier is required"))
				continue
			}
			if err := runSecurityShow(nil, []string{id}); err != nil {
				printMenuError(err)
			}
		case "View or change security configuration":
			if err := securityMenuDispatch(runSecurityConfig, "configure"); err != nil {
				printMenuError(err)
			}
		}
	}
}

// securityMenuDispatch resolves a target interactively and runs the
// underlying command implementation with it.
func securityMenuDispatch(run func(*cobra.Command, []string) error, verb string) error {
	args, err := promptSecurityTarget(verb)
	if err != nil {
		return err
	}
	return run(nil, args)
}

// printMenuError surfaces a failed menu action without tearing down the
// menu loop.
func printMenuError(err error) {
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
}

type securityHistoryResult struct {
	AppName string                 `json:"app_name"`
	AppID   string                 `json:"app_id,omitempty"`
	Scans   []*security.ScanRecord `json:"scans"`
}

func runSecurityHistory(cmd *cobra.Command, args []string) error {
	restore := enterSecurityJSON()
	defer restore()

	target, err := resolveSecurityTarget(args)
	if err != nil {
		return err
	}
	store := security.NewScanStore(server.DataDir())
	records, err := store.ListScans(target.identityKey)
	if err != nil {
		return err
	}
	limit := securityHistoryLimit
	if limit <= 0 || limit > security.DefaultScanRetention {
		limit = security.DefaultScanRetention
	}
	if len(records) > limit {
		records = records[:limit]
	}

	if securityJSON {
		return machine.WriteEnvelope(machine.Success(target.identityKey, securityHistoryResult{
			AppName: target.appName,
			AppID:   target.appID,
			Scans:   records,
		}))
	}
	if len(records) == 0 {
		fmt.Printf("%s No scans recorded for %s; run 'phelix security scan' first.\n",
			colorYellowString(), target.appName)
		return nil
	}
	t := table.New(os.Stdout, false)
	t.Header([]string{"Scan", "Status", "Trigger", "Findings", "Coverage", "Finished"})
	for _, record := range records {
		findings := strconv.Itoa(record.Counts.Affected + record.Counts.PotentiallyAffected + record.Counts.Unknown)
		finished := ""
		if record.FinishedAt != nil {
			finished = record.FinishedAt.Format(time.RFC3339)
		}
		t.Append([]string{
			record.ScanID,
			string(record.Status),
			string(record.Trigger),
			findings,
			record.Coverage,
			finished,
		})
	}
	_ = t.Render()
	return nil
}

// colorYellowString wraps the shared color helper for the empty-history
// line.
func colorYellowString() string {
	return color.YellowString("⚠")
}
