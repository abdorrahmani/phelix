package monitor

import (
	"bufio"
	"log"
	"os"
	"os/exec"
)

// newPhelixCommand builds the `phelix <type> <id>` subprocess for command types
// the app manager does not handle in-process. It returns nil if the phelix
// executable cannot be located in PATH.
func newPhelixCommand(cmdType, appID string) (*exec.Cmd, error) {
	// Find the phelix executable in PATH
	phelixPath, err := exec.LookPath("phelix")
	if err != nil {
		return nil, err
	}

	execCmd := exec.Command(phelixPath, cmdType, appID)

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
	execCmd.Dir = "."

	return execCmd, nil
}

// runPhelixCommand pipes the subprocess's stdout/stderr to the daemon log and
// waits for it to finish. Any captured output is surfaced so backend-issued
// commands remain diagnosable through journalctl / phelix.log.
func runPhelixCommand(execCmd *exec.Cmd) error {
	stdout, err := execCmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := execCmd.StderrPipe()
	if err != nil {
		return err
	}

	if err := execCmd.Start(); err != nil {
		return err
	}

	stdoutDone := make(chan struct{})
	stderrDone := make(chan struct{})

	go func() {
		defer close(stdoutDone)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			log.Printf("[Command stdout] %s", scanner.Text())
		}
	}()
	go func() {
		defer close(stderrDone)
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			log.Printf("[Command stderr] %s", scanner.Text())
		}
	}()

	if err := execCmd.Wait(); err != nil {
		return err
	}

	<-stdoutDone
	<-stderrDone

	return nil
}
