package update

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// --- Latest-version resolution ----------------------------------------------

func TestResolveLatestFromHeader(t *testing.T) {
	rs := newReleaseServer(t, "1.2.3", nil, "")
	got, err := rs.client.ResolveLatest()
	if err != nil {
		t.Fatalf("ResolveLatest() error: %v", err)
	}
	if got != "1.2.3" {
		t.Fatalf("ResolveLatest() = %q, want %q", got, "1.2.3")
	}
}

func TestResolveLatestFromBodyFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No X-Phelix-Version header: the body is the fallback source.
		w.Write([]byte("v1.2.4\n"))
	}))
	defer srv.Close()
	client := &ReleaseClient{BaseURL: srv.URL}

	got, err := client.ResolveLatest()
	if err != nil {
		t.Fatalf("ResolveLatest() error: %v", err)
	}
	if got != "v1.2.4" {
		t.Fatalf("ResolveLatest() = %q, want %q", got, "v1.2.4")
	}
}

func TestResolveLatestErrors(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		defer srv.Close()
		_, err := (&ReleaseClient{BaseURL: srv.URL}).ResolveLatest()
		if !phelixerr.IsCode(err, phelixerr.CodeNotFound) {
			t.Fatalf("error = %v, want CodeNotFound", err)
		}
	})

	t.Run("server error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		defer srv.Close()
		_, err := (&ReleaseClient{BaseURL: srv.URL}).ResolveLatest()
		if !phelixerr.IsCode(err, phelixerr.CodeConnection) {
			t.Fatalf("error = %v, want CodeConnection", err)
		}
	})

	t.Run("network failure", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close() // nothing is listening anymore
		_, err := (&ReleaseClient{BaseURL: url}).ResolveLatest()
		if !phelixerr.IsCode(err, phelixerr.CodeConnection) {
			t.Fatalf("error = %v, want CodeConnection", err)
		}
	})

	t.Run("empty response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		defer srv.Close()
		_, err := (&ReleaseClient{BaseURL: srv.URL}).ResolveLatest()
		if !phelixerr.IsCode(err, phelixerr.CodeNotFound) {
			t.Fatalf("error = %v, want CodeNotFound", err)
		}
	})
}

// --- Artifact download --------------------------------------------------------

func TestDownloadArtifactOK(t *testing.T) {
	rs := newReleaseServer(t, "1.2.3", []byte("BINARY-PAYLOAD"), "")
	dest := filepath.Join(t.TempDir(), "out.bin")

	if err := rs.client.DownloadArtifact("1.2.3", Platform{"linux", "amd64"}, dest); err != nil {
		t.Fatalf("DownloadArtifact() error: %v", err)
	}
	if got := readFile(t, dest); string(got) != "BINARY-PAYLOAD" {
		t.Fatalf("downloaded content = %q", got)
	}
}

func TestDownloadArtifactErrors(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		rs := newReleaseServer(t, "1.2.3", nil, "")
		dest := filepath.Join(t.TempDir(), "out.bin")
		err := rs.client.DownloadArtifact("1.2.3", Platform{"linux", "amd64"}, dest)
		if !phelixerr.IsCode(err, phelixerr.CodeNotFound) {
			t.Fatalf("error = %v, want CodeNotFound", err)
		}
		if strings.Contains(err.Error(), "http://") {
			t.Fatalf("error message leaks the URL: %v", err)
		}
	})

	t.Run("server error", func(t *testing.T) {
		rs := newReleaseServer(t, "1.2.3", nil, "")
		rs.binaryStatus = http.StatusInternalServerError
		dest := filepath.Join(t.TempDir(), "out.bin")
		err := rs.client.DownloadArtifact("1.2.3", Platform{"linux", "amd64"}, dest)
		if !phelixerr.IsCode(err, phelixerr.CodeConnection) {
			t.Fatalf("error = %v, want CodeConnection", err)
		}
	})

	t.Run("empty payload", func(t *testing.T) {
		rs := newReleaseServer(t, "1.2.3", []byte{}, "")
		dest := filepath.Join(t.TempDir(), "out.bin")
		err := rs.client.DownloadArtifact("1.2.3", Platform{"linux", "amd64"}, dest)
		if !phelixerr.IsCode(err, phelixerr.CodeUpdateFailed) {
			t.Fatalf("error = %v, want CodeUpdateFailed", err)
		}
	})
}

