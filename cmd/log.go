package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/spf13/cobra"
)

var LogCmd = &cobra.Command{
	Use:   "log <ID|AppName>",
	Short: "Display logs for a specific application by its ID or AppName.",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := app.Manager.LoadState(); err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load app state", err)
		}

		if len(args) == 0 {
			return displaySelfLogs()
		}

		identifier := args[0]

		appInfo, err := GetAppInfo(identifier)
		if err != nil {
			return err
		}

		if err := displayLogs(appInfo); err != nil {
			return err
		}

		return nil
	},
}

func displaySelfLogs() error {
	// Resolve through the logs package so this matches where the daemon
	// actually writes (PHELIX_DATA_DIR / docker / $HOME), not a hardcoded
	// $HOME path.
	path := logs.SelfLogPath()

	f, err := os.Open(path)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to open log file", err)
	}
	defer f.Close()

	return streamLogs("phelix", f)
}

func displayLogs(appInfo *app.AppInfo) error {
	// State saved by older versions may carry an empty log_file; fall back to
	// the canonical per-app path instead of failing to open "".
	logPath := appInfo.LogFile
	if logPath == "" {
		logPath = logs.AppLogPath(appInfo.ID)
	}

	f, err := os.Open(logPath)
	if err != nil {
		return phelixerr.Wrapf(
			phelixerr.CodeFilesystem,
			err,
			"failed to open log file %s",
			logPath,
		)
	}
	defer f.Close()

	return streamLogs(appInfo.Name, f)
}

func streamLogs(name string, f *os.File) error {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)
	defer signal.Stop(sigChan)

	fmt.Printf("Phelix: Showing logs for %s (Ctrl+C to exit)\n", name)

	if err := displayHistoricalLogs(f); err != nil {
		return err
	}

	if err := streamNewLogs(f, sigChan); err != nil {
		return err
	}

	fmt.Println("\nGoodbye!")
	return nil
}

func displayHistoricalLogs(f *os.File) error {
	// Log lines are treated as opaque text: the CLI's own redaction (and the
	// backend's, for anything that round-tripped through the dashboard) may
	// leave "[REDACTED*]"-style sentinels in the history. They are normal
	// content here — never parsed, un-redacted, or treated as an error
	// condition (H2, docs/CLI_CHANGES_REQUIRED.md §4).
	lastLines, err := getLastNLines(f, 10)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to read historical logs", err)
	}

	for _, line := range lastLines {
		fmt.Println(line)
	}

	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to seek to end of log file", err)
	}

	return nil
}

func streamNewLogs(f *os.File, sigChan chan os.Signal) error {
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
					fmt.Print(line)
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
	return nil
}

func getLastNLines(file *os.File, n int) ([]string, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	scanner := bufio.NewScanner(file)
	lines := make([]string, 0, n)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return lines, nil
}
