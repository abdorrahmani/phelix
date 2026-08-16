package auth

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/abdorrahmani/phelix/config"
	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// SendAppsToServer uploads the list of running apps to the Phelix server.
func SendAppsToServer() error {
	cfg := config.Get()
	session, err := GetValidSession()
	if err != nil {
		return phelixerr.Wrap(
			phelixerr.CodeUnauthenticated,
			"authentication required; please run 'phelix auth login'",
			err,
		)
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
	req, err := http.NewRequest("POST", cfg.App.API+"/phelix/apps", bytes.NewBuffer(body))
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeNetwork, "error creating request", err)
	}

	req.Header.Set("X-Session-ID", session.SessionID)
	req.Header.Set("Authorization", "Bearer "+session.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeNetwork, "error sending request", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		return phelixerr.Newf(
			phelixerr.CodeServer,
			"server returned %d: %s",
			resp.StatusCode,
			string(data),
		)
	}

	log.Printf("[Monitor] Successfully sent apps to server")
	return nil
}
