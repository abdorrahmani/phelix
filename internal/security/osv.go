package security

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// DefaultOSVBaseURL is the public OSV.dev API endpoint.
const DefaultOSVBaseURL = "https://api.osv.dev"

// Request bounds for advisory retrieval. The response caps keep a hostile
// or broken endpoint from exhausting memory; the per-request timeout keeps
// a hung endpoint from stalling a scan.
const (
	osvRequestTimeout    = 10 * time.Second
	osvMaxResponseBytes  = 4 << 20
	osvBatchSize         = 500
	osvMaxAttempts       = 3
	osvMaxRetryAfter     = 30 * time.Second
	osvDetailConcurrency = 8
)

// osvEcosystemName maps an internal ecosystem to the OSV ecosystem string.
func osvEcosystemName(eco Ecosystem) (string, error) {
	switch eco {
	case EcosystemGo:
		return "Go", nil
	case EcosystemRust:
		return "Cargo", nil
	default:
		return "", phelixerr.Newf(phelixerr.CodeInvalidArgument, "unsupported ecosystem %q for OSV queries", string(eco))
	}
}

// OSVQuery is one package identity to ask OSV about. Version is
// deliberately omitted: advisories are retrieved for the whole package and
// evaluated locally so that every advisory gets an explicit disposition.
type OSVQuery struct {
	Ecosystem Ecosystem
	Name      string
}

// OSVAdvisory is the subset of the OSV advisory schema the matcher uses.
// All content is untrusted data: it is decoded into bounded strings and
// never executed or rendered raw.
type OSVAdvisory struct {
	ID               string         `json:"id"`
	Aliases          []string       `json:"aliases,omitempty"`
	Related          []string       `json:"related,omitempty"`
	Summary          string         `json:"summary,omitempty"`
	Details          string         `json:"details,omitempty"`
	Modified         string         `json:"modified,omitempty"`
	Published        string         `json:"published,omitempty"`
	Severity         []OSVSeverity  `json:"severity,omitempty"`
	Affected         []OSVAffected  `json:"affected,omitempty"`
	References       []OSVReference `json:"references,omitempty"`
	DatabaseSpecific map[string]any `json:"database_specific,omitempty"`
}

// OSVSeverity carries a raw severity artifact such as a CVSS vector.
type OSVSeverity struct {
	Type  string `json:"type,omitempty"`
	Score string `json:"score,omitempty"`
}

// OSVAffected is one affected package entry of an advisory.
type OSVAffected struct {
	Package struct {
		Ecosystem string `json:"ecosystem,omitempty"`
		Name      string `json:"name,omitempty"`
		Purl      string `json:"purl,omitempty"`
	} `json:"package"`
	Ranges           []OSVRange     `json:"ranges,omitempty"`
	Versions         []string       `json:"versions,omitempty"`
	DatabaseSpecific map[string]any `json:"database_specific,omitempty"`
}

// OSVRange is one version range with its event sequence.
type OSVRange struct {
	Type             string              `json:"type,omitempty"`
	Repo             string              `json:"repo,omitempty"`
	Events           []map[string]string `json:"events"`
	DatabaseSpecific map[string]any      `json:"database_specific,omitempty"`
}

// OSVReference is one advisory reference link.
type OSVReference struct {
	Type string `json:"type,omitempty"`
	URL  string `json:"url,omitempty"`
}

// OSVClient retrieves advisories from OSV.dev. BaseURL and HTTP are
// injectable for tests.
type OSVClient struct {
	BaseURL string
	HTTP    *http.Client
}

func (c *OSVClient) baseURL() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return DefaultOSVBaseURL
}

func (c *OSVClient) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: osvRequestTimeout}
}

// checkRedirect refuses any redirect that would downgrade an HTTPS origin
// to HTTP, matching the release client's transport policy.
func (c *OSVClient) checkRedirect(redirect *http.Request, via []*http.Request) error {
	if len(via) > 0 && via[0].URL.Scheme == "https" && redirect.URL.Scheme != "https" {
		return fmt.Errorf("refusing to follow redirect from %s to %s", via[0].URL, redirect.URL)
	}
	return nil
}

// do performs one request with the shared transport policy and returns the
// decoded body capped at the response limit.
func (c *OSVClient) do(ctx context.Context, method, path string, body any, out any) error {
	cl := *c.httpClient()
	cl.CheckRedirect = c.checkRedirect

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return phelixerr.Wrap(phelixerr.CodeInvalidArgument, "could not encode OSV request", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.baseURL(), "/")+path, reader)
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "invalid OSV request to %s", path)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := cl.Do(req)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeConnection, "could not reach the OSV.dev advisory service", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return c.retryAfterTooManyRequests(ctx, method, path, body, out, resp)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return phelixerr.Newf(phelixerr.CodeConnection,
			"the OSV.dev advisory service returned HTTP %d for %s", resp.StatusCode, path)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, osvMaxResponseBytes))
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeConnection, "could not read the OSV.dev response", err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeConnection, err, "the OSV.dev response for %s is not valid advisory data", path)
	}
	return nil
}