// --- Checksums -----------------------------------------------------------------

func TestFetchChecksum(t *testing.T) {
	payload := []byte("BINARY-PAYLOAD")
	sum := sha256Hex(t, payload)

	t.Run("found", func(t *testing.T) {
		rs := newReleaseServer(t, "1.2.3", payload, sum+"  phelix-linux-amd64\n")
		got, found, err := rs.client.FetchChecksum("1.2.3", Platform{"linux", "amd64"})
		if err != nil || !found {
			t.Fatalf("FetchChecksum() = %q, %v, %v; want found", got, found, err)
		}
		if got != sum {
			t.Fatalf("checksum = %q, want %q", got, sum)
		}
	})

	t.Run("absent", func(t *testing.T) {
		rs := newReleaseServer(t, "1.2.3", payload, "")
		_, found, err := rs.client.FetchChecksum("1.2.3", Platform{"linux", "amd64"})
		if err != nil {
			t.Fatalf("FetchChecksum() unexpected error: %v", err)
		}
		if found {
			t.Fatal("FetchChecksum() found = true, want false")
		}
	})

	t.Run("malformed", func(t *testing.T) {
		rs := newReleaseServer(t, "1.2.3", payload, "definitely-not-a-checksum")
		_, _, err := rs.client.FetchChecksum("1.2.3", Platform{"linux", "amd64"})
		if !phelixerr.IsCode(err, phelixerr.CodeUpdateFailed) {
			t.Fatalf("error = %v, want CodeUpdateFailed", err)
		}
	})

	t.Run("server error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		defer srv.Close()
		_, _, err := (&ReleaseClient{BaseURL: srv.URL}).FetchChecksum("1.2.3", Platform{"linux", "amd64"})
		if !phelixerr.IsCode(err, phelixerr.CodeConnection) {
			t.Fatalf("error = %v, want CodeConnection", err)
		}
	})
}

func TestVerifyChecksum(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "bin")
	writeFile(t, file, []byte("BINARY-PAYLOAD"), 0o600)
	sum := sha256Hex(t, []byte("BINARY-PAYLOAD"))

	if err := VerifyChecksum(file, sum); err != nil {
		t.Fatalf("VerifyChecksum() unexpected error: %v", err)
	}
	// Uppercase published checksums must compare case-insensitively.
	if err := VerifyChecksum(file, strings.ToUpper(sum)); err != nil {
		t.Fatalf("VerifyChecksum(uppercase) unexpected error: %v", err)
	}

	err := VerifyChecksum(file, strings.Repeat("ab", 32))
	if !phelixerr.IsCode(err, phelixerr.CodeUpdateFailed) {
		t.Fatalf("error = %v, want CodeUpdateFailed", err)
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("error should be a hard checksum mismatch: %v", err)
	}
}

func TestFileSHA256MatchesReference(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "bin")
	writeFile(t, file, []byte("hello"), 0o600)
	got, err := fileSHA256(file)
	if err != nil {
		t.Fatalf("fileSHA256() error: %v", err)
	}
	// echo -n hello | sha256sum
	want := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got != want {
		t.Fatalf("fileSHA256() = %q, want %q", got, want)
	}
}

// TestRedirectGuardRejectsHTTPDowngrade verifies an HTTPS download is never
// silently redirected to HTTP.
func TestRedirectGuardRejectsHTTPDowngrade(t *testing.T) {
	c := &ReleaseClient{}
	httpsOrigin := &http.Request{URL: &url.URL{Scheme: "https", Host: "phelix.anophel.com"}}
	httpsRedirect := &http.Request{URL: &url.URL{Scheme: "https", Host: "phelix.anophel.com"}}
	httpRedirect := &http.Request{URL: &url.URL{Scheme: "http", Host: "evil.example.com"}}

	if err := c.checkRedirect(httpsRedirect, []*http.Request{httpsOrigin}); err != nil {
		t.Fatalf("https→https redirect should be allowed: %v", err)
	}
	if err := c.checkRedirect(httpRedirect, []*http.Request{httpsOrigin}); err == nil {
		t.Fatal("https→http redirect should be refused")
	}
	// A first request has no redirect history, so anything goes.
	if err := c.checkRedirect(httpRedirect, nil); err != nil {
		t.Fatalf("initial request should be unrestricted: %v", err)
	}
}
