package cmd

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/update"
	"github.com/abdorrahmani/phelix/internal/version"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var updateCheckOnly bool

// UpdateCmd updates the installed Phelix binary to the latest stable
// release using the same release server and layout as install.sh.
var UpdateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update Phelix to the latest release",
	Long: `Update the Phelix CLI/agent binary to the latest stable release.

The command resolves the latest release from the same release server the
one-line installer uses, downloads the matching prebuilt binary for this
platform, verifies its SHA-256 checksum when the release publishes one,
validates it, and atomically replaces the currently installed executable.

On Linux, when the phelix.service systemd unit is running, the monitor
service is restarted after the binary is replaced (managed applications
keep running). Application state under ~/.phelix/ is never touched.

Use --check to only report whether an update is available.`,
	Args: cobra.NoArgs,
	RunE: func(cobraCmd *cobra.Command, args []string) error {
		return runUpdate()
	},
}

func init() {
	UpdateCmd.Flags().BoolVar(&updateCheckOnly, "check", false, "Check for an update without installing it")
}

func runUpdate() error {
	baseURL, err := releaseBaseURL()
	if err != nil {
		return err
	}

	_, err = update.Run(update.Options{
		BaseURL:        baseURL,
		CurrentVersion: version.Version,
		CheckOnly:      updateCheckOnly,
		StdinIsTTY:     IsInteractive(),
		Steps:          printUpdateStep,
	})
	return err
}

// releaseBaseURL returns the release server base URL. It defaults to the
// public host shared with install.sh and can be overridden with
// PHELIX_RELEASE_BASE_URL for staging mirrors and tests. HTTPS is enforced
// except for loopback hosts, which local tests use.
func releaseBaseURL() (string, error) {
	raw := strings.TrimSpace(os.Getenv("PHELIX_RELEASE_BASE_URL"))
	if raw == "" {
		return update.DefaultBaseURL, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", phelixerr.Newf(phelixerr.CodeInvalidArgument, "invalid PHELIX_RELEASE_BASE_URL %q", raw)
	}
	if parsed.Scheme != "https" && !isLoopbackHost(parsed.Hostname()) {
		return "", phelixerr.Newf(phelixerr.CodeInvalidArgument, "PHELIX_RELEASE_BASE_URL must use HTTPS (got %q)", raw)
	}
	return strings.TrimSuffix(raw, "/"), nil
}

func isLoopbackHost(host string) bool {
	return host == "localhost" ||
		strings.HasPrefix(host, "127.") ||
		strings.HasPrefix(host, "[::1]") ||
		host == "::1"
}

// printUpdateStep renders orchestrator progress with the CLI's symbol
// conventions (▸ info, ✓ success, ⚠ warning).
func printUpdateStep(kind update.StepKind, format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	switch kind {
	case update.StepSuccess:
		fmt.Println(color.GreenString("✓ " + line))
	case update.StepWarn:
		fmt.Println(color.YellowString("⚠ " + line))
	default:
		fmt.Println(color.BlueString("▸ " + line))
	}
}
