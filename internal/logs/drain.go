package logs

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

const drainArg = "--phelix-internal-log-drain"

// init handles the private drain process before any commands or services start.
func init() {
	if len(os.Args) == 2 && os.Args[1] == drainArg {
		os.Exit(runDrain())
	}
}

// AttachDrain gives detached apps a redacted log sink that survives CLI exit.
func AttachDrain(cmd *exec.Cmd, dst *os.File) (func(), error) {
	values := phelixerr.SecretValuesInEnv(cmd.Env)
	if len(values) == 0 {
		cmd.Stdout, cmd.Stderr = dst, dst
		return func() {}, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	var files []*os.File
	closeFiles := func() {
		for _, f := range files {
			_ = f.Close()
		}
	}
	pipe := func() (*os.File, *os.File, error) {
		r, w, err := os.Pipe()
		if err == nil {
			files = append(files, r, w)
		}
		return r, w, err
	}
	stdoutR, stdoutW, err := pipe()
	if err != nil {
		return nil, err
	}
	stderrR, stderrW, err := pipe()
	if err != nil {
		closeFiles()
		return nil, err
	}
	configR, configW, err := pipe()
	if err != nil {
		closeFiles()
		return nil, err
	}
	readyR, readyW, err := pipe()
	if err != nil {
		closeFiles()
		return nil, err
	}
	helper := exec.Command(exe, drainArg)
	helper.ExtraFiles = []*os.File{stdoutR, stderrR, dst, configR, readyW}
	helper.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := helper.Start(); err != nil {
		closeFiles()
		return nil, err
	}
	go func() { _ = helper.Wait() }()
	_ = stdoutR.Close()
	_ = stderrR.Close()
	_ = configR.Close()
	_ = readyW.Close()
	_ = configW.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(configW).Encode(values); err != nil {
		closeFiles()
		_ = helper.Process.Kill()
		return nil, err
	}
	_ = configW.Close()
	_ = readyR.SetReadDeadline(time.Now().Add(5 * time.Second))
	var ready [1]byte
	if _, err := io.ReadFull(readyR, ready[:]); err != nil || ready[0] != 1 {
		closeFiles()
		_ = helper.Process.Kill()
		return nil, phelixerr.New(phelixerr.CodeProcessFailed, "cannot start secure log drain")
	}
	_ = readyR.Close()
	cmd.Stdout, cmd.Stderr = stdoutW, stderrW
	return sync.OnceFunc(func() { _ = stdoutW.Close(); _ = stderrW.Close() }), nil
}

// runDrain holds values until both streams reach EOF and their final lines are written.
func runDrain() int {
	stdout := os.NewFile(3, "stdout")
	stderr := os.NewFile(4, "stderr")
	dst := os.NewFile(5, "log")
	config := os.NewFile(6, "values")
	ready := os.NewFile(7, "ready")
	defer stdout.Close()
	defer stderr.Close()
	defer dst.Close()
	defer config.Close()
	defer ready.Close()
	var values []string
	if err := json.NewDecoder(config).Decode(&values); err != nil {
		return 1
	}
	for _, value := range values {
		phelixerr.RegisterSecretValue(value)
	}
	if _, err := ready.Write([]byte{1}); err != nil {
		return 1
	}
	_ = ready.Close()
	var wg sync.WaitGroup
	var mu sync.Mutex
	failed := false
	for _, stream := range []struct {
		f    *os.File
		name LogStream
	}{{stdout, StreamStdout}, {stderr, StreamStderr}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reader := bufio.NewReaderSize(stream.f, 32<<10)
			writer := NewAppLogWriter(dst, stream.name)
			for {
				part, err := reader.ReadSlice('\n')
				mu.Lock()
				_, writeErr := writer.Write(part)
				if writeErr != nil {
					failed = true
				}
				mu.Unlock()
				if err != nil && err != bufio.ErrBufferFull {
					break
				}
			}
			mu.Lock()
			if err := writer.Close(); err != nil {
				failed = true
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if err := dst.Sync(); err != nil {
		return 1
	}
	if failed {
		return 1
	}
	return 0
}