// retryAfterTooManyRequests retries a rate-limited request a bounded
// number of times, honoring a server Retry-After capped at the retry
// ceiling.
func (c *OSVClient) retryAfterTooManyRequests(ctx context.Context, method, path string, body any, out any, resp *http.Response) error {
	delay := osvRetryDelay(resp.Header.Get("Retry-After"))
	var lastErr error
	for attempt := 1; attempt <= osvMaxAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return phelixerr.Wrap(phelixerr.CodeTimeout, "OSV.dev request cancelled while rate limited", ctx.Err())
		case <-time.After(delay):
		}
		err := c.do(ctx, method, path, body, out)
		if err == nil {
			return nil
		}
		lastErr = err
		if !phelixerr.IsCode(err, phelixerr.CodeConnection) || !strings.Contains(err.Error(), "HTTP 429") {
			return err
		}
		delay = minDuration(2*delay, osvMaxRetryAfter)
	}
	return lastErr
}

func osvRetryDelay(header string) time.Duration {
	if header == "" {
		return time.Second
	}
	if seconds, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && seconds > 0 {
		return minDuration(time.Duration(seconds)*time.Second, osvMaxRetryAfter)
	}
	if when, err := http.ParseTime(header); err == nil {
		if d := time.Until(when); d > 0 {
			return minDuration(d, osvMaxRetryAfter)
		}
	}
	return time.Second
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

type osvBatchRequest struct {
	Queries []osvBatchQuery `json:"queries"`
}

type osvBatchQuery struct {
	Package osvPackageName `json:"package"`
}

type osvPackageName struct {
	Ecosystem string `json:"ecosystem"`
	Name      string `json:"name"`
}

type osvBatchResponse struct {
	Results []osvBatchResult `json:"results"`
}

type osvBatchResult struct {
	Vulns []struct {
		ID string `json:"id"`
	} `json:"vulns"`
}

// QueryIDs asks OSV for the advisory IDs touching each package. Results
// are aligned with the input queries. Package-name queries intentionally
// omit a version so the matcher can give every advisory a disposition.
func (c *OSVClient) QueryIDs(ctx context.Context, queries []OSVQuery) ([][]string, error) {
	out := make([][]string, len(queries))
	for start := 0; start < len(queries); start += osvBatchSize {
		end := start + osvBatchSize
		if end > len(queries) {
			end = len(queries)
		}
		batch := make([]osvBatchQuery, 0, end-start)
		for _, q := range queries[start:end] {
			eco, err := osvEcosystemName(q.Ecosystem)
			if err != nil {
				return nil, err
			}
			batch = append(batch, osvBatchQuery{Package: osvPackageName{Ecosystem: eco, Name: q.Name}})
		}
		var resp osvBatchResponse
		if err := c.do(ctx, http.MethodPost, "/v1/querybatch", osvBatchRequest{Queries: batch}, &resp); err != nil {
			return nil, err
		}
		if len(resp.Results) != end-start {
			return nil, phelixerr.Newf(phelixerr.CodeConnection,
				"the OSV.dev batch response returned %d results for %d queries", len(resp.Results), end-start)
		}
		for i, result := range resp.Results {
			ids := make([]string, 0, len(result.Vulns))
			for _, v := range result.Vulns {
				if v.ID != "" {
					ids = append(ids, v.ID)
				}
			}
			out[start+i] = ids
		}
	}
	return out, nil
}

// FetchAdvisory retrieves one advisory by OSV ID.
func (c *OSVClient) FetchAdvisory(ctx context.Context, id string) (*OSVAdvisory, error) {
	if !validOSVID(id) {
		return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument, "invalid OSV advisory id %q", id)
	}
	var advisory OSVAdvisory
	if err := c.do(ctx, http.MethodGet, "/v1/vulns/"+url.PathEscape(id), nil, &advisory); err != nil {
		return nil, err
	}
	if advisory.ID == "" {
		return nil, phelixerr.Newf(phelixerr.CodeConnection, "the OSV.dev response for %s carries no advisory id", id)
	}
	return &advisory, nil
}

// validOSVID guards advisory IDs used as cache file names and URL paths.
func validOSVID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '.', r == '_':
		default:
			return false
		}
	}
	return true
}
