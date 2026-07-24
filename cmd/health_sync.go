package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/abdorrahmani/phelix/cmd/auth"
	"github.com/abdorrahmani/phelix/config"
	"github.com/abdorrahmani/phelix/internal/health"
)

// HealthSetPayload is the REST request body for initializing health checks.
type HealthSetPayload struct {
	AppID         string `json:"app_id"`
	AppName       string `json:"app_name"`
	Path          string `json:"path"`
	Interval      string `json:"interval"`
	Retries       int    `json:"retries"`
	ExpectedCodes string `json:"expected_codes"`
	Timeout       string `json:"timeout"`
	Mode          string `json:"mode"`
}

// HealthAddPayload is the REST request body for adding a health check endpoint.
type HealthAddPayload struct {
	AppID         string `json:"app_id"`
	AppName       string `json:"app_name"`
	Name          string `json:"name"`
	URL           string `json:"url"`
	Interval      string `json:"interval"`
	Retries       int    `json:"retries"`
	ExpectedCodes string `json:"expected_codes"`
	Timeout       string `json:"timeout"`
}

// SendHealthSetToServer uploads the health set config (deploy tier + default endpoint) to the Phelix backend.
func SendHealthSetToServer(appID, appName, path, interval, timeout, expectedCodes, mode string, retries int) error {
	cfg := config.Get()
	session, err := auth.GetValidSession()
	if err != nil {
		log.Printf("[Health] Skipping backend sync: %v", err)
		return nil
	}

	payload := HealthSetPayload{
		AppID:         appID,
		AppName:       appName,
		Path:          path,
		Interval:      interval,
		Retries:       retries,
		ExpectedCodes: expectedCodes,
		Timeout:       timeout,
		Mode:          mode,
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", cfg.App.API+"/phelix/health/set", bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("error creating request: %w", err)
	}

	req.Header.Set("X-Session-ID", session.SessionID)
	req.Header.Set("Authorization", "Bearer "+session.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("error sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, string(data))
	}

	log.Printf("[Health] Successfully synced health set config to server for app '%s'", appName)
	return nil
}

// SendHealthAddToServer uploads the health check endpoint config to the Phelix backend.
func SendHealthAddToServer(appID, appName string, epConfig *health.HealthCheckConfig) error {
	cfg := config.Get()
	session, err := auth.GetValidSession()
	if err != nil {
		log.Printf("[Health] Skipping backend sync: %v", err)
		return nil
	}

	payload := HealthAddPayload{
		AppID:         appID,
		AppName:       appName,
		Name:          epConfig.Name,
		URL:           epConfig.URL,
		Interval:      epConfig.Interval,
		Retries:       epConfig.Retries,
		ExpectedCodes: epConfig.ExpectedCodes,
		Timeout:       epConfig.Timeout,
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", cfg.App.API+"/phelix/health", bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("error creating request: %w", err)
	}

	req.Header.Set("X-Session-ID", session.SessionID)
	req.Header.Set("Authorization", "Bearer "+session.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("error sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, string(data))
	}

	log.Printf("[Health] Successfully synced endpoint '%s' to server", epConfig.Name)
	return nil
}
