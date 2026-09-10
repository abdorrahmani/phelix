package grpc

import (
	"fmt"
	"strings"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/health"
)

// ProcessHealthCommand routes a HealthCommand to the appropriate handler and returns a HealthResult.
func ProcessHealthCommand(cmd *pb.HealthCommand) *pb.HealthResult {
	start := time.Now()

	switch c := cmd.Command.(type) {
	case *pb.HealthCommand_Set:
		return processHealthSet(cmd, c.Set, start)
	case *pb.HealthCommand_Add:
		return processHealthAdd(cmd, c.Add, start)
	case *pb.HealthCommand_Remove:
		return processHealthRemove(cmd, c.Remove, start)
	case *pb.HealthCommand_List:
		return processHealthList(cmd, c.List, start)
	case *pb.HealthCommand_Status:
		return processHealthStatus(cmd, c.Status, start)
	case *pb.HealthCommand_Watch:
		// Watch is handled separately via streaming - return initial status
		return processHealthStatus(cmd, &pb.HealthStatusCommand{
			AppId:   c.Watch.AppId,
			AppName: c.Watch.AppName,
		}, start)
	case *pb.HealthCommand_DaemonStatus:
		return processHealthDaemonStatus(cmd, start)
	case *pb.HealthCommand_Check:
		return processHealthCheck(cmd, c.Check, start)
	default:
		return &pb.HealthResult{
			RequestId: cmd.GetRequestId(),
			ServerId:  cmd.GetServerId(),
			Command:   "unknown",
			Success:   false,
			Timestamp: time.Now().UnixMilli(),
			Error:     "unknown command type",
		}
	}
}

// processHealthSet initializes health checks for an application.
func processHealthSet(cmd *pb.HealthCommand, c *pb.HealthSetCommand, start time.Time) *pb.HealthResult {
	if err := app.Manager.LoadState(); err != nil {
		return newHealthResult(cmd, false, start, fmt.Sprintf("failed to load app state: %v", err))
	}

	appID, err := resolveAppIDFromRequest(c.GetAppId(), c.GetAppName())
	if err != nil {
		return newHealthResult(cmd, false, start, err.Error())
	}

	appInfo := findAppByID(appID)
	if appInfo == nil {
		return newHealthResult(cmd, false, start, fmt.Sprintf("app not found: %s", c.GetAppId()))
	}

	configMgr, err := health.InitConfigManager()
	if err != nil {
		return newHealthResult(cmd, false, start, fmt.Sprintf("failed to initialize config: %v", err))
	}

	config := configMgr.GetConfig(appID)
	if config == nil {
		config = &health.AppHealthConfig{
			AppID:     appID,
			AppName:   appInfo.Name,
			Endpoints: make(map[string]*health.HealthCheckConfig),
			Enabled:   true,
		}
	}

	interval := c.GetInterval()
	if interval == "" {
		interval = "10s"
	}
	if _, err := time.ParseDuration(interval); err != nil {
		return newHealthResult(cmd, false, start, fmt.Sprintf("invalid interval format: %s", interval))
	}

	retries := int(c.GetRetries())
	if retries <= 0 {
		retries = 3
	}

	timeout := c.GetTimeout()
	if timeout == "" {
		timeout = "10s"
	}
	if _, err := time.ParseDuration(timeout); err != nil {
		return newHealthResult(cmd, false, start, fmt.Sprintf("invalid timeout format: %s", timeout))
	}

	expectedCodes := c.GetExpectedCodes()
	if expectedCodes == "" {
		expectedCodes = "200-299"
	}

	if c.GetPath() != "" {
		endpointName := "default"
		url := fmt.Sprintf("http://localhost:%d%s", appInfo.Port, c.GetPath())
		config.Endpoints[endpointName] = &health.HealthCheckConfig{
			Name:          endpointName,
			URL:           url,
			Interval:      interval,
			Retries:       retries,
			ExpectedCodes: expectedCodes,
			Timeout:       timeout,
		}
	}

	mode := health.TierModeAuto
	if c.GetMode() != "" {
		switch health.DeployTierMode(c.GetMode()) {
		case health.TierModeAuto, health.TierModeHTTP, health.TierModeTCPOnly, health.TierModeNone:
			mode = health.DeployTierMode(c.GetMode())
		default:
			return newHealthResult(cmd, false, start, fmt.Sprintf("invalid mode %q: must be one of auto, http, tcp-only, none", c.GetMode()))
		}
	}
	// Only Mode/Path are persisted for deploy: the interval/retries/timeout
	// fields here describe the monitoring daemon, not the deploy probe (see
	// cmd/health.go) — copying them made Tier 1 deploys mathematically
	// time out. Deploy defaults (1s/5/30s) apply instead.
	config.DeployTier = &health.DeployTierConfig{
		Mode: mode,
		Path: c.GetPath(),
	}

	if err := configMgr.SaveConfig(appID, config); err != nil {
		return newHealthResult(cmd, false, start, fmt.Sprintf("failed to save config: %v", err))
	}
	syncHealthDaemon()

	return newHealthResult(cmd, true, start, "")
}

