package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/spf13/cobra"
)

var LogCmd = &cobra.Command{
	Use:   "log <ID|AppName>",
	Short: "Display logs for a specific application by its ID or AppName.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		identifier := args[0]

		if err := app.Manager.LoadState(); err != nil {
			return fmt.Errorf("⚠ Failed to load app state: %v", err)
		}

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

func displayLogs(appInfo *app.AppInfo) error {
	f, err := os.Open(appInfo.LogFile)
	if err != nil {
		return fmt.Errorf("⚠ Failed to open log file %s: %v", appInfo.LogFile, err)
	}
	defer f.Close()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)

	fmt.Printf("Gophel: Showing logs for application %s (press Ctrl+C to exit)...\n", appInfo.ID)

	if err := displayHistoricalLogs(f); err != nil {
		return err
	}

	if err := streamNewLogs(f, sigChan); err != nil {
		return err
	}

	fmt.Println("\nGophel bye!")
	return nil
}

func displayHistoricalLogs(f *os.File) error {
	lastLines, err := getLastNLines(f, 10)
	if err != nil {
		return fmt.Errorf("⚠ Failed to read historical logs: %v", err)
	}

	for _, line := range lastLines {
		fmt.Println(line)
	}

	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("⚠ Failed to seek to end of log file: %v", err)
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
