package cmd

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

func TestUpdateBetaChannel(t *testing.T) {
	for _, tc := range []struct {
		name, checksum, version string
		check, fail             bool
	}{
		{name: "check", check: true, version: "1.0.0"},
		{name: "install", version: "1.1.0-beta.1"},
		{name: "checksum mismatch", checksum: strings.Repeat("0", 64), version: "1.0.0", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binDir := freshInstalledBinary(t)
			home := t.TempDir()
			payload := readBytes(t, betaReleasePayload)
			checksum := tc.checksum
			if checksum == "" {
				checksum = sha256HexOf(t, payload)
			}
			artifact := fmt.Sprintf("/releases/1.1.0-beta.1/phelix-%s-%s", runtime.GOOS, runtime.GOARCH)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/releases/beta/version":
					fmt.Fprintln(w, "1.1.0-beta.1")
				case artifact:
					if tc.check {
						t.Error("check downloaded an artifact")
					}
					w.Write(payload)
				case artifact + ".sha256":
					fmt.Fprint(w, checksum)
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			args := []string{"update", "--channel", "beta"}
			if tc.check {
				args = append(args, "--check")
			}
			code, stdout, stderr := runViaPath(t, binDir, home, srv.URL, args...)
			if tc.fail {
				if code != 1 || !strings.Contains(stderr, "checksum mismatch") {
					t.Fatalf("exit = %d, stderr = %s", code, stderr)
				}
			} else if code != 0 || !strings.Contains(stdout, "v1.1.0-beta.1") {
				t.Fatalf("exit = %d, stdout = %s, stderr = %s", code, stdout, stderr)
			}
			code, stdout, stderr = runViaPath(t, binDir, home, srv.URL, "version", "--short")
			if code != 0 || strings.TrimSpace(stdout) != tc.version {
				t.Fatalf("version = %q, exit = %d, stderr = %s; want %s", stdout, code, stderr, tc.version)
			}
		})
	}
}

func TestUpdateInvalidChannel(t *testing.T) {
	code, _, stderr := runViaPath(t, freshInstalledBinary(t), t.TempDir(), "http://127.0.0.1:1", "update", "--channel", "nightly")
	if code == 0 || !strings.Contains(stderr, "choose stable or beta") {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
}