// processHealthAdd adds a health check endpoint.
func processHealthAdd(cmd *pb.HealthCommand, c *pb.HealthAddCommand, start time.Time) *pb.HealthResult {
	if err := app.Manager.LoadState(); err != nil {
		return newHealthResult(cmd, false, start, fmt.Sprintf("failed to load app state: %v", err))
	}

	appID, err := resolveAppIDFromRequest(c.GetAppId(), c.GetAppName())
	if err != nil {
		return newHealthResult(cmd, false, start, err.Error())
	}

	appInfo := findAppByID(appID)
	if appInfo == nil {
		return newHealthResult(cmd, false, start, fmt.Sprintf("app not found: %s", c.GetAppId()))
	}

	if c.GetName() == "" {
		return newHealthResult(cmd, false, start, "endpoint name is required")
	}
	if c.GetUrl() == "" {
		return newHealthResult(cmd, false, start, "endpoint URL is required")
	}

	configMgr, err := health.InitConfigManager()
	if err != nil {
		return newHealthResult(cmd, false, start, fmt.Sprintf("failed to initialize config: %v", err))
	}

	config := configMgr.GetConfig(appID)
	if config == nil {
		config = &health.AppHealthConfig{
			AppID:     appID,
			AppName:   appInfo.Name,
			Endpoints: make(map[string]*health.HealthCheckConfig),
			Enabled:   true,
		}
	}

	if _, exists := config.Endpoints[c.GetName()]; exists {
		return newHealthResult(cmd, false, start, fmt.Sprintf("endpoint '%s' already exists", c.GetName()))
	}

	interval := c.GetInterval()
	if interval == "" {
		interval = "10s"
	}
	if _, err := time.ParseDuration(interval); err != nil {
		return newHealthResult(cmd, false, start, fmt.Sprintf("invalid interval format: %s", interval))
	}

	retries := int(c.GetRetries())
	if retries <= 0 {
		retries = 3
	}

	timeout := c.GetTimeout()
	if timeout == "" {
		timeout = "10s"
	}
	if _, err := time.ParseDuration(timeout); err != nil {
		return newHealthResult(cmd, false, start, fmt.Sprintf("invalid timeout format: %s", timeout))
	}

	expectedCodes := c.GetExpectedCodes()
	if expectedCodes == "" {
		expectedCodes = "200-299"
	}

	config.Endpoints[c.GetName()] = &health.HealthCheckConfig{
		Name:          c.GetName(),
		URL:           c.GetUrl(),
		Interval:      interval,
		Retries:       retries,
		ExpectedCodes: expectedCodes,
		Timeout:       timeout,
	}

	if err := configMgr.SaveConfig(appID, config); err != nil {
		return newHealthResult(cmd, false, start, fmt.Sprintf("failed to save config: %v", err))
	}
	syncHealthDaemon()

	return newHealthResult(cmd, true, start, "")
}

// processHealthRemove removes a health check endpoint.
func processHealthRemove(cmd *pb.HealthCommand, c *pb.HealthRemoveCommand, start time.Time) *pb.HealthResult {
	if err := app.Manager.LoadState(); err != nil {
		return newHealthResult(cmd, false, start, fmt.Sprintf("failed to load app state: %v", err))
	}

	appID, err := resolveAppIDFromRequest(c.GetAppId(), c.GetAppName())
	if err != nil {
		return newHealthResult(cmd, false, start, err.Error())
	}

	if c.GetName() == "" {
		return newHealthResult(cmd, false, start, "endpoint name is required")
	}

	configMgr, err := health.InitConfigManager()
	if err != nil {
		return newHealthResult(cmd, false, start, fmt.Sprintf("failed to initialize config: %v", err))
	}

	config := configMgr.GetConfig(appID)
	if config == nil {
		return newHealthResult(cmd, false, start, "no health checks configured for this app")
	}

	if _, exists := config.Endpoints[c.GetName()]; !exists {
		return newHealthResult(cmd, false, start, fmt.Sprintf("endpoint '%s' not found", c.GetName()))
	}

	delete(config.Endpoints, c.GetName())
	if err := configMgr.SaveConfig(appID, config); err != nil {
		return newHealthResult(cmd, false, start, fmt.Sprintf("failed to save config: %v", err))
	}
	syncHealthDaemon()

	return newHealthResult(cmd, true, start, "")
}

