package cmd

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/spf13/cobra"
)

// maxBufferedLines bounds the in-memory history window when no --lines bound
// is given: retention archives mean a scan can span several files.
const maxBufferedLines = 10000

var (
	logLines    int
	logNoFollow bool
	logSince    string
	logJSON     bool
	logLevel    string
	logVersion  string
	logApp      string
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

		if len(args) > 0 && logApp != "" && args[0] != logApp {
			return phelixerr.New(phelixerr.CodeInvalidArgument,
				"use either the positional app argument or --app, not both")
		}
		if len(args) == 0 && logApp != "" {
			args = []string{logApp}
		}

		if logLevel != "" || logVersion != "" {
			if !logs.LogStoreEnabled() {
				return phelixerr.New(phelixerr.CodeInvalidArgument,
					"--level/--version query the long-term store, which is disabled; "+
						"start the monitor daemon with PHELIX_LOG_STORE=1 to enable it")
			}
			return showLogStoreQuery(args, since)
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
	history, err := readLogHistoryFiles(spec.Path, logLines, since)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to read historical logs", err)
	}
	lines, total := history.lines, history.total

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
	return streamNewLogs(f)
}

// logHistory is the result of scanning one log path's history: the retained
// lines (newest last) and the total number of lines scanned.
type logHistory struct {
	lines []string
	total int
}

// readLogHistoryFiles scans the full history of a log path: the gzip
// archives and plain rotations first (oldest to newest), then the active
// file. Rotation archives are transparently decompressed so `phelix log`
// covers everything the on-disk retention still holds.
func readLogHistoryFiles(path string, n int, since *time.Time) (logHistory, error) {
	var h logHistory
	paths := logs.LogHistoryPaths(path, 0)
	if len(paths) == 0 {
		f, err := os.Open(path)
		if err != nil {
			return h, err
		}
		f.Close()
		return h, nil
	}
	for _, p := range paths {
		rc, err := openMaybeGz(p)
		if err != nil {
			return h, err
		}
		lines, total, err := readLogHistory(rc, n, since)
		rc.Close()
		if err != nil {
			return h, err
		}
		h.lines = append(h.lines, lines...)
		h.total += total
	}
	if n > 0 && len(h.lines) > n {
		h.lines = h.lines[len(h.lines)-n:]
	}
	return h, nil
}

// openMaybeGz opens a history path, transparently decompressing gzip
// archives.
func openMaybeGz(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if !logs.IsGzipPath(path) {
		return f, nil
	}
	gr, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return readCloser{Reader: gr, closer: f}, nil
}

// readCloser pairs a gzip reader with the file underneath it so one Close
// releases both.
type readCloser struct {
	io.Reader
	closer io.Closer
}

func (rc readCloser) Close() error { return rc.closer.Close() }

// logStoreResult is the machine-contract result of a store query.
type logStoreResult struct {
	App     string           `json:"app,omitempty"`
	Level   string           `json:"level,omitempty"`
	Version string           `json:"version,omitempty"`
	Count   int              `json:"count"`
	Items   []logs.LogRecord `json:"items"`
}

// showLogStoreQuery runs the long-term store query. The positional argument
// (or --app) narrows the query to one app; without it, every app's records
// are searched. --lines doubles as the result cap.
func showLogStoreQuery(args []string, since *time.Time) error {
	appName := ""
	if len(args) > 0 {
		appInfo, err := GetAppInfo(args[0])
		if err != nil {
			return err
		}
		appName = appInfo.Name
	}

	s, err := logs.OpenLogStore()
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to open the log store", err)
	}
	defer s.Close()

	records, err := s.Query(appName, logs.LogLevel(logLevel), logVersion, "", sinceOrZero(since), logLines)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "log store query failed", err)
	}

	if machine.Active() {
		return writeEnvelopeResult(machine.Success("", logStoreResult{
			App:     appName,
			Level:   logLevel,
			Version: logVersion,
			Count:   len(records),
			Items:   records,
		}))
	}

	for _, rec := range records {
		fmt.Println(rec.TextLine())
	}
	if len(records) == 0 {
		fmt.Println("  (no matching records in the long-term store)")
	}
	return nil
}

// sinceOrZero maps a nil since to the zero time the store query expects.
func sinceOrZero(since *time.Time) time.Time {
	if since == nil {
		return time.Time{}
	}
	return *since
}

// parseLogSince parses --since as either a Go duration relative to now
// ("30m", "2h"), a day count ("30d"), or an RFC3339 timestamp. Empty means
// no filter.
func parseLogSince(raw string) (*time.Time, error) {
	if raw == "" {
		return nil, nil
	}
	if strings.HasSuffix(raw, "d") {
		if days, err := strconv.Atoi(strings.TrimSuffix(raw, "d")); err == nil && days > 0 {
			t := time.Now().AddDate(0, 0, -days)
			return &t, nil
		}
		return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"invalid --since %q: day counts look like 30d", raw)
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
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "{") {
		var rec logs.LogRecord
		if err := json.Unmarshal([]byte(trimmed), &rec); err == nil {
			if ts := logs.ParseRecordTime(rec.TS); !ts.IsZero() {
				return ts.Before(since)
			}
		}
	}
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
func readLogHistory(r io.Reader, n int, since *time.Time) ([]string, int, error) {
	total := 0
	var lines []string
	scanner := bufio.NewScanner(r)
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
	LogCmd.Flags().StringVar(&logLevel, "level", "",
		"Query the long-term store for a level (debug, info, warn, error); requires the monitor to run with PHELIX_LOG_STORE=1")
	LogCmd.Flags().StringVar(&logVersion, "version", "",
		"Query the long-term store for one serving version (e.g. v18); requires the store")
	LogCmd.Flags().StringVar(&logApp, "app", "",
		"App name for the store query (alternative to the positional argument)")
}
