package logs

import (
	"compress/gzip"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultRotationMaxSizeMB = 100
	defaultRotationMaxFiles  = 5
)

// rotatedSizes remembers the size of each plain rotated file at the previous
// check. A rotated file whose size is stable across two consecutive checks is
// considered quiet and only then compressed: a managed app keeps its stdout
// file descriptor open across the rename, so an actively written rotation
// must stay plain and readable for a while before it is archived.
var (
	rotatedMu     sync.Mutex
	rotatedSizes  = make(map[string]int64)
	rotatedQuiet  = make(map[string]bool)
	rotatedWrites = make(map[string]time.Time)
)

// RotateLogFile rotates path when it exceeds maxSizeMB: the active file is
// renamed to path.1, an empty active file takes its place, older rotations
// shift up to path.N and the oldest beyond maxFiles is deleted. It reports
// whether a rotation actually happened. Rotated files are gzip-compressed
// only after they have gone quiet, because the app that owns the open
// descriptor keeps writing into the renamed file; compression of a live
// rotation would detach that descriptor from any readable path and lose the
// lines written afterwards. Errors on individual rotated files are skipped:
// rotation must never fail because an archive could not move.
func RotateLogFile(path string, maxSizeMB, maxFiles int, compress bool) (bool, error) {
	if maxSizeMB <= 0 {
		maxSizeMB = defaultRotationMaxSizeMB
	}
	if maxFiles <= 0 {
		maxFiles = defaultRotationMaxFiles
	}
	if maxFiles < 1 {
		maxFiles = 1
	}

	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		compressQuietRotated(path, compress, maxFiles)
		return false, nil
	case err != nil:
		return false, err
	case info.Size() <= int64(maxSizeMB)<<20:
		compressQuietRotated(path, compress, maxFiles)
		return false, nil
	}

	oldest := rotatedPath(path, maxFiles)
	_ = os.Remove(oldest)
	_ = os.Remove(oldest + ".gz")

	for i := maxFiles - 1; i >= 1; i-- {
		_ = os.Rename(rotatedPath(path, i)+".gz", rotatedPath(path, i+1)+".gz")
		_ = os.Rename(rotatedPath(path, i), rotatedPath(path, i+1))
	}

	if err := os.Rename(path, rotatedPath(path, 1)); err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err == nil {
		f.Close()
	}

	compressQuietRotated(path, compress, maxFiles)
	return true, nil
}

// rotatedPath is the Nth rotation of the active file.
func rotatedPath(path string, n int) string {
	return path + "." + strconv.Itoa(n)
}

// compressQuietRotated gzips every plain rotated file of path that has been
// observed at the same size across two consecutive checks. Files that grew
// since the last check are still being written and stay plain.
func compressQuietRotated(path string, compress bool, maxFiles int) {
	rotatedMu.Lock()
	defer rotatedMu.Unlock()

	now := time.Now()
	for i := 1; i <= maxFiles; i++ {
		plain := rotatedPath(path, i)
		info, err := os.Stat(plain)
		if err != nil {
			delete(rotatedSizes, plain)
			rotatedQuiet[plain] = false
			continue
		}
		if _, err := os.Stat(plain + ".gz"); err == nil {
			continue
		}

		last, seen := rotatedSizes[plain]
		rotatedSizes[plain] = info.Size()
		if !compress {
			continue
		}
		if !seen || last != info.Size() {
			rotatedQuiet[plain] = false
			rotatedWrites[plain] = now
			continue
		}
		if rotatedQuiet[plain] {
			continue
		}
		if checked, ok := rotatedWrites[plain]; ok && now.Sub(checked) < time.Minute {
			continue
		}
		if err := gzipFile(plain); err != nil {
			continue
		}
		rotatedQuiet[plain] = true
		delete(rotatedSizes, plain)
	}
}

// gzipFile compresses path to path.gz and removes the original.
func gzipFile(path string) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.Create(path + ".gz")
	if err != nil {
		return err
	}

	w := gzip.NewWriter(dst)
	if _, err := io.Copy(w, src); err != nil {
		dst.Close()
		return err
	}
	if err := w.Close(); err != nil {
		dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	return os.Remove(path)
}

// PlainRotatedPaths lists the rotated files of path that exist as plain
// (uncompressed) files, oldest first. The collector reads them alongside the
// active file: after a rotation the app keeps writing into the renamed file
// until it restarts, so stopping at the active file would lose live output.
func PlainRotatedPaths(path string, maxFiles int) []string {
	if maxFiles <= 0 {
		maxFiles = defaultRotationMaxFiles
	}
	var out []string
	for i := 1; i <= maxFiles; i++ {
		p := rotatedPath(path, i)
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
			out = append(out, p)
		}
	}
	return out
}

// LogHistoryPaths lists every readable file holding the history of path,
// oldest first: the gzip archives, then the plain rotations, then the active
// file. Consumers that tail the newest lines read the list in order with a
// sliding window.
func LogHistoryPaths(path string, maxFiles int) []string {
	if maxFiles <= 0 {
		maxFiles = defaultRotationMaxFiles
	}
	var out []string
	for i := maxFiles; i >= 1; i-- {
		gz := rotatedPath(path, i) + ".gz"
		if _, err := os.Stat(gz); err == nil {
			out = append(out, gz)
			continue
		}
		p := rotatedPath(path, i)
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	if _, err := os.Stat(path); err == nil {
		out = append(out, path)
	}
	return out
}

// IsGzipPath reports whether a history path is a gzip archive.
func IsGzipPath(path string) bool {
	return strings.HasSuffix(path, ".gz")
}