// processHealthList returns all configured endpoints.
func processHealthList(cmd *pb.HealthCommand, c *pb.HealthListCommand, start time.Time) *pb.HealthResult {
	if err := app.Manager.LoadState(); err != nil {
		return newHealthResult(cmd, false, start, fmt.Sprintf("failed to load app state: %v", err))
	}

	appID, err := resolveAppIDFromRequest(c.GetAppId(), c.GetAppName())
	if err != nil {
		return newHealthResult(cmd, false, start, err.Error())
	}

	configMgr, err := health.InitConfigManager()
	if err != nil {
		return newHealthResult(cmd, false, start, fmt.Sprintf("failed to initialize config: %v", err))
	}

	config := configMgr.GetConfig(appID)
	if config == nil || len(config.Endpoints) == 0 {
		result := newHealthResult(cmd, true, start, "")
		result.Result = &pb.HealthResult_ListResult{
			ListResult: &pb.HealthResultListResult{},
		}
		return result
	}

	var endpoints []*pb.EndpointConfig
	for _, ep := range config.Endpoints {
		endpoints = append(endpoints, &pb.EndpointConfig{
			EndpointId:    ep.ID,
			Name:          ep.Name,
			Url:           ep.URL,
			Interval:      ep.Interval,
			Retries:       int32(ep.Retries),
			ExpectedCodes: ep.ExpectedCodes,
			Timeout:       ep.Timeout,
		})
	}

	var deployTier *pb.DeployTierConfigProto
	if config.DeployTier != nil {
		deployTier = &pb.DeployTierConfigProto{
			Mode:     string(config.DeployTier.Mode),
			Path:     config.DeployTier.Path,
			Interval: config.DeployTier.Interval,
			Retries:  int32(config.DeployTier.Retries),
			Timeout:  config.DeployTier.Timeout,
		}
	}

	result := newHealthResult(cmd, true, start, "")
	result.Result = &pb.HealthResult_ListResult{
		ListResult: &pb.HealthResultListResult{
			Endpoints:  endpoints,
			DeployTier: deployTier,
		},
	}
	return result
}

// processHealthStatus returns the current health status.
func processHealthStatus(cmd *pb.HealthCommand, c *pb.HealthStatusCommand, start time.Time) *pb.HealthResult {
	if err := app.Manager.LoadState(); err != nil {
		return newHealthResult(cmd, false, start, fmt.Sprintf("failed to load app state: %v", err))
	}

	appID, err := resolveAppIDFromRequest(c.GetAppId(), c.GetAppName())
	if err != nil {
		return newHealthResult(cmd, false, start, err.Error())
	}

	globalDaemon := health.GetGlobalDaemon()
	daemon, err := globalDaemon.GetDaemon()
	if err != nil {
		return newHealthResult(cmd, false, start, fmt.Sprintf("health daemon not ready: %v", err))
	}

	// Built from the canonical snapshot rather than from the daemon's result map,
	// so every CONFIGURED endpoint is listed — an endpoint that has not been
	// probed yet reports UNKNOWN instead of being omitted, matching what
	// `phelix health status` prints locally.
	snap := daemon.SnapshotForApp(appID, c.GetAppName())
	var endpoints []*pb.EndpointHealthStatus
	for _, e := range snap.EndpointList() {
		ep := &pb.EndpointHealthStatus{
			EndpointId: e.Config.ID,
			Name:       e.Config.Name,
			Url:        e.Config.URL,
			Status:     health.StatusUnknown,
		}
		if r := e.Result; r != nil {
			ep.Status = r.Status
			ep.CheckedAt = r.CheckedAt.UnixMilli()
			if r.StatusCode != nil {
				ep.StatusCode = int32(*r.StatusCode)
			}
			if r.LatencyMs != nil {
				ep.LatencyMs = *r.LatencyMs
			}
			if r.Error != nil {
				ep.Error = *r.Error
			}
		}
		endpoints = append(endpoints, ep)
	}

	result := newHealthResult(cmd, true, start, "")
	result.Result = &pb.HealthResult_StatusResult{
		StatusResult: &pb.HealthResultStatusResult{
			Endpoints: endpoints,
		},
	}
	return result
}

