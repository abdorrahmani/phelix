package cmd

import (
	"strings"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/spf13/cobra"
)

var WizardCmd = &cobra.Command{
	Use:   "wizard",
	Short: "Choose a task and follow the prompts",
	Long: `Choose a command, then follow the prompts for its missing details.
All commands are available except security and log.
Use an interactive terminal to open the menu.`,
	Args: cobra.NoArgs,
	RunE: func(command *cobra.Command, args []string) error {
		if !IsInteractive() {
			return phelixerr.New(phelixerr.CodeInvalidArgument,
				"the wizard requires an interactive terminal; run a specific command instead")
		}
		return runWizardMenu(command.Root(), PromptSelect, nil)
	},
}

// wizardExcluded keeps the requested exceptions out of every wizard menu and prompt.
func wizardExcluded(command *cobra.Command) bool {
	for current := command; current != nil; current = current.Parent() {
		switch current.Name() {
		case "security", "log", "wizard", "help":
			return true
		}
	}
	return command.Hidden
}

// runWizardMenu follows the available command choices and runs the selected task.
func runWizardMenu(command *cobra.Command, choose func(string, []string) (string, error), run func(*cobra.Command, []string) error) error {
	options := []string{"Cancel"}
	if run != nil {
		options = append(options, "Run "+command.Name())
	}
	choices := map[string]*cobra.Command{}
	for _, child := range command.Commands() {
		if wizardExcluded(child) || !child.IsAvailableCommand() {
			continue
		}
		label := child.Name()
		options = append(options, label)
		choices[label] = child
	}
	selected, err := func() (string, error) {
		if enabled, _ := command.Flags().GetBool("json"); enabled {
			restore := machine.EnterJSON()
			defer restore()
		}
		return choose("Choose a "+command.Name()+" task", options)
	}()
	if err != nil {
		return err
	}
	if selected == "Cancel" {
		return nil
	}
	if run != nil && selected == "Run "+command.Name() {
		return run(command, nil)
	}
	child, ok := choices[selected]
	if !ok {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "selected command was not found")
	}
	if child.HasAvailableSubCommands() && !child.Runnable() {
		return runWizardMenu(child, choose, nil)
	}
	argv := strings.Fields(strings.TrimPrefix(child.CommandPath(), child.Root().Name()+" "))
	child.Root().SetArgs(argv)
	return child.Root().ExecuteContext(command.Context())
}
