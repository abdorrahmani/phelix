package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/fatih/color"
)

// Debug is the global --debug binding. When true, error rendering includes the
// underlying root cause (never secrets). It is wired to the root command's
// persistent --debug flag in main.go.
var Debug bool

// errOut is the destination for rendered CLI errors. It is a var so tests can
// capture output; production always uses stderr.
var errOut io.Writer = os.Stderr

// RenderError presents an error to the user on stderr and returns the process
// exit code derived from its category. This is the single CLI error boundary:
// every command's error flows through here (via main.go), so presentation is
// consistent across Phelix.
func RenderError(err error, debug bool) int {
	if err == nil {
		return ExitOK
	}
	return renderCLIError(err, debug)
}

// CLI exit codes. These are part of the CLI's external contract for scripts,
// so they are documented in the README and must remain stable. They are
// derived from the structured error category, never from arbitrary internals.
const (
	ExitOK         = 0  // success
	ExitFailure    = 1  // generic failure
	ExitUsage      = 2  // invalid usage / invalid arguments
	ExitAuth       = 10 // authentication failure
	ExitPermission = 11 // permission denied
	ExitNotFound   = 12 // resource not found
	ExitBuild      = 20 // build failure
	ExitDeploy     = 21 // deployment failure
	ExitRollback   = 22 // rollback failure
	ExitNetwork    = 30 // network / connection failure
	ExitConfig     = 40 // configuration failure
	ExitDocker     = 50 // docker failure
	ExitTimeout    = 60 // timeout
	ExitEncryption = 70 // encryption failure
)

// isUsageError reports whether err is a command-usage error raised by cobra
// itself (arg-count / required-flag validation). These arrive as plain
// errors before any RunE body executes, so they carry no structured code;
// classifying them at the boundary keeps the 2 (usage) exit code consistent
// without touching every command's Args validator.
func isUsageError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.HasPrefix(msg, "requires at least ") &&
		strings.Contains(msg, "arg(s)") ||
		strings.HasPrefix(msg, "accepts at most ") ||
		strings.HasPrefix(msg, "accepts ") && strings.Contains(msg, "arg(s)") ||
		strings.HasPrefix(msg, "accepts between ") ||
		strings.HasPrefix(msg, `required flag(s) "`) && strings.HasSuffix(msg, `" not set`)
}

// ExitCodeFor maps a structured (or plain) error to a process exit code.
// Unknown / unwrapped errors resolve to a generic failure.
func ExitCodeFor(err error) int {
	if err == nil {
		return ExitOK
	}
	if isUsageError(err) {
		return ExitUsage
	}
	switch phelixerr.CodeOf(err) {
	case phelixerr.CodeOK:
		return ExitOK
	case phelixerr.CodeInvalidArgument, phelixerr.CodeValidation:
		return ExitUsage
	case phelixerr.CodeUnauthenticated, phelixerr.CodeInvalidCredentials, phelixerr.CodeSessionExpired:
		return ExitAuth
	case phelixerr.CodeUnauthorized, phelixerr.CodePermissionDenied:
		return ExitPermission
	case phelixerr.CodeNotFound, phelixerr.CodeVersionNotFound,
		phelixerr.CodeRollbackTargetNotFound:
		return ExitNotFound
	case phelixerr.CodeBuildFailed, phelixerr.CodeToolchainNotFound,
		phelixerr.CodeBuildTimeout, phelixerr.CodeUnsupportedProject:
		return ExitBuild
	case phelixerr.CodeDeployFailed, phelixerr.CodeInstanceStartFailed,
		phelixerr.CodeHealthCheckFailed, phelixerr.CodeDeployLocked:
		return ExitDeploy
	case phelixerr.CodeRollbackFailed:
		return ExitRollback
	case phelixerr.CodeUpdateFailed:
		return ExitFailure
	case phelixerr.CodeNetwork, phelixerr.CodeConnection, phelixerr.CodeGRPC:
		return ExitNetwork
	case phelixerr.CodeConfiguration:
		return ExitConfig
	case phelixerr.CodeDocker, phelixerr.CodeDockerDaemonUnavailable:
		return ExitDocker
	case phelixerr.CodeTimeout:
		return ExitTimeout
	case phelixerr.CodeEncryption:
		return ExitEncryption
	default:
		return ExitFailure
	}
}

// hintFor returns an optional actionable hint line for a structured error, or
// "" when there is nothing helpful to say. Hint text must never embed secrets.
func hintFor(err error) string {
	if err == nil {
		return ""
	}
	switch phelixerr.CodeOf(err) {
	case phelixerr.CodeUnauthenticated, phelixerr.CodeInvalidCredentials, phelixerr.CodeSessionExpired:
		return "Run:\n  phelix auth login"
	case phelixerr.CodeNotFound, phelixerr.CodeVersionNotFound, phelixerr.CodeRollbackTargetNotFound:
		return "Run:\n  phelix list"
	case phelixerr.CodeHealthCheckFailed:
		return "Run:\n  phelix health status <app>"
	case phelixerr.CodeDeployLocked:
		return "If this lock is stale, clear it with:\n  phelix deploy unlock <app>"
	case phelixerr.CodeToolchainNotFound:
		return "Install the required toolchain, or allow Phelix to install it automatically."
	case phelixerr.CodeUpdateFailed:
		return "Re-run:\n  phelix update\nOr reinstall with the one-line installer:\n  curl -fsSL https://phelix.anophel.com/install.sh | bash"
	case phelixerr.CodeDocker, phelixerr.CodeDockerDaemonUnavailable:
		return "Make sure Docker is installed and running, then try again."
	default:
		return ""
	}
}

// renderCLIError prints a single, consistent error block to stderr and
// returns the process exit code derived from the error's category.
//
// Normal mode shows the message, the stable code, and (when available) an
// actionable hint. Debug mode additionally reveals the root cause chain.
// Secrets are never printed, in either mode.
func renderCLIError(err error, debug bool) int {
	exitCode := ExitCodeFor(err)
	code := phelixerr.CodeOf(err)
	if isUsageError(err) {
		// Cobra's arg/flag validation errors are plain errors; present them
		// under the usage category rather than UNKNOWN so the user sees a
		// meaningful code alongside the 2 exit status.
		code = phelixerr.CodeInvalidArgument
	}
	msg := phelixerr.Redact(err.Error())
	if msg == "" {
		msg = "operation failed"
	}

	// Headline.
	fmt.Fprintf(errOut, "%s %s\n", color.RedString("Error:"), msg)
	fmt.Fprintf(errOut, "  %s %s\n", color.HiBlackString("Code:"), code)

	// Actionable hint, when we have one.
	if h := hintFor(err); h != "" {
		// Hint text may be multi-line (e.g. "Run:\n  phelix auth login").
		for _, line := range splitLines(h) {
			fmt.Fprintf(errOut, "  %s\n", color.CyanString(line))
		}
	}

	// Root cause, only in debug mode. Both message and cause pass through the
	// credential redactor so a value embedded by a lower layer never leaks,
	// even in --debug output.
	if debug {
		fmt.Fprintf(errOut, "\n%s\n", color.HiBlackString("Cause:"))
		if cause := phelixerr.Cause(err); cause != nil {
			fmt.Fprintf(errOut, "  %s\n", phelixerr.Redact(cause.Error()))
		} else {
			fmt.Fprintf(errOut, "  (none)\n")
		}
	}
	fmt.Fprintln(errOut)

	return exitCode
}

// splitLines splits a multi-line hint into individual display lines.
func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}
