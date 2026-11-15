package grpc

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	phelixconfig "github.com/abdorrahmani/phelix/config"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/abdorrahmani/phelix/internal/server"
)

var (
	appConfigAPIBaseURL  string
	appConfigHTTPClient  *http.Client
	appConfigHTTPTimeout = 10 * time.Second
)

type AppConfigTask struct {
	RequestID        string                     `json:"request_id"`
	Command          string                     `json:"command"`
	AppName          string                     `json:"app_name"`
	WorkloadType     string                     `json:"workload_type,omitempty"`
	Content          string                     `json:"content,omitempty"`
	ExpectedRevision string                     `json:"expected_revision,omitempty"`
	MustNotExist     bool                       `json:"must_not_exist,omitempty"`
	ReadDeniedPaths  []project.ConfigDeniedPath `json:"read_denied_paths,omitempty"`
	WriteDeniedPaths []project.ConfigDeniedPath `json:"write_denied_paths,omitempty"`
	InitialContent   []byte                     `json:"-"`
}

type AppConfigResult struct {
	RequestID       string             `json:"request_id"`
	PoliciesApplied bool               `json:"policies_applied,omitempty"`
	Status          string             `json:"status"`
	Error           string             `json:"error,omitempty"`
	ErrorCode       string             `json:"error_code,omitempty"`
	Content         string             `json:"content,omitempty"`
	Schema          string             `json:"schema,omitempty"`
	InitialContent  string             `json:"initial_content,omitempty"`
	Exists          bool               `json:"exists,omitempty"`
	Revision        string             `json:"revision,omitempty"`
	UpdatedAt       string             `json:"updated_at,omitempty"`
	Changed         bool               `json:"changed,omitempty"`
	Valid           bool               `json:"valid,omitempty"`
	Problems        []AppConfigProblem `json:"problems,omitempty"`
}

type AppConfigProblem struct {
	Message  string `json:"message"`
	Severity string `json:"severity"`
	Path     string `json:"path,omitempty"`
	Line     int    `json:"line,omitempty"`
	Column   int    `json:"column,omitempty"`
}

// TakeAppConfigTask fetches the pending task for a configuration wake-up.
func TakeAppConfigTask(requestID string) (*AppConfigTask, error) {
	req, err := newAppConfigRequest(http.MethodGet,
		"/phelix/agent/config/pending?request_id="+url.QueryEscape(requestID), nil)
	if err != nil {
		return nil, err
	}
	resp, err := appConfigDo(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, appConfigHTTPError("fetch configuration task", resp)
	}
	var task AppConfigTask
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&task); err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeNetwork, "failed to decode configuration task", err)
	}
	return &task, nil
}

// SubmitAppConfigResult posts the agent's answer to a fetched task.
func SubmitAppConfigResult(result AppConfigResult) error {
	body, err := json.Marshal(result)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeUnknown, "failed to encode configuration result", err)
	}
	req, err := newAppConfigRequest(http.MethodPost,
		"/phelix/agent/config/result", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := appConfigDo(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return appConfigHTTPError("submit configuration result", resp)
	}
	return nil
}

func newAppConfigRequest(method, path string, body io.Reader) (*http.Request, error) {
	cfg := phelixconfig.Get()
	if cfg == nil {
		return nil, phelixerr.New(phelixerr.CodeConfiguration, "no configuration loaded")
	}
	session, err := loadSession()
	if err != nil {
		return nil, err
	}
	base := cfg.App.API
	if appConfigAPIBaseURL != "" {
		base = appConfigAPIBaseURL
	}
	req, err := http.NewRequest(method, strings.TrimRight(base, "/")+path, body)
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeNetwork, "failed to create configuration request", err)
	}
	req.Header.Set("Authorization", "Bearer "+session.Token)
	req.Header.Set("X-Session-ID", session.SessionID)
	req.Header.Set("X-Server-ID", server.GetServerID())
	return req, nil
}

func appConfigDo(req *http.Request) (*http.Response, error) {
	client := appConfigHTTPClient
	if client == nil {
		client = &http.Client{Timeout: appConfigHTTPTimeout, CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeNetwork, "configuration request failed", err)
	}
	return resp, nil
}

func appConfigHTTPError(action string, resp *http.Response) error {
	code := phelixerr.CodeNetwork
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden {
		code = phelixerr.CodeNotFound
	}
	return phelixerr.Newf(code, "%s: %s", action, resp.Status)
}
