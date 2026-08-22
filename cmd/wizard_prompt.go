package cmd

import (
	"os"
	"strconv"
	"strings"

	"github.com/AlecAivazis/survey/v2"
	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"golang.org/x/term"
)

// IsInteractive reports whether stdin is connected to a terminal. The wizard
// only prompts when this is true; otherwise commands fall back to their normal
// missing-argument errors so the CLI stays scriptable in pipes/CI.
func IsInteractive() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// PromptString asks for a free-form value, pre-filling def as the default
// (survey shows it in [brackets]). Returns an empty result only if def was
// empty and the user hit enter.
func PromptString(msg, def string) (string, error) {
	ans := def
	prompt := &survey.Input{
		Message: msg,
		Default: def,
	}
	if err := survey.AskOne(prompt, &ans); err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeInvalidArgument, "input cancelled", err)
	}
	return strings.TrimSpace(ans), nil
}

// PromptInt asks for an integer, re-prompting until a valid parse is entered.
func PromptInt(msg string, def int) (int, error) {
	var raw string
	prompt := &survey.Input{
		Message: msg,
		Default: strconv.Itoa(def),
	}
	validate := func(val interface{}) error {
		s, _ := val.(string)
		if _, err := strconv.Atoi(strings.TrimSpace(s)); err != nil {
			return phelixerr.Newf(phelixerr.CodeInvalidArgument, "please enter a valid integer")
		}
		return nil
	}
	if err := survey.AskOne(prompt, &raw, survey.WithValidator(validate)); err != nil {
		return 0, phelixerr.Wrap(phelixerr.CodeInvalidArgument, "input cancelled", err)
	}
	n, _ := strconv.Atoi(strings.TrimSpace(raw))
	return n, nil
}

// PromptConfirm asks a yes/no question, defaulting to def.
func PromptConfirm(msg string, def bool) (bool, error) {
	ans := def
	prompt := &survey.Confirm{
		Message: msg,
		Default: def,
	}
	if err := survey.AskOne(prompt, &ans); err != nil {
		return false, phelixerr.Wrap(phelixerr.CodeInvalidArgument, "input cancelled", err)
	}
	return ans, nil
}

// PromptSelect presents a single-choice menu from opts. The first option is
// preselected. Passes no Default to survey so the menu always renders — a
// non-matching Default makes survey error out before painting the list.
func PromptSelect(msg string, opts []string) (string, error) {
	if len(opts) == 0 {
		return "", phelixerr.Newf(phelixerr.CodeInvalidArgument, "no options to choose from")
	}
	var ans string
	prompt := &survey.Select{
		Message: msg,
		Options: opts,
	}
	if err := survey.AskOne(prompt, &ans); err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeInvalidArgument, "selection cancelled", err)
	}
	return ans, nil
}

// PromptApp lists locally-managed applications as "Name (ID)" and returns the
// chosen app name. When allowAll is true, "All applications" is prepended so
// callers (e.g. start) can offer a multi-app choice. Returns a clear error if
// no apps exist yet.
func PromptApp(allowAll bool, msg string) (string, error) {
	apps := app.Manager.ListApplications()
	if len(apps) == 0 {
		return "", phelixerr.Newf(
			phelixerr.CodeNotFound,
			"no applications found — build one first with 'phelix build <NAME>'",
		)
	}

	opts := make([]string, 0, len(apps)+1)
	if allowAll {
		opts = append(opts, "All applications")
	}
	for _, a := range apps {
		label := a.Name
		if a.Name == "" {
			label = a.ID
		}
		opts = append(opts, label)
	}

	chosen, err := PromptSelect(msg, opts)
	if err != nil {
		return "", err
	}
	if chosen == "All applications" {
		return "", nil
	}
	return chosen, nil
}

// PromptEnvAction chooses which env sub-command the wizard should run.
func PromptEnvAction() (string, error) {
	return PromptSelect(
		"Which environment action?",
		[]string{"set", "get", "list", "unset", "check"},
	)
}
