package logs

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// runDockerDrain replays interrupted output before releasing its credentials.
func runDockerDrain(id string, dst, ready *os.File) int {
	var cursors [2]time.Time
	var mu sync.Mutex
	for {
		since := cursors[0]
		if cursors[1].Before(since) {
			since = cursors[1]
		}
		args := []string{"logs", "--follow", "--timestamps"}
		if !since.IsZero() {
			args = append(args, "--since", since.Add(-time.Second).Format(time.RFC3339Nano))
		}
		command := exec.Command("docker", append(args, id)...)
		stdout, err := command.StdoutPipe()
		if err != nil {
			return 1
		}
		stderr, err := command.StderrPipe()
		if err != nil {
			stdout.Close()
			return 1
		}
		if err := command.Start(); err != nil {
			stdout.Close()
			stderr.Close()
			if ready != nil {
				return 1
			}
			time.Sleep(time.Second)
			continue
		}
		if ready != nil {
			if _, err := ready.Write([]byte{1}); err != nil {
				_ = command.Process.Kill()
				_ = command.Wait()
				return 1
			}
			ready.Close()
			ready = nil
		}
		var wg sync.WaitGroup
		failed := false
		writers := [2]*AppLogWriter{}
		for index, stream := range []struct {
			r    io.Reader
			name LogStream
		}{{stdout, StreamStdout}, {stderr, StreamStderr}} {
			wg.Go(func() {
				reader := bufio.NewReaderSize(stream.r, 32<<10)
				writer := NewAppLogWriter(dst, stream.name)
				writers[index] = writer
				var stampTime time.Time
				first, keep := true, false
				for {
					part, err := reader.ReadSlice('\n')
					mu.Lock()
					if first {
						keep = false
						stamp, rest, ok := strings.Cut(string(part), " ")
						if at, parseErr := time.Parse(time.RFC3339Nano, stamp); ok && parseErr == nil {
							stampTime = at
							part = []byte(rest)
							keep = true
						}
					}
					if !keep {
						part = nil
					}
					if keep && len(part) > 0 && part[len(part)-1] == '\n' && stampTime.After(cursors[index]) {
						cursors[index] = stampTime
					}
					if _, err := writer.Write(part); err != nil {
						failed = true
					}
					mu.Unlock()
					first = err != bufio.ErrBufferFull
					if err != nil && err != bufio.ErrBufferFull {
						break
					}
				}
			})
		}
		wg.Wait()
		_ = command.Wait()
		if err := dst.Sync(); err != nil || failed {
			return 1
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		inspect := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.State.Running}}", id)
		output, err := inspect.Output()
		cancel()
		exited := err == nil && strings.TrimSpace(string(output)) == "false"
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && (strings.Contains(string(exitErr.Stderr), "No such object") || strings.Contains(string(exitErr.Stderr), "No such container")) {
			exited = true
		}
		if exited {
			for _, writer := range writers {
				if err := writer.Close(); err != nil {
					return 1
				}
			}
			if err := dst.Sync(); err != nil {
				return 1
			}
			time.Sleep(200 * time.Millisecond)
			return 0
		}
		time.Sleep(time.Second)
	}
}
