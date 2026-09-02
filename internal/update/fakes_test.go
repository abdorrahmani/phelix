package update

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// --- Shared test fakes -------------------------------------------------------

// fakeRunner is a CommandRunner fake driven by queued responses keyed by the
// full command line ("name arg1 arg2"). Each key pops one response per call;
// the last response for a key repeats. Unlisted commands fail, which is what
// "tool not installed" looks like.
type fakeRunner struct {
	mu         sync.Mutex
	calls      []string
	responses  map[string][]fakeResp
	responseFn func(command string, callNum int) (string, error)
}

type fakeResp struct {
	out string
	err error
}

func (f *fakeRunner) Run(name string, args ...string) (string, error) {
	command := strings.Join(append([]string{name}, args...), " ")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, command)
	n := 0
	for _, c := range f.calls {
		if c == command {
			n++
		}
	}
	if f.responseFn != nil {
		return f.responseFn(command, n)
	}
	if q, ok := f.responses[command]; ok && len(q) > 0 {
		r := q[len(q)-1]
		if n <= len(q) {
			r = q[n-1]
		}
		return r.out, r.err
	}
	return "", fmt.Errorf("exec: %q: not found", command)
}

func (f *fakeRunner) hasCall(command string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == command {
			return true
		}
	}
	return false
}

// stepRecorder captures progress lines rendered with their kind.
type stepRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (s *stepRecorder) record(kind StepKind, format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, fmt.Sprintf("[%d] %s", kind, fmt.Sprintf(format, args...)))
}

func (s *stepRecorder) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}

func (s *stepRecorder) contains(substr string) bool {
	for _, l := range s.all() {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

// buildPhelixRelease builds the real phelix binary once per test run, stamped
// with a known release version. Its bytes serve as the release artifact in
// download tests: it is a genuine Go binary from this module, so it passes
// ValidateReleaseFile exactly like a real release would.
var (
	releaseBuildOnce sync.Once
	releaseBinPath   string
	releaseBuildErr  error
)

const releaseTestVersion = "9.9.9"

func buildPhelixRelease(t *testing.T) []byte {
	t.Helper()
	releaseBuildOnce.Do(func() {
		repoRoot, err := filepath.Abs("../..")
		if err != nil {
			releaseBuildErr = err
			return
		}
		dir, err := os.MkdirTemp("", "phelix-release")
		if err != nil {
			releaseBuildErr = err
			return
		}
		releaseBinPath = filepath.Join(dir, "phelix")
		ldflags := fmt.Sprintf("-X github.com/abdorrahmani/phelix/internal/version.Version=%s", releaseTestVersion)
		build := exec.Command("go", "build", "-ldflags", ldflags, "-o", releaseBinPath, ".")
		build.Dir = repoRoot
		if out, err := build.CombinedOutput(); err != nil {
			releaseBuildErr = fmt.Errorf("go build: %v: %s", err, out)
		}
	})
	if releaseBuildErr != nil {
		t.Fatalf("failed to build release test binary: %v", releaseBuildErr)
	}
	data, err := os.ReadFile(releaseBinPath)
	if err != nil {
		t.Fatalf("failed to read release test binary: %v", err)
	}
	return data
}

// releaseServer is an httptest server speaking the installer's release
// contract, plus the client pointed at it.
type releaseServer struct {
	srv    *httptest.Server
	client *ReleaseClient

	latestVersion string   // served via X-Phelix-Version and body
	binary        []byte   // artifact payload
	checksum      string   // .sha256 payload; empty means 404
	binaryStatus  int      // status for the artifact endpoint
	requests      []string // artifact URLs requested
}

func newReleaseServer(t *testing.T, latestVersion string, binary []byte, checksum string) *releaseServer {
	t.Helper()
	rs := &releaseServer{latestVersion: latestVersion, binary: binary, checksum: checksum, binaryStatus: http.StatusOK}

	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Phelix-Version", rs.latestVersion)
		fmt.Fprintln(w, rs.latestVersion)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		rs.requests = append(rs.requests, r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, ".sha256"):
			if rs.checksum == "" {
				http.NotFound(w, r)
				return
			}
			fmt.Fprint(w, rs.checksum)
		case r.URL.Path == rs.artifactPath(rs.latestVersion):
			if rs.binaryStatus != http.StatusOK {
				http.Error(w, "boom", rs.binaryStatus)
				return
			}
			if rs.binary == nil {
				// No artifact registered for this release at all.
				http.NotFound(w, r)
				return
			}
			// A registered but empty payload downloads as a 200 with no
			// bytes, which the client must reject.
			w.Write(rs.binary)
		default:
			http.NotFound(w, r)
		}
	})

	rs.srv = httptest.NewServer(mux)
	t.Cleanup(rs.srv.Close)
	rs.client = &ReleaseClient{BaseURL: rs.srv.URL}
	return rs
}

func (rs *releaseServer) artifactPath(version string) string {
	return fmt.Sprintf("/releases/%s/phelix-linux-amd64", version)
}

func sha256Hex(t *testing.T, data []byte) string {
	t.Helper()
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func writeFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}
