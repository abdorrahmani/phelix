package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/abdorrahmani/gophel/config"
	"github.com/abdorrahmani/gophel/internal/app"
)

// SendAppsToServer uploads the list of running apps to the Gophel server.
func SendAppsToServer() error {
	cfg := config.Get()
	session, err := GetValidSession()
	if err != nil {
		return fmt.Errorf("authentication required. Please run 'gophel auth login'")
	}

	appList := app.Manager.ListApplications()
	log.Printf("[Monitor] Preparing to send %d apps to server", len(appList))

	var appDetails []AppDetail
	for _, a := range appList {
		id, _ := strconv.ParseUint(a.ID, 10, 32)
		appDetails = append(appDetails, AppDetail{
			ID:          uint(id),
			Name:        a.Name,
			Status:      a.Status,
			PID:         a.PID,
			Uptime:      a.Uptime,
			BuildStatus: a.BuildStatus,
			CreatedAt:   a.CreatedAt,
			UpdatedAt:   a.UpdatedAt,
		})
	}

	body, _ := json.Marshal(map[string]any{"apps": appDetails})
	req, err := http.NewRequest("POST", cfg.App.API+"/gophel/apps", bytes.NewBuffer(body))
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

	log.Printf("[Monitor] Successfully sent apps to server")
	return nil
}
