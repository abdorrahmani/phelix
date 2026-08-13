package monitor

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"

	"github.com/abdorrahmani/phelix/internal/app"
)

// Command executor implementation
type appCommandExecutor struct{}

// NewCommandExecutor returns the default CommandExecutor implementation. It
// is transport-agnostic and can be reused by any monitoring transport (gRPC
// today, previously WebSocket).
func NewCommandExecutor() CommandExecutor {
	return &appCommandExecutor{}
}

func (e *appCommandExecutor) Execute(cmd Command) error {
	apps := app.Manager.ListApplications()
	var targetAppID string
	var targetAppName string

	// First try to find by app name
	for _, app := range apps {
		if app.Name == cmd.Payload.AppName {
			targetAppID = app.ID
			targetAppName = app.Name
			break
		}
	}

	// If not found by name, try to use the appName as ID directly
	if targetAppID == "" {
		// Check if the appName is actually an ID
		for _, app := range apps {
			if app.ID == cmd.Payload.AppName {
				targetAppID = app.ID
				targetAppName = app.Name
				break
			}
		}
	}

	if targetAppID == "" {
		return fmt.Errorf("app not found: %s", cmd.Payload.AppName)
	}

	log.Printf("Executing command '%s' for app '%s' (ID: %s)", cmd.Payload.Type, targetAppName, targetAppID)

	// Find the phelix executable in PATH
	phelixPath, err := exec.LookPath("phelix")
	if err != nil {
		return fmt.Errorf("failed to find phelix executable: %v", err)
	}

	// Create the command with the correct format: phelix commandName ID
	execCmd := exec.Command(phelixPath, cmd.Payload.Type, targetAppID)

	// Set up environment with Go variables
	env := os.Environ()
	env = append(env,
		"GOROOT=/usr/local/go",
		"GOPATH="+os.Getenv("HOME")+"/go",
	)

	// Add Go paths to PATH
	path := os.Getenv("PATH")
	goRoot := "/usr/local/go/bin"
	goPath := os.Getenv("HOME") + "/go/bin"
	env = append(env, "PATH="+goRoot+":"+goPath+":"+path)

	execCmd.Env = env

	// Set working directory to the current directory
	execCmd.Dir = "."

	// Create pipes for stdout and stderr
	stdout, err := execCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %v", err)
	}
	stderr, err := execCmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %v", err)
	}

	// Start the command
	if err := execCmd.Start(); err != nil {
		return fmt.Errorf("failed to start command: %v", err)
	}

	// Create channels to handle output
	stdoutDone := make(chan struct{})
	stderrDone := make(chan struct{})

	// Handle stdout
	go func() {
		defer close(stdoutDone)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			log.Printf("[Command stdout] %s", scanner.Text())
		}
	}()

	// Handle stderr
	go func() {
		defer close(stderrDone)
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			log.Printf("[Command stderr] %s", scanner.Text())
		}
	}()

	// Wait for command to complete
	if err := execCmd.Wait(); err != nil {
		return fmt.Errorf("command failed: %v", err)
	}

	// Wait for output handling to complete
	<-stdoutDone
	<-stderrDone

	return nil
}
