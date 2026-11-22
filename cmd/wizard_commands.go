package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/abdorrahmani/phelix/internal/controlapi"
	pb "github.com/abdorrahmani/phelix/internal/controlapi/proto"
	"github.com/abdorrahmani/phelix/internal/controller"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/secrets"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var errWizardCancelled = errors.New("wizard cancelled")

// ConfigureWizards adds guided input to the commands available in the user's terminal.
func ConfigureWizards(root *cobra.Command) {
	configureWizards(root, IsInteractive, PromptSelect, PromptString, PromptPassword)
}

// configureWizards keeps guided input ahead of validation and uses each command's usual action.
func configureWizards(root *cobra.Command, interactive func() bool, choose func(string, []string) (string, error), input func(string, string) (string, error), password func(string) (string, error)) {
	root.InitDefaultCompletionCmd()
	var visit func(*cobra.Command)
	visit = func(command *cobra.Command) {
		for _, child := range command.Commands() {
			visit(child)
		}
		if wizardExcluded(command) {
			return
		}
		validate := command.Args
		run := command.RunE
		if run == nil && command.Run != nil {
			original := command.Run
			run = func(c *cobra.Command, args []string) error {
				original(c, args)
				return nil
			}
		}
		var prepared []string
		var cancelled bool
		command.Args = func(c *cobra.Command, args []string) error {
			prepared, cancelled = args, false
			if interactive() && (!c.HasAvailableSubCommands() || len(args) > 0 || (run != nil && c.Flags().NFlag() > 0)) {
				var err error
				prepared, err = prepareWizardCommand(c, args, choose, input, password)
				if errors.Is(err, errWizardCancelled) {
					cancelled = true
					return nil
				}
				if err != nil {
					return err
				}
			}
			if validate != nil {
				return validate(c, prepared)
			}
			return nil
		}
		command.RunE = func(c *cobra.Command, args []string) error {
			if cancelled {
				return nil
			}
			if interactive() && c.HasAvailableSubCommands() && len(args) == 0 && (run == nil || c.Flags().NFlag() == 0) {
				var action func(*cobra.Command, []string) error
				if run != nil {
					action = func(c *cobra.Command, args []string) error {
						filled, err := prepareWizardCommand(c, args, choose, input, password)
						if errors.Is(err, errWizardCancelled) {
							return nil
						}
						if err != nil {
							return err
						}
						if validate != nil {
							if err := validate(c, filled); err != nil {
								return err
							}
						}
						return run(c, filled)
					}
				}
				return runWizardMenu(c, choose, action)
			}
			if run != nil {
				return run(c, prepared)
			}
			return c.Help()
		}
	}
	visit(root)
}

