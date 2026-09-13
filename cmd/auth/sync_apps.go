package auth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/abdorrahmani/phelix/config"
	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/server"
)

// SendAppsToServer uploads the list of watched apps to the Phelix server.
// If the user is not logged in, it returns ErrNotLoggedIn so callers can skip
// the upload quietly — building/running still works, only dashboard sync is
// skipped until the user runs 'phelix auth login'.
//
// Apps with Watching=false are excluded from the upload: they do not
// participate in backend monitoring, so the dashboard must not register them.
// The upload itself still happens (even with an empty app list) — it is a
// one-shot registration sync triggered by login/build, not periodic
// monitoring traffic, and an empty list is the correct statement when no app
// is watched.
func SendAppsToServer() error {
	cfg := config.Get()
	if cfg == nil {
		return phelixerr.New(phelixerr.CodeConfiguration, "no configuration loaded")
	}
	session, err := GetValidSession()
	if err != nil {
		return ErrNotLoggedIn
	}

	appList := app.Manager.ListApplications()
	watched := 0
	for _, a := range appList {
		if a.Watching {
			watched++
		}
	}
	logs.Info("monitor", "preparing to send %d of %d apps to server", watched, len(appList))

	// Same server identity the gRPC monitor stream reports (agent id); the
	// backend keys app rows by (server_id, cli_id), so the REST app upload
	// must tag its entries with it to adopt existing rows instead of
	// duplicating them.
	serverID := server.GetServerID()

	var appDetails []AppDetail
	for _, a := range appList {
		if !a.Watching {
			continue
		}
		id, _ := strconv.ParseUint(a.ID, 10, 32)
		appDetails = append(appDetails, AppDetail{
			ID:          uint(id),
			Name:        a.Name,
			Status:      a.Status,
			ServerID:    serverID,
			PID:         a.PID,
			Type:        a.Language,
			Language:    a.Language,
			Uptime:      a.Uptime,
			BuildStatus: a.BuildStatus,
			CreatedAt:   a.CreatedAt,
			UpdatedAt:   a.UpdatedAt,
			Process: AppProcess{
				WorkingDir:         a.Process.WorkingDir,
				Executable:         a.Process.Executable,
				StartCommand:       a.Process.StartCommand,
				StopCommand:        a.Process.StopCommand,
				MaxCPUPercent:      a.Process.MaxCPUPercent,
				MaxMemoryMB:        a.Process.MaxMemoryMB,
				MaxOpenFiles:       a.Process.MaxOpenFiles,
				MaxProcesses:       a.Process.MaxProcesses,
				AutoRestart:        a.Process.AutoRestart,
				CrashLoopBackoff:   a.Process.CrashLoopBackoff,
				GracefulShutdown:   a.Process.GracefulShutdown,
				MaxRestartAttempts: a.Process.MaxRestartAttempts,
				RestartDelayMs:     a.Process.RestartDelayMs,
			},
			Networking: AppNetworking{
				ListenPort:       a.Networking.ListenPort,
				BindAddress:      a.Networking.BindAddress,
				PublicDomain:     a.Networking.PublicDomain,
				BasePath:         a.Networking.BasePath,
				TLSEnabled:       a.Networking.TLSEnabled,
				CertPath:         a.Networking.CertPath,
				KeyPath:          a.Networking.KeyPath,
				ProxyEnabled:     a.Networking.ProxyEnabled,
				CORSEnabled:      a.Networking.CORSEnabled,
				AllowedOrigins:   a.Networking.AllowedOrigins,
				RateLimitEnabled: a.Networking.RateLimitEnabled,
				RateLimitRPS:     a.Networking.RateLimitRPS,
			},
			Logging: AppLogging{
				JSONLogs:          a.Logging.JSONLogs,
				PersistentLogs:    a.Logging.PersistentLogs,
				LogLevel:          a.Logging.LogLevel,
				LogFilePath:       a.Logging.LogFilePath,
				StderrFilePath:    a.Logging.StderrFilePath,
				LogFormat:         a.Logging.LogFormat,
				RotationEnabled:   a.Logging.RotationEnabled,
				RotationMaxSizeMB: a.Logging.RotationMaxSizeMB,
				RotationMaxFiles:  a.Logging.RotationMaxFiles,
				RotationCompress:  a.Logging.RotationCompress,
			},
			Storage: AppStorage{
				Volumes: a.Storage.Volumes,
				DataDir: a.Storage.DataDir,
				TempDir: a.Storage.TempDir,
			},
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

		return phelixerr.Newf(
			phelixerr.CodeServer,
			"server returned HTTP %d",
			resp.StatusCode,
		)
	}

	logs.Info("monitor", "successfully sent apps to server")
	return nil
}
