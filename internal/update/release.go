package update

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// DefaultBaseURL is the release host shared with install.sh. The layout is
// the installer's release contract, which this package must keep following:
//
//	<base>/releases/<version>/phelix-<os>-<arch>
//	<base>/releases/<version>/phelix-<os>-<arch>.sha256   (optional sidecar)
//	<base>/releases/latest/version                        (X-Phelix-Version header)
const DefaultBaseURL = "https://phelix.anophel.com"

const (
	latestVersionPath = "/releases/latest/version"
	versionHeader     = "X-Phelix-Version"
)

// Request timeouts. The download gets a generous budget for slow links; the
// metadata endpoints are small and must fail fast.
const (
	metadataTimeout = 15 * time.Second
	downloadTimeout = 10 * time.Minute
)

// ReleaseClient resolves and downloads Phelix releases using the same
// contract as install.sh. BaseURL and HTTP are injectable for tests.
type ReleaseClient struct {
	BaseURL string
	HTTP    *http.Client
}

func (c *ReleaseClient) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: downloadTimeout}
}

// checkRedirect refuses any redirect that would downgrade an HTTPS origin to
// HTTP (no silent TLS downgrade).
func (c *ReleaseClient) checkRedirect(redirect *http.Request, via []*http.Request) error {
	if len(via) > 0 && via[0].URL.Scheme == "https" && redirect.URL.Scheme != "https" {
		return fmt.Errorf("refusing to follow redirect from %s to %s", via[0].URL, redirect.URL)
	}
	return nil
}

// do issues req using the client's HTTP transport.
func (c *ReleaseClient) do(req *http.Request) (*http.Response, error) {
	cl := *c.client()
	cl.CheckRedirect = c.checkRedirect
	return cl.Do(req)
}

// artifactURL builds the release download URL for version/platform.
func (c *ReleaseClient) artifactURL(version string, p Platform) string {
	return strings.TrimSuffix(c.BaseURL, "/") + "/releases/" + version + "/phelix-" + p.OS + "-" + p.Arch
}

// ResolveLatest returns the latest stable version string exactly as the
// server publishes it, using the same authoritative endpoint and header as
// install.sh: GET /releases/latest/version → X-Phelix-Version. When the
// header is absent the response body is used as a fallback.
func (c *ReleaseClient) ResolveLatest() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), metadataTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(c.BaseURL, "/")+latestVersionPath, nil)
	if err != nil {
		return "", phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "invalid release server URL %q", c.BaseURL)
	}
	resp, err := c.do(req)
	if err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeConnection, "could not reach the release server", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", phelixerr.New(phelixerr.CodeNotFound,
			"the release server publishes no latest-version information")
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return "", phelixerr.Newf(phelixerr.CodeConnection,
			"the release server returned HTTP %d while resolving the latest version", resp.StatusCode)
	}

	v := strings.TrimSpace(resp.Header.Get(versionHeader))
	if v == "" {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		v = strings.TrimSpace(strings.SplitN(string(body), "\n", 2)[0])
	}
	if v == "" {
		return "", phelixerr.New(phelixerr.CodeNotFound,
			"the release server did not report a latest version")
	}
	return v, nil
}

// DownloadArtifact streams the release binary for version/platform into
// destPath (created/truncated by this call). It fails on HTTP errors and on
// empty payloads; callers must still checksum-verify and run-validate the
// file before trusting it.
func (c *ReleaseClient) DownloadArtifact(version string, p Platform, destPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.artifactURL(version, p), nil)
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "invalid release download URL")
	}
	resp, err := c.do(req)
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeConnection, err,
			"could not download release %s for %s", version, p)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return phelixerr.Newf(phelixerr.CodeNotFound,
			"release %s for %s was not found on the release server", version, p)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return phelixerr.Newf(phelixerr.CodeConnection,
			"the release server returned HTTP %d while downloading %s for %s", resp.StatusCode, version, p)
	}

	f, err := os.Create(destPath)
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "create download target %s", destPath)
	}
	n, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		return phelixerr.Wrap(phelixerr.CodeConnection, "download interrupted", copyErr)
	}
	if closeErr != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, closeErr, "finalize download %s", destPath)
	}
	if n == 0 {
		return phelixerr.New(phelixerr.CodeUpdateFailed, "the downloaded release binary is empty")
	}
	return nil
}

// FetchChecksum downloads the .sha256 sidecar for a release artifact. found
// is false when the release publishes no checksum — install.sh's policy in
// that case is to warn and continue, never to guess. A malformed sidecar is
// a hard error.
func (c *ReleaseClient) FetchChecksum(version string, p Platform) (sumHex string, found bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), metadataTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.artifactURL(version, p)+".sha256", nil)
	if err != nil {
		return "", false, phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "invalid checksum URL")
	}
	resp, err := c.do(req)
	if err != nil {
		return "", false, phelixerr.Wrap(phelixerr.CodeConnection, "could not download the release checksum", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", false, nil
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return "", false, phelixerr.Newf(phelixerr.CodeConnection,
			"the release server returned HTTP %d while fetching the checksum", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return "", false, phelixerr.Wrap(phelixerr.CodeConnection, "could not read the release checksum", err)
	}
	fields := strings.Fields(string(body))
	for _, f := range fields {
		if isSHA256Hex(f) {
			return strings.ToLower(f), true, nil
		}
	}
	return "", false, phelixerr.New(phelixerr.CodeUpdateFailed,
		"the published checksum file for this release is malformed")
}

// isSHA256Hex reports whether s is a 64-character hexadecimal SHA-256 digest.
func isSHA256Hex(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// VerifyChecksum compares the SHA-256 of filePath against wantHex. A
// mismatch is always a hard failure — it must never be downgraded to a
// warning.
func VerifyChecksum(filePath, wantHex string) error {
	sum, err := fileSHA256(filePath)
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "hash %s", filePath)
	}
	if subtle.ConstantTimeCompare([]byte(sum), []byte(strings.ToLower(wantHex))) != 1 {
		return phelixerr.Newf(phelixerr.CodeUpdateFailed,
			"checksum mismatch for the downloaded release: got %s, want %s", sum, wantHex)
	}
	return nil
}

func fileSHA256(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