// prepareWizardCommand collects missing details while keeping supplied arguments and flags.
func prepareWizardCommand(command *cobra.Command, args []string, choose func(string, []string) (string, error), input func(string, string) (string, error), password func(string) (string, error)) ([]string, error) {
	if enabled, _ := command.Flags().GetBool("json"); enabled {
		restore := machine.EnterJSON()
		defer restore()
	}
	for _, flag := range []string{"all", "workspace"} {
		if enabled, _ := command.Flags().GetBool(flag); enabled {
			return args, nil
		}
	}
	if command.Name() == "build" {
		if resume, _ := command.Flags().GetString("resume"); resume != "" {
			return args, nil
		}
	}
	args = append([]string{}, args...)
	if command.Name() == "env" && command.Parent() == command.Root() {
		if len(args) == 0 {
			action, err := choose("Which environment action?", []string{"set", "get", "list", "unset", "check"})
			if err != nil {
				return nil, err
			}
			args = append(args, action)
		}
		switch args[0] {
		case "set", "get", "list", "unset", "check":
		default:
			return nil, phelixerr.New(phelixerr.CodeInvalidArgument, "choose set, get, list, unset, or check")
		}
		if len(args) == 1 {
			var err error
			args, err = promptControllerApp(command, args, choose)
			if err != nil {
				return nil, err
			}
		}
		if len(args) == 2 && args[0] != "list" {
			var value string
			var err error
			if args[0] == "set" {
				value, err = password("Environment variable (KEY=VALUE)")
			} else {
				value, err = input("Environment variable name", "")
			}
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(value) == "" {
				return nil, phelixerr.New(phelixerr.CodeInvalidArgument, "an environment variable is required")
			}
			args = append(args, value)
		}
	} else {
		appIndex, _ := wizardAppArgument(command)
		for index, argument := range wizardPositionals(command) {
			if index < len(args) {
				continue
			}
			if index == appIndex {
				var err error
				args, err = promptControllerApp(command, args, choose)
				if err != nil {
					return nil, err
				}
				continue
			}
			if strings.HasPrefix(argument, "[") {
				if command.Name() != "build" || argument != "[NAME]" {
					break
				}
			}
			label := strings.Trim(argument, "[]<>")
			def := ""
			if command.Name() == "build" && label == "NAME" {
				cfg, err := loadProjectConfig()
				if err != nil {
					return nil, err
				}
				if cfg != nil {
					def = cfg.Name
				}
				label = "Application name"
			}
			value, err := input(strings.ReplaceAll(label, "-", " "), def)
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(value) == "" {
				return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument, "%s is required", label)
			}
			args = append(args, value)
		}
	}
	if err := promptWizardFlags(command, input, choose); err != nil {
		return nil, err
	}
	if command.Parent() != nil && command.Parent().Name() == "auth" && command.Name() == "login" {
		for _, name := range []string{"username", "apiKey"} {
			flag := command.Flags().Lookup(name)
			if flag == nil || flag.Changed || flag.Value.String() != "" {
				continue
			}
			var value string
			var err error
			if name == "apiKey" {
				value, err = password("API key")
			} else {
				value, err = input("Username", "")
			}
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(value) == "" {
				return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument, "%s is required", name)
			}
			if err := command.Flags().Set(name, value); err != nil {
				return nil, err
			}
		}
	}
	if command.Name() == "rollback" && len(args) == 1 && command.Flags().Lookup("to") != nil {
		to, _ := command.Flags().GetString("to")
		list, _ := command.Flags().GetBool("list")
		dryRun, _ := command.Flags().GetBool("dry-run")
		if to == "" && !list && !dryRun {
			if err := promptControllerVersions(command, args[0], choose); err != nil {
				return nil, err
			}
			if flag := command.Flags().Lookup("reason"); flag != nil && !flag.Changed {
				reason, err := input("Reason (optional)", "")
				if err != nil {
					return nil, err
				}
				if strings.TrimSpace(reason) != "" {
					if err := command.Flags().Set("reason", reason); err != nil {
						return nil, err
					}
				}
			}
			if command.Parent() == command.Root() && command.Flags().Lookup("dry-run") != nil {
				flag := command.Flags().Lookup("dry-run")
				changed := flag.Changed
				if err := command.Flags().Set("dry-run", "true"); err != nil {
					return nil, err
				}
				err := runControlClient(command, args)
				if resetErr := command.Flags().Set("dry-run", "false"); resetErr != nil {
					return nil, resetErr
				}
				flag.Changed = changed
				if err != nil {
					return nil, err
				}
				answer, err := choose("Proceed with this rollback?", []string{"Cancel", "Proceed"})
				if err != nil {
					return nil, err
				}
				if answer != "Proceed" {
					fmt.Fprintln(command.OutOrStdout(), "Rollback cancelled.")
					return nil, errWizardCancelled
				}
			}
		}
	}
	if command.Parent() != nil && command.Parent().Name() == "secrets" && command.Name() == "set" {
		if fromFile, _ := command.Flags().GetString("from-file"); fromFile == "" {
			value, err := password("Secret value")
			if err != nil {
				return nil, err
			}
			if err := secrets.ValidateSecretValue(value); err != nil {
				return nil, err
			}
			command.SetIn(strings.NewReader(value))
		}
	}
	return args, nil
}

// promptControllerApp gets the available applications from the running controller.
func promptControllerApp(command *cobra.Command, args []string, choose func(string, []string) (string, error)) ([]string, error) {
	cfg, err := controller.LoadClientConfig()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(command.Context(), 3*time.Second)
	conn, client, err := controlapi.Connect(ctx, cfg.Socket)
	cancel()
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeConnection, "Controller unavailable", err)
	}
	defer conn.Close()
	return selectControlApp(command, args, client, choose)
}

