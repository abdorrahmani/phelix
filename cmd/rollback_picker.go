package cmd

import (
	"fmt"

	"github.com/AlecAivazis/survey/v2"
	"github.com/abdorrahmani/phelix/internal/deploy"
)

// rollbackSelectTemplate extends survey's default Select template with a
// details footer for the focused option, so the picker shows the selected
// version's metadata without a second command. The description comes from the
// Select.Description callback via GetDescription; it renders for the
// PageEntries[SelectedIndex] entry, which already respects filtering.
var rollbackSelectTemplate = `
{{- define "option"}}
    {{- if eq .SelectedIndex .CurrentIndex }}{{color .Config.Icons.SelectFocus.Format }}{{ .Config.Icons.SelectFocus.Text }} {{else}}{{color "default"}}  {{end}}
    {{- .CurrentOpt.Value}}
    {{- color "reset"}}
{{end}}
{{- if .ShowHelp }}{{- color .Config.Icons.Help.Format }}{{ .Config.Icons.Help.Text }} {{ .Help }}{{color "reset"}}{{"\n"}}{{end}}
{{- color .Config.Icons.Question.Format }}{{ .Config.Icons.Question.Text }} {{color "reset"}}
{{- color "default+hb"}}{{ .Message }}{{ .FilterMessage }}{{color "reset"}}
{{- if .ShowAnswer}}{{color "cyan"}} {{.Answer}}{{color "reset"}}{{"\n"}}
{{- else}}
  {{- "  "}}{{- color "cyan"}}[Use arrows to move, type to filter]{{color "reset"}}
  {{- "\n"}}
  {{- range $ix, $option := .PageEntries}}
    {{- template "option" $.IterateOption $ix $option}}
  {{- end}}
  {{- "\n"}}
  {{- with $opt := index .PageEntries .SelectedIndex}}{{ $.GetDescription $opt }}{{end}}
{{- end}}`

// askSelectWithDetails runs a survey Select with the details-footer template.
// The template swap is global but the CLI prompts sequentially in a single
// goroutine, so saving/restoring around AskOne is safe.
func askSelectWithDetails(msg string, opts []string, details func(value string, index int) string) (string, error) {
	old := survey.SelectQuestionTemplate
	survey.SelectQuestionTemplate = rollbackSelectTemplate
	defer func() { survey.SelectQuestionTemplate = old }()

	var ans string
	prompt := &survey.Select{
		Message:     msg,
		Options:     opts,
		Description: details,
	}
	if err := survey.AskOne(prompt, &ans); err != nil {
		return "", err
	}
	return ans, nil
}

// pickerVersionDetails renders the details block shown below the candidate
// list for the currently focused version. Only stored metadata is used — no
// process is started and no network check runs while the cursor moves, so an
// unknown field renders as "—" (per-version health is not stored on disk).
func pickerVersionDetails(v deploy.VersionMeta) string {
	tag := v.Tag
	if tag == "" {
		tag = "—"
	}
	commit := v.GitCommit
	if len(commit) > 7 {
		commit = commit[:7]
	}
	if commit == "" {
		commit = "—"
	}
	built := relTime(v.BuiltAt)
	size := "—"
	if v.SizeBytes > 0 {
		size = fmt.Sprintf("%.1f MB", float64(v.SizeBytes)/(1024*1024))
	}
	mode := v.DeployMode
	if mode == "" {
		mode = "—"
	}
	return fmt.Sprintf(
		"v%d\n├── Tag: %s\n├── Commit: %s\n├── Built: %s\n├── Binary: %s\n├── Health: —\n└── Deploy: %s",
		v.Version, tag, commit, built, size, mode,
	)
}
