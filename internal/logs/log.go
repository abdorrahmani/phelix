package logs

import (
	"bufio"
	"io"
	"os"
)

// NewGenericLogCollector creates a reusable log collector.
func NewGenericLogCollector(maxLines, coldLines int) *GenericLogCollector {
	return &GenericLogCollector{
		readers:   make(map[string]*readerState),
		maxLines:  maxLines,
		ColdLines: coldLines,
	}
}

// Collect reads new log lines from a given file key.
// key must be unique per logical log source (appID, serverID, etc).
func (c *GenericLogCollector) Collect(key, filepath string, mapper func(string) LogEntry) ([]LogEntry, error) {
	c.mu.Lock()
	rs := c.readers[key]
	c.mu.Unlock()

	if rs == nil {
		f, err := os.Open(filepath)
		if err != nil {
			return nil, err
		}

		rs = &readerState{
			file:    f,
			reader:  bufio.NewReader(f),
			lastPos: 0,
		}

		if off, err := findOffsetForLastNLines(f, c.ColdLines); err == nil {
			rs.lastPos = off
		}

		c.mu.Lock()
		c.readers[key] = rs
		c.mu.Unlock()
	}

	if _, err := rs.file.Seek(rs.lastPos, io.SeekStart); err != nil {
		return nil, err
	}
	rs.reader.Reset(rs.file)

	var out []LogEntry
	lines := 0

	for lines < c.maxLines {
		line, err := rs.reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}

		pos, _ := rs.file.Seek(0, io.SeekCurrent)
		rs.lastPos = pos

		out = append(out, mapper(trimTrailingNewline(line)))
		lines++
	}

	return out, nil
}

// findOffsetForLastNLines finds the byte offset in the file where the last N lines begin.
func findOffsetForLastNLines(f *os.File, n int) (int64, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if info.Size() == 0 {
		return 0, nil
	}

	var count int
	var pos = info.Size() - 1
	buf := make([]byte, 1)
	for pos >= 0 {
		if _, err := f.ReadAt(buf, pos); err != nil {
			return 0, err
		}
		if buf[0] == '\n' {
			count++
			if count >= n+1 { // position to start of last N lines
				return pos + 1, nil
			}
		}
		pos--
	}
	return 0, nil
}

// trimTrailingNewline removes trailing newline characters (\n or \r) from the end of a string.
func trimTrailingNewline(s string) string {
	if len(s) == 0 {
		return s
	}
	if s[len(s)-1] == '\n' || s[len(s)-1] == '\r' {
		return trimTrailingNewline(s[:len(s)-1])
	}
	return s
}

// trimLogFile reduces the size of the log file if it exceeds MaxLogSize.
func trimLogFile(logFile string) error {
	file, err := os.Open(logFile)
	if err != nil {
		return err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return err
	}

	if info.Size() <= MaxLogSize {
		return nil
	}

	start := info.Size() - MaxLogSize
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return err
	}

	tempPath := logFile + ".tmp"
	tempFile, err := os.Create(tempPath)
	if err != nil {
		return err
	}
	defer tempFile.Close()

	_, err = io.Copy(tempFile, file)
	if err != nil {
		return err
	}

	return os.Rename(tempPath, logFile)
}