// promptWizardFlags asks for the settings needed to complete the chosen task.
func promptWizardFlags(command *cobra.Command, input func(string, string) (string, error), choose func(string, []string) (string, error)) error {
	var promptErr error
	command.Flags().VisitAll(func(flag *pflag.Flag) {
		if promptErr != nil || flag.Changed {
			return
		}
		required := len(flag.Annotations[cobra.BashCompOneRequiredFlag]) > 0
		def := flag.DefValue
		path := strings.TrimPrefix(command.CommandPath(), command.Root().Name()+" ")
		switch path {
		case "scale":
			if flag.Name == "replicas" {
				required, def = true, "1"
			}
		case "health set":
			if flag.Name == "path" {
				required, def = true, "/health"
			}
		case "autoscale pause":
			required = required || flag.Name == "reason"
		case "session checkpoint":
			if flag.Name == "note" {
				required = true
				for _, name := range []string{"step", "plan", "operation", "deployment"} {
					if command.Flags().Changed(name) {
						required = false
					}
				}
			}
		case "build":
			if flag.Name == "port" {
				cfg, err := loadProjectConfig()
				if err != nil {
					promptErr = err
					return
				}
				matrix, _ := command.Flags().GetBool("matrix")
				if matrix || matrixActive(command, cfg) || cfg.IsWorker() || cfg.IsJob() {
					return
				}
				if cfg != nil && cfg.Port != 0 {
					return
				}
				required = true
			}
		}
		if !required {
			return
		}
		value, err := input(flag.Usage, def)
		if err == nil && strings.TrimSpace(value) == "" {
			err = phelixerr.Newf(phelixerr.CodeInvalidArgument, "--%s is required", flag.Name)
		}
		if err == nil {
			err = command.Flags().Set(flag.Name, value)
		}
		promptErr = err
	})
	if promptErr != nil {
		return promptErr
	}
	if command.Name() == "dockerize" {
		for _, name := range []string{"tag", "push", "registry"} {
			flag := command.Flags().Lookup(name)
			if flag == nil || flag.Changed {
				continue
			}
			if name == "registry" {
				push, _ := command.Flags().GetBool("push")
				if !push {
					continue
				}
			}
			var value string
			var err error
			if name == "push" {
				answer, chooseErr := choose("Push the image after building?", []string{"No", "Yes"})
				err = chooseErr
				value = fmt.Sprint(answer == "Yes")
			} else {
				value, err = input(flag.Usage, flag.DefValue)
			}
			if err != nil {
				return err
			}
			if err := command.Flags().Set(name, value); err != nil {
				return err
			}
		}
	}
	return nil
}

// promptControllerVersions offers previous builds before submitting a rollback.
func promptControllerVersions(command *cobra.Command, application string, choose func(string, []string) (string, error)) error {
	cfg, err := controller.LoadClientConfig()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(command.Context(), 10*time.Second)
	defer cancel()
	conn, client, err := controlapi.Connect(ctx, cfg.Socket)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeConnection, "Controller unavailable", err)
	}
	defer conn.Close()
	response, err := client.Query(ctx, &pb.QueryRequest{Kind: pb.QueryKind_VERSIONS, Application: application})
	if err != nil {
		return err
	}
	var options []string
	targets := map[string]string{}
	for _, version := range response.Versions {
		if version.Current {
			continue
		}
		label := fmt.Sprintf("v%d", version.Number)
		if version.Tag != "" {
			label += " (" + version.Tag + ")"
		}
		options = append(options, label)
		targets[label] = fmt.Sprint(version.Number)
	}
	if len(options) == 0 {
		fmt.Fprintln(command.OutOrStdout(), "No previous versions are available for rollback.")
		return errWizardCancelled
	}
	chosen, err := choose("Select version to roll back to", options)
	if err != nil {
		return err
	}
	target, ok := targets[chosen]
	if !ok {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "selected version was not found")
	}
	return command.Flags().Set("to", target)
}