// processHealthDaemonStatus returns the daemon status.
func processHealthDaemonStatus(cmd *pb.HealthCommand, start time.Time) *pb.HealthResult {
	globalDaemon := health.GetGlobalDaemon()
	running := globalDaemon.IsRunning()

	result := newHealthResult(cmd, true, start, "")

	daemonResult := &pb.HealthResultDaemonStatusResult{
		DaemonRunning: running,
	}

	if running {
		daemon, err := globalDaemon.GetDaemon()
		if err == nil {
			allStatus := daemon.GetAllStatus()
			totalEndpoints := 0
			healthyEndpoints := 0
			for _, endpoints := range allStatus {
				totalEndpoints += len(endpoints)
				for _, r := range endpoints {
					if r.Status == "UP" {
						healthyEndpoints++
					}
				}
			}
			daemonResult.TotalApps = int32(len(allStatus))
			daemonResult.TotalEndpoints = int32(totalEndpoints)
			daemonResult.HealthyEndpoints = int32(healthyEndpoints)
			daemonResult.UnhealthyEndpoints = int32(totalEndpoints - healthyEndpoints)
			result.Message = fmt.Sprintf("daemon running: %d apps, %d/%d endpoints healthy",
				len(allStatus), healthyEndpoints, totalEndpoints)
		}
	} else {
		result.Message = "daemon not running"
	}

	result.Result = &pb.HealthResult_DaemonStatusResult{
		DaemonStatusResult: daemonResult,
	}
	return result
}

// processHealthCheck performs a one-shot health check.
func processHealthCheck(cmd *pb.HealthCommand, c *pb.HealthCheckCommand, start time.Time) *pb.HealthResult {
	if c.GetUrl() == "" {
		return newHealthResult(cmd, false, start, "URL is required")
	}

	expectedCodes := c.GetExpectedCodes()
	if expectedCodes == "" {
		expectedCodes = "200-299"
	}

	checker := health.NewChecker()
	config := &health.HealthCheckConfig{
		Name:          "remote-check",
		URL:           c.GetUrl(),
		ExpectedCodes: expectedCodes,
		Timeout:       c.GetTimeout(),
	}

	healthResult := checker.Check(config)

	result := newHealthResult(cmd, healthResult.Status == "UP", start, "")
	result.AppId = c.GetAppId()
	result.AppName = c.GetAppName()

	checkResult := &pb.HealthResultCheckResult{
		Url:    c.GetUrl(),
		Status: healthResult.Status,
	}
	if healthResult.StatusCode != nil {
		checkResult.StatusCode = int32(*healthResult.StatusCode)
	}
	if healthResult.LatencyMs != nil {
		checkResult.LatencyMs = *healthResult.LatencyMs
	}
	if healthResult.Error != nil {
		checkResult.Error = *healthResult.Error
	}

	result.Result = &pb.HealthResult_CheckResult{
		CheckResult: checkResult,
	}
	return result
}

// ProcessHealthWatch returns a channel that streams health updates for the given app.
// The caller should read from the channel and send each update through the AgentStream.
// The channel closes when the context is cancelled or the app is removed.
func ProcessHealthWatch(appID, appName string) (<-chan *pb.HealthStatusUpdate, error) {
	if err := app.Manager.LoadState(); err != nil {
		return nil, fmt.Errorf("failed to load app state: %w", err)
	}

	resolvedID, err := resolveAppIDFromRequest(appID, appName)
	if err != nil {
		return nil, err
	}

	globalDaemon := health.GetGlobalDaemon()
	daemon, err := globalDaemon.GetDaemon()
	if err != nil {
		return nil, fmt.Errorf("health daemon not ready: %w", err)
	}

	updateChan := make(chan *pb.HealthStatusUpdate, 100)

	// Register listener for real-time updates. This is a live view for one
	// interactive watch session — it is NOT the synchronization channel, so it
	// reports whatever has been observed and nothing else. The backend must
	// persist endpoint state from AppHealthSnapshot instead.
	listener := func(event interface{}) {
		snap := daemon.SnapshotForApp(resolvedID, appName)
		for _, e := range snap.EndpointList() {
			if e.Result == nil {
				continue
			}
			update := &pb.HealthStatusUpdate{
				AppId:        resolvedID,
				AppName:      snap.AppName,
				EndpointId:   e.Config.ID,
				EndpointName: e.Config.Name,
				Url:          e.Config.URL,
				Status:       e.Result.Status,
				CheckedAt:    e.Result.CheckedAt.UnixMilli(),
				IsHealthy:    e.Result.Status == health.StatusUp,
			}
			if e.Result.StatusCode != nil {
				update.StatusCode = int32(*e.Result.StatusCode)
			}
			if e.Result.LatencyMs != nil {
				update.LatencyMs = *e.Result.LatencyMs
			}
			if e.Result.Error != nil {
				update.Error = *e.Result.Error
			}
			select {
			case updateChan <- update:
			default:
				// Channel full, skip
			}
		}
	}
	daemon.AddEventListener(listener)

	return updateChan, nil
}

