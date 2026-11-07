package security

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOSVClient_QueryIDs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/querybatch" {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"vulns":[{"id":"GO-2024-1001"}]},{"vulns":[{"id":"GHSA-1111-2222-3333"},{"id":"GO-2024-1002"}]},{}]}`))
	}))
	defer server.Close()

	client := &OSVClient{BaseURL: server.URL}
	ids, err := client.QueryIDs(context.Background(), []OSVQuery{
		{Ecosystem: EcosystemGo, Name: "golang.org/x/crypto"},
		{Ecosystem: EcosystemRust, Name: "serde"},
		{Ecosystem: EcosystemGo, Name: "golang.org/x/net"},
	})
	if err != nil {
		t.Fatalf("QueryIDs: %v", err)
	}
	if len(ids) != 3 || len(ids[0]) != 1 || ids[0][0] != "GO-2024-1001" {
		t.Fatalf("first query result wrong: %v", ids)
	}
	if len(ids[1]) != 2 {
		t.Fatalf("second query result wrong: %v", ids[1])
	}
	if len(ids[2]) != 0 {
		t.Fatalf("third query result wrong: %v", ids[2])
	}
}

func TestOSVClient_QueryIDs_ResultMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()

	client := &OSVClient{BaseURL: server.URL}
	if _, err := client.QueryIDs(context.Background(), []OSVQuery{{Ecosystem: EcosystemGo, Name: "x"}}); err == nil {
		t.Fatal("misaligned batch response must fail")
	}
}

func TestOSVClient_FetchAdvisory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/vulns/GO-2024-1001" {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"id":"GO-2024-1001","aliases":["CVE-2024-0001"],"summary":"example advisory"}`))
	}))
	defer server.Close()

	client := &OSVClient{BaseURL: server.URL}
	adv, err := client.FetchAdvisory(context.Background(), "GO-2024-1001")
	if err != nil {
		t.Fatalf("FetchAdvisory: %v", err)
	}
	if adv.ID != "GO-2024-1001" || len(adv.Aliases) != 1 {
		t.Fatalf("advisory decode wrong: %+v", adv)
	}
}

func TestOSVClient_FetchAdvisory_RejectsInvalidID(t *testing.T) {
	client := &OSVClient{BaseURL: "https://api.osv.dev"}
	if _, err := client.FetchAdvisory(context.Background(), "../etc/passwd"); err == nil {
		t.Fatal("path-like advisory ids must be rejected before any request")
	}
}

func TestOSVClient_RetryAfter429(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"vulns":[{"id":"GO-2024-1001"}]}]}`))
	}))
	defer server.Close()

	client := &OSVClient{BaseURL: server.URL}
	ids, err := client.QueryIDs(context.Background(), []OSVQuery{{Ecosystem: EcosystemGo, Name: "x"}})
	if err != nil || attempts != 2 {
		t.Fatalf("429 must retry once and succeed: attempts=%d err=%v", attempts, err)
	}
	if len(ids) != 1 || len(ids[0]) != 1 {
		t.Fatalf("post-retry result wrong: %v", ids)
	}
}

func TestOSVClient_ResponseSizeCap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, osvMaxResponseBytes+1024))
	}))
	defer server.Close()

	client := &OSVClient{BaseURL: server.URL}
	if _, err := client.FetchAdvisory(context.Background(), "GO-2024-1001"); err == nil {
		t.Fatal("oversized responses must fail validation, not be trusted")
	}
}

func TestOSVClient_HTTPSRedirectPolicy(t *testing.T) {
	client := &OSVClient{}
	httpsReq := httptest.NewRequest(http.MethodGet, "https://api.osv.dev/v1/vulns/X", nil)
	httpRedirect := httptest.NewRequest(http.MethodGet, "http://insecure.example/v1/vulns/X", nil)
	if err := client.checkRedirect(httpRedirect, []*http.Request{httpsReq}); err == nil {
		t.Fatal("HTTPS to HTTP redirects must be refused")
	}
	httpsRedirect := httptest.NewRequest(http.MethodGet, "https://api.osv.dev/v1/vulns/X", nil)
	if err := client.checkRedirect(httpsRedirect, []*http.Request{httpsReq}); err != nil {
		t.Fatalf("HTTPS to HTTPS redirects must pass: %v", err)
	}
}

func TestValidOSVID(t *testing.T) {
	for _, good := range []string{"GO-2024-1001", "GHSA-1111-2222-3333", "RUSTSEC-2024-0001", "OSV-2024-555"} {
		if !validOSVID(good) {
			t.Fatalf("%q must be a valid OSV id", good)
		}
	}
	for _, bad := range []string{"", "../escape", "id with space", strings.Repeat("x", 129)} {
		if validOSVID(bad) {
			t.Fatalf("%q must be rejected", bad)
		}
	}
}

func TestOSVRetryDelay(t *testing.T) {
	if d := osvRetryDelay(""); d != time.Second {
		t.Fatalf("missing header defaults to one second, got %v", d)
	}
	if d := osvRetryDelay("2"); d != 2*time.Second {
		t.Fatalf("seconds header honored, got %v", d)
	}
	if d := osvRetryDelay("99999"); d != osvMaxRetryAfter {
		t.Fatalf("seconds header capped, got %v", d)
	}
	if d := osvRetryDelay("garbage"); d != time.Second {
		t.Fatalf("garbage header defaults to one second, got %v", d)
	}
}
