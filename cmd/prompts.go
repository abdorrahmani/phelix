package cmd

import (
	"fmt"
	"strings"

	"github.com/fatih/color"
)

// Confirm prints a yes/no prompt and returns the user's answer.
// It follows the existing ad-hoc prompt style used in cmd/auth (fmt.Scanln)
// and the project's ⚠/color convention. The default answer is No.
//
// Example output:
//
//	⚠ Rust toolchain is not installed. Install it now? [y/N]:
func Confirm(message string) bool {
	fmt.Printf("%s %s %s: ",
		color.YellowString("⚠"),
		message,
		color.YellowString("[y/N]"),
	)

	var response string
	_, _ = fmt.Scanln(&response)

	answer := strings.ToLower(strings.TrimSpace(response))
	return answer == "y" || answer == "yes"
}
