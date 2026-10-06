package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/spf13/cobra"
)

var (
	logLines    int
	logNoFollow bool
	logSince    string
	logJSON     bool
)

var LogCmd = &cobra.Command{
	Use:   "log <ID|AppName>",
	Short: "Display logs for a specific application by its ID or AppName.",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// Machine mode: log lines detour to stderr so the JSON envelope stays
		// alone on stdout.
		if logJSON {
			restore := machine.EnterJSON()
			defer restore()
		}

		if err := app.Manager.LoadState(); err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load app state", err)
		}

		// --json is a bounded snapshot: a following stream would never produce
		// a terminal envelope, so machine mode implies --no-follow. Human mode
		// keeps its historical default of tailing until Ctrl+C; --no-follow
		// makes non-following explicit for scripts.
		if logJSON {
			logNoFollow = true
		}

		since, err := parseLogSince(logSince)
		if err != nil {
			return err
		}

		if len(args) == 0 {
			return showLogView(logViewSpec{
				Source: "self",
				Name:   "phelix",
				Path:   logs.SelfLogPath(),
			}, since)
		}

		appInfo, err := GetAppInfo(args[0])
		if err != nil {
			return err
		}

		// State saved by older versions may carry an empty log_file; fall back
		// to the canonical per-app path instead of failing to open "".
		logPath := appInfo.LogFile
		if logPath == "" {
			logPath = logs.AppLogPath(appInfo.ID)
		}

		return showLogView(logViewSpec{
			Source: "app",
			Name:   appInfo.Name,
			Path:   logPath,
		}, since)
	},
}

// logViewSpec names one log stream.
type logViewSpec struct {
	Source string // "app" | "self"
	Name   string
	Path   string
}

// logResult is the machine-contract log snapshot. items are redacted log
// lines; truncated reports whether the --lines bound left older lines
// unread.
type logResult struct {
	Source    string   `json:"source"`
	App       string   `json:"app,omitempty"`
	Path      string   `json:"path"`
	Items     []string `json:"items"`
	Count     int      `json:"count"`
	Truncated bool     `json:"truncated"`
}

func showLogView(spec logViewSpec, since *time.Time) error {
	f, err := os.Open(spec.Path)
	if err != nil {
		return phelixerr.Wrapf(
			phelixerr.CodeFilesystem,
			err,
			"failed to open log file %s",
			spec.Path,
		)
	}
	defer f.Close()

	lines, total, err := readLogHistory(f, logLines, since)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to read historical logs", err)
	}

	truncated := since == nil && total > len(lines)

	if machine.Active() {
		items := make([]string, 0, len(lines))
		for _, line := range lines {
			items = append(items, phelixerr.Redact(line))
		}
		return writeEnvelopeResult(machine.Success("", logResult{
			Source:    spec.Source,
			App:       spec.Name,
			Path:      spec.Path,
			Items:     items,
			Count:     len(items),
			Truncated: truncated,
		}))
	}

	fmt.Printf("Phelix: Showing logs for %s (Ctrl+C to exit)\n", spec.Name)

	// Log lines are treated as opaque text that still passes through the
	// centralized redactor: Phelix cannot vouch for what a managed app
	// prints, so credential-shaped content is masked on display (the file
	// itself is untouched). Backend "[REDACTED:*]" sentinels are normal
	// content — never parsed or treated as an error (H2,
	// docs/CLI_CHANGES_REQUIRED.md §4).
	for _, line := range lines {
		fmt.Println(phelixerr.Redact(line))
	}
	if truncated {
		fmt.Printf("  (showing last %d of %d lines — use --lines N for more)\n", len(lines), total)
	}

	if logNoFollow {
		return nil
	}
	return streamNewLogs(f)
}

// parseLogSince parses --since as either a Go duration relative to now
// ("30m", "2h") or an RFC3339 timestamp. Empty means no filter.
func parseLogSince(raw string) (*time.Time, error) {
	if raw == "" {
		return nil, nil
	}
	if d, err := time.ParseDuration(raw); err == nil {
		if d <= 0 {
			return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"invalid --since %q: duration must be positive", raw)
		}
		t := time.Now().Add(-d)
		return &t, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return &t, nil
	}
	return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument,
		"invalid --since %q: use a Go duration like 30m/2h or an RFC3339 timestamp", raw)
}

// logTimeLayout is the timestamp prefix the log writers stamp on every line
// (internal/logs: "2006/01/02 15:04:05 [LEVEL] …").
const logTimeLayout = "2006/01/02 15:04:05"

// linePredates reports whether a log line carries a parseable timestamp
// strictly before since. Lines without a parseable prefix are conservatively
// kept — an untimestamped line must not be silently dropped.
func linePredates(line string, since time.Time) bool {
	if len(line) < len(logTimeLayout) {
		return false
	}
	ts, err := time.Parse(logTimeLayout, line[:len(logTimeLayout)])
	if err != nil {
		return false
	}
	return ts.Before(since)
}

// readLogHistory returns up to n trailing lines (n <= 0 means all), plus the
// total number of lines scanned. When since is set, lines with a parseable
// timestamp before it are filtered out of the result (but still counted).
func readLogHistory(f *os.File, n int, since *time.Time) ([]string, int, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, 0, err
	}

	// Upper bound for the in-memory window when n <= 0: the log files are
	// trimmed by retention, but a scan must never buffer an unbounded file.
	const maxBufferedLines = 10000

	total := 0
	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		total++
		line := scanner.Text()
		if since != nil && linePredates(line, *since) {
			continue
		}
		lines = append(lines, line)
		if n <= 0 {
			if len(lines) > maxBufferedLines {
				lines = lines[1:]
			}
			continue
		}
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, total, err
	}
	return lines, total, nil
}

func streamNewLogs(f *os.File) error {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)
	defer signal.Stop(sigChan)

	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to seek to end of log file", err)
	}

	reader := bufio.NewReader(f)
	done := make(chan struct{})

	go func() {
		for {
			select {
			case <-done:
				return
			default:
				line, err := reader.ReadString('\n')
				if len(line) > 0 {
					// Print whatever was read, including a final partial line
					// that has not been terminated yet.
					fmt.Print(phelixerr.Redact(strings.TrimRight(line, "\n")) + "\n")
				}
				if err != nil && err != io.EOF {
					fmt.Printf("⚠ Error reading log: %v\n", err)
					return
				}
				if err == io.EOF {
					// No new data — poll again after a short interval instead
					// of busy-looping on EOF.
					time.Sleep(250 * time.Millisecond)
				}
			}
		}
	}()

	<-sigChan
	close(done)
	fmt.Println("\nGoodbye!")
	return nil
}

func init() {
	LogCmd.Flags().IntVar(&logLines, "lines", 10,
		"Number of historical lines to show before following (human mode) or returning (with --json)")
	LogCmd.Flags().BoolVar(&logNoFollow, "no-follow", false,
		"Print the historical lines and exit instead of tailing (the human default still tails)")
	LogCmd.Flags().StringVar(&logSince, "since", "",
		"Only show lines logged after this point: a Go duration (30m, 2h) or RFC3339 timestamp")
	LogCmd.Flags().BoolVar(&logJSON, "json", false,
		"Output a bounded machine-readable snapshot (implies --no-follow; cannot be combined with following)")
}