// ============================================================================
// Helper functions
// ============================================================================

// resolveAppIDFromRequest resolves an app ID or name to the actual app ID.
//
// app_id is the identity, so it wins whenever it names a known app. A request
// that carries only a name — or an app_id the agent does not know together with a
// name it does — resolves through the name. Anything else is returned verbatim so
// the caller's own lookup produces the "app not found" error.
func resolveAppIDFromRequest(appID, appName string) (string, error) {
	if appID == "" && appName == "" {
		return "", fmt.Errorf("app_id or app_name is required")
	}

	apps := app.Manager.ListApplications()
	if appID != "" {
		for _, a := range apps {
			if a.ID == appID {
				return appID, nil
			}
		}
	}
	if appName != "" {
		for _, a := range apps {
			if a.Name == appName {
				return a.ID, nil
			}
		}
	}
	return appID, nil
}

// syncHealthDaemon nudges the health daemon to reconcile immediately after a
// backend-issued configuration change, so a new endpoint starts being checked
// (and reported) without waiting out the reconcile interval. A no-op in a process
// with no running daemon. Sync is idempotent, so racing the periodic reconcile is
// harmless.
func syncHealthDaemon() {
	if d := health.RunningDaemon(); d != nil {
		go d.Sync()
	}
}

// findAppByID finds an app by its ID.
func findAppByID(appID string) *app.AppListItem {
	apps := app.Manager.ListApplications()
	for _, a := range apps {
		if a.ID == appID {
			return &a
		}
	}
	return nil
}

// newHealthResult creates a standard HealthResult from a HealthCommand.
func newHealthResult(cmd *pb.HealthCommand, success bool, start time.Time, errMsg string) *pb.HealthResult {
	command := "unknown"
	if cmd.Command != nil {
		switch cmd.Command.(type) {
		case *pb.HealthCommand_Set:
			command = "health_set"
		case *pb.HealthCommand_Add:
			command = "health_add"
		case *pb.HealthCommand_Remove:
			command = "health_remove"
		case *pb.HealthCommand_List:
			command = "health_list"
		case *pb.HealthCommand_Status:
			command = "health_status"
		case *pb.HealthCommand_Watch:
			command = "health_watch"
		case *pb.HealthCommand_DaemonStatus:
			command = "health_daemon_status"
		case *pb.HealthCommand_Check:
			command = "health_check"
		}
	}

	return &pb.HealthResult{
		RequestId:     cmd.GetRequestId(),
		ServerId:      cmd.GetServerId(),
		Command:       command,
		Success:       success,
		ExecutionTime: time.Since(start).Milliseconds(),
		Timestamp:     time.Now().UnixMilli(),
		Error:         errMsg,
		Diagnostics:   getDiagnostics(errMsg),
	}
}

// getDiagnostics generates diagnostic information from an error message.
func getDiagnostics(errMsg string) string {
	if errMsg == "" {
		return "ok"
	}
	var diags []string
	if strings.Contains(errMsg, "not found") {
		diags = append(diags, "app_or_endpoint_not_found")
	}
	if strings.Contains(errMsg, "config") {
		diags = append(diags, "configuration_error")
	}
	if strings.Contains(errMsg, "daemon") {
		diags = append(diags, "daemon_not_ready")
	}
	if strings.Contains(errMsg, "invalid") {
		diags = append(diags, "invalid_parameters")
	}
	if len(diags) == 0 {
		return "general_error"
	}
	return strings.Join(diags, ",")
}
