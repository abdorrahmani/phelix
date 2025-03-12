package cmd

import (
	"bufio"
	"fmt"
	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/spf13/cobra"
	"io"
	"os"
	"os/signal"
	"time"
)

var LogCmd = &cobra.Command{
	Use:   "log <ID>",
	Short: "Display logs for a specific application by its ID",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		id := args[0]

		if err := app.Manager.LoadState(); err != nil {
			fmt.Printf("Failed to load app state: %v", err)
			return
		}

		appInfo, exists := app.Manager.(*app.AppManager).Apps[id]
		if !exists {
			fmt.Printf("Application %s not found\n", id)
			return
		}

		logFile := appInfo.LogFile
		if logFile == "" {
			fmt.Printf("No log file configured for application %s\n", id)
			return
		}

		f, err := os.Open(logFile)
		if err != nil {
			fmt.Printf("Failed to open log file %s: %v\n", logFile, err)
			return
		}

		defer f.Close()

		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt)

		fmt.Printf("Gophel: Showing logs for application %s (press Ctrl+C to exit)...\n", id)

		lastLines, err := getLastNLines(f, 10)
		if err != nil {
			fmt.Printf("Failed to read historical logs: %v\n", err)
			return
		}
		for _, line := range lastLines {
			fmt.Println(line)
		}

		_, err = f.Seek(0, io.SeekEnd)
		if err != nil {
			fmt.Printf("Failed to seek to end of log file: %v\n", err)
			return
		}

		reader := bufio.NewReader(f)
		go func() {
			for {
				line, err := reader.ReadString('\n')
				if err == nil {
					fmt.Print(line)
				} else if err.Error() != "EOF" {
					fmt.Printf("Error reading log: %v\n", err)
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
		}()

		<-sigChan
		fmt.Println("\nGophel bye!")

	},
}

// getLastNLines reads the last N lines from a file
func getLastNLines(file *os.File, n int) ([]string, error) {
	// Reset file pointer to start
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
