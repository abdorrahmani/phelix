package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
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
	path := filepath.Join(os.Getenv("HOME"), ".phelix", "logs", "phelix.log")

	f, err := os.Open(path)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to open log file", err)
	}
	defer f.Close()

	return streamLogs("phelix", f)
}

func displayLogs(appInfo *app.AppInfo) error {
	f, err := os.Open(appInfo.LogFile)
	if err != nil {
		return phelixerr.Wrapf(
			phelixerr.CodeFilesystem,
			err,
			"failed to open log file %s",
			appInfo.LogFile,
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
				if err == nil {
					fmt.Print(line)
				} else if err != io.EOF {
					fmt.Printf("⚠ Error reading log: %v\n", err)
					return
				}
				time.Sleep(50 * time.Millisecond)
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
