package grpc

import (
	"fmt"
	"sort"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/health"
)

// ProcessHealthCommand routes a HealthCommand to the appropriate handler and
// returns a HealthResult.
//
// Mutating commands (set/add/remove/update/set_enabled) are made idempotent by
// request_id: a redelivery of the same command replays the recorded result
// instead of re-applying it, and a request_id reused for a different payload is
// rejected with IDEMPOTENCY_CONFLICT. See healthResults (health_command_cache.go)
// and docs/health-backend-contract.md. Reads and recheck carry no side effect and
// are answered directly.
func ProcessHealthCommand(cmd *pb.HealthCommand) *pb.HealthResult {
	start := time.Now()

	if mutatingHealthCommand(cmd) {
		replay, cached, err := healthResults.lookup(cmd)
		if err != nil {
			return healthErr(cmd, start, err)
		}
		if replay {
			return cached
		}
		result := dispatchHealthCommand(cmd, start)
		healthResults.store(cmd, result)
		return result
	}

	return dispatchHealthCommand(cmd, start)
}

// mutatingHealthCommand reports whether a command changes persisted health
// configuration (and is therefore idempotency-cached). recheck probes but does
// not mutate config; reads obviously do not.
func mutatingHealthCommand(cmd *pb.HealthCommand) bool {
	switch cmd.Command.(type) {
	case *pb.HealthCommand_Set, *pb.HealthCommand_Add, *pb.HealthCommand_Remove,
		*pb.HealthCommand_Update, *pb.HealthCommand_SetEnabled:
		return true
	}
	return false
}

// dispatchHealthCommand runs one command's handler.
func dispatchHealthCommand(cmd *pb.HealthCommand, start time.Time) *pb.HealthResult {
	switch c := cmd.Command.(type) {
	case *pb.HealthCommand_Set:
		return processHealthSet(cmd, c.Set, start)
	case *pb.HealthCommand_Add:
		return processHealthAdd(cmd, c.Add, start)
	case *pb.HealthCommand_Remove:
		return processHealthRemove(cmd, c.Remove, start)
	case *pb.HealthCommand_Update:
		return processHealthUpdate(cmd, c.Update, start)
	case *pb.HealthCommand_SetEnabled:
		return processHealthSetEnabled(cmd, c.SetEnabled, start)
	case *pb.HealthCommand_Recheck:
		return processHealthRecheck(cmd, c.Recheck, start)
	case *pb.HealthCommand_List:
		return processHealthList(cmd, c.List, start)
	case *pb.HealthCommand_Status:
		return processHealthStatus(cmd, c.Status, start)
	case *pb.HealthCommand_Watch:
		// Watch streams separately (handleHealthWatch); the one-shot path returns
		// the current status as the initial frame.
		return processHealthStatus(cmd, &pb.HealthStatusCommand{
			AppId:   c.Watch.AppId,
			AppName: c.Watch.AppName,
		}, start)
	case *pb.HealthCommand_DaemonStatus:
		return processHealthDaemonStatus(cmd, start)
	case *pb.HealthCommand_Check:
		return processHealthCheck(cmd, c.Check, start)
	default:
		return healthErr(cmd, start, phelixerr.New(phelixerr.CodeInvalidArgument, "unknown health command type"))
	}
}

// processHealthSet initializes health checks for an application, including the
// deploy-tier probe configuration.
func processHealthSet(cmd *pb.HealthCommand, c *pb.HealthSetCommand, start time.Time) *pb.HealthResult {
	appInfo, err := loadApp(c.GetAppId(), c.GetAppName())
	if err != nil {
		return healthErr(cmd, start, err)
	}

	interval, timeout, retries, codes, err := normalizeEndpointParams(c.GetInterval(), c.GetTimeout(), int(c.GetRetries()), c.GetExpectedCodes())
	if err != nil {
		return healthErr(cmd, start, err)
	}
	mode, err := parseTierMode(c.GetMode())
	if err != nil {
		return healthErr(cmd, start, err)
	}
	// Deploy-tier knobs are optional; validate the durations when present.
	if err := validateOptionalDuration("deploy_interval", c.GetDeployInterval()); err != nil {
		return healthErr(cmd, start, err)
	}
	if err := validateOptionalDuration("deploy_timeout", c.GetDeployTimeout()); err != nil {
		return healthErr(cmd, start, err)
	}
	if c.GetDeployRetries() < 0 {
		return healthErr(cmd, start, phelixerr.Newf(phelixerr.CodeInvalidArgument, "deploy_retries must be >= 0, got %d", c.GetDeployRetries()))
	}

	configMgr, err := health.InitConfigManager()
	if err != nil {
		return healthErr(cmd, start, phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to initialize config", err))
	}

	_, err = configMgr.Mutate(appInfo.ID, appInfo.Name, func(config *health.AppHealthConfig) error {
		if c.GetPath() != "" {
			url := localhostURL(appInfo.Port, c.GetPath())
			ep := config.Endpoints["default"]
			if ep == nil {
				ep = &health.HealthCheckConfig{Name: "default"}
				config.Endpoints["default"] = ep
			}
			ep.URL, ep.Interval, ep.Retries, ep.ExpectedCodes, ep.Timeout = url, interval, retries, codes, timeout
		}
		// Only Mode/Path/deploy-tier knobs are persisted for deploy: the daemon
		// interval/retries/timeout above describe monitoring, not the deploy probe
		// (copying them made Tier 1 deploys time out — see cmd/health.go).
		config.DeployTier = &health.DeployTierConfig{
			Mode:     mode,
			Path:     c.GetPath(),
			Interval: c.GetDeployInterval(),
			Retries:  int(c.GetDeployRetries()),
			Timeout:  c.GetDeployTimeout(),
		}
		return nil
	})
	if err != nil {
		return healthErr(cmd, start, err)
	}
	syncHealthDaemon()
	return newHealthOK(cmd, start)
}

// processHealthAdd adds a health check endpoint. It is idempotent: re-adding an
// endpoint whose configuration is identical succeeds (so a redelivery after an
// agent restart, which lost the request_id cache, does not fail with
// ALREADY_EXISTS); only a genuine same-name/different-config collision is
// rejected.
func processHealthAdd(cmd *pb.HealthCommand, c *pb.HealthAddCommand, start time.Time) *pb.HealthResult {
	appInfo, err := loadApp(c.GetAppId(), c.GetAppName())
	if err != nil {
		return healthErr(cmd, start, err)
	}
	if c.GetName() == "" {
		return healthErr(cmd, start, phelixerr.New(phelixerr.CodeInvalidArgument, "endpoint name is required"))
	}
	if c.GetUrl() == "" {
		return healthErr(cmd, start, phelixerr.New(phelixerr.CodeInvalidArgument, "endpoint URL is required"))
	}
	interval, timeout, retries, codes, err := normalizeEndpointParams(c.GetInterval(), c.GetTimeout(), int(c.GetRetries()), c.GetExpectedCodes())
	if err != nil {
		return healthErr(cmd, start, err)
	}

	configMgr, err := health.InitConfigManager()
	if err != nil {
		return healthErr(cmd, start, phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to initialize config", err))
	}

	_, err = configMgr.Mutate(appInfo.ID, appInfo.Name, func(config *health.AppHealthConfig) error {
		if ep, exists := config.Endpoints[c.GetName()]; exists {
			if endpointConfigEqual(ep, c.GetUrl(), interval, retries, codes, timeout) {
				return nil // idempotent re-add
			}
			return phelixerr.Newf(phelixerr.CodeAlreadyExists, "endpoint %q already exists", c.GetName())
		}
		config.Endpoints[c.GetName()] = &health.HealthCheckConfig{
			Name: c.GetName(), URL: c.GetUrl(), Interval: interval,
			Retries: retries, ExpectedCodes: codes, Timeout: timeout,
		}
		return nil
	})
	if err != nil {
		return healthErr(cmd, start, err)
	}
	syncHealthDaemon()
	return newHealthOK(cmd, start)
}

// processHealthRemove removes a health check endpoint by name. It is
// idempotent: removing an endpoint that is already gone succeeds (so a
// redelivery never fails with NOT_FOUND).
func processHealthRemove(cmd *pb.HealthCommand, c *pb.HealthRemoveCommand, start time.Time) *pb.HealthResult {
	appInfo, err := loadApp(c.GetAppId(), c.GetAppName())
	if err != nil {
		return healthErr(cmd, start, err)
	}
	if c.GetName() == "" {
		return healthErr(cmd, start, phelixerr.New(phelixerr.CodeInvalidArgument, "endpoint name is required"))
	}

	configMgr, err := health.InitConfigManager()
	if err != nil {
		return healthErr(cmd, start, phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to initialize config", err))
	}

	_, err = configMgr.Mutate(appInfo.ID, appInfo.Name, func(config *health.AppHealthConfig) error {
		delete(config.Endpoints, c.GetName()) // delete of an absent key is a no-op
		return nil
	})
	if err != nil {
		return healthErr(cmd, start, err)
	}
	syncHealthDaemon()
	return newHealthOK(cmd, start)
}

// processHealthUpdate edits one endpoint in place, keyed by endpoint_id, so its
// stable identity (and the backend's record and history) survives a rename or
// URL change. The incoming fields are the complete desired configuration for
// that endpoint.
func processHealthUpdate(cmd *pb.HealthCommand, c *pb.HealthUpdateCommand, start time.Time) *pb.HealthResult {
	appInfo, err := loadApp(c.GetAppId(), c.GetAppName())
	if err != nil {
		return healthErr(cmd, start, err)
	}
	if c.GetEndpointId() == "" {
		return healthErr(cmd, start, phelixerr.New(phelixerr.CodeInvalidArgument, "endpoint_id is required"))
	}
	if c.GetName() == "" {
		return healthErr(cmd, start, phelixerr.New(phelixerr.CodeInvalidArgument, "endpoint name is required"))
	}
	if c.GetUrl() == "" {
		return healthErr(cmd, start, phelixerr.New(phelixerr.CodeInvalidArgument, "endpoint URL is required"))
	}
	interval, timeout, retries, codes, err := normalizeEndpointParams(c.GetInterval(), c.GetTimeout(), int(c.GetRetries()), c.GetExpectedCodes())
	if err != nil {
		return healthErr(cmd, start, err)
	}

	configMgr, err := health.InitConfigManager()
	if err != nil {
		return healthErr(cmd, start, phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to initialize config", err))
	}

	_, err = configMgr.Mutate(appInfo.ID, appInfo.Name, func(config *health.AppHealthConfig) error {
		oldName := ""
		for name, ep := range config.Endpoints {
			if ep != nil && ep.ID == c.GetEndpointId() {
				oldName = name
				break
			}
		}
		if oldName == "" {
			return phelixerr.Newf(phelixerr.CodeNotFound, "endpoint %q not found", c.GetEndpointId())
		}
		// A rename to a name already taken by a DIFFERENT endpoint would collide.
		if c.GetName() != oldName {
			if _, taken := config.Endpoints[c.GetName()]; taken {
				return phelixerr.Newf(phelixerr.CodeAlreadyExists, "endpoint %q already exists", c.GetName())
			}
			delete(config.Endpoints, oldName)
		}
		config.Endpoints[c.GetName()] = &health.HealthCheckConfig{
			ID: c.GetEndpointId(), Name: c.GetName(), URL: c.GetUrl(), Interval: interval,
			Retries: retries, ExpectedCodes: codes, Timeout: timeout,
		}
		return nil
	})
	if err != nil {
		return healthErr(cmd, start, err)
	}
	syncHealthDaemon()
	return newHealthOK(cmd, start)
}

// processHealthSetEnabled toggles the app's health configuration. Disabling
// stops the daemon's probes (config and endpoints are kept) and is reflected in
// the next AppHealthSnapshot. It requires an existing configuration.
func processHealthSetEnabled(cmd *pb.HealthCommand, c *pb.HealthSetEnabledCommand, start time.Time) *pb.HealthResult {
	appInfo, err := loadApp(c.GetAppId(), c.GetAppName())
	if err != nil {
		return healthErr(cmd, start, err)
	}

	configMgr, err := health.InitConfigManager()
	if err != nil {
		return healthErr(cmd, start, phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to initialize config", err))
	}
	if configMgr.GetConfig(appInfo.ID) == nil {
		return healthErr(cmd, start, phelixerr.New(phelixerr.CodeNotFound, "no health configuration for this app"))
	}

	_, err = configMgr.Mutate(appInfo.ID, appInfo.Name, func(config *health.AppHealthConfig) error {
		config.Enabled = c.GetEnabled()
		return nil
	})
	if err != nil {
		return healthErr(cmd, start, err)
	}
	syncHealthDaemon()
	return newHealthOK(cmd, start)
}

// processHealthRecheck probes configured endpoints immediately and returns the
// fresh results, without waiting for the daemon's interval. endpoint_id selects
// one endpoint; empty means all. It does not mutate config; the daemon's own
// state converges on its next tick.
func processHealthRecheck(cmd *pb.HealthCommand, c *pb.HealthRecheckCommand, start time.Time) *pb.HealthResult {
	appInfo, err := loadApp(c.GetAppId(), c.GetAppName())
	if err != nil {
		return healthErr(cmd, start, err)
	}
	configMgr, err := health.InitConfigManager()
	if err != nil {
		return healthErr(cmd, start, phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to initialize config", err))
	}
	config := configMgr.GetConfig(appInfo.ID)
	if config == nil || len(config.Endpoints) == 0 {
		return healthErr(cmd, start, phelixerr.New(phelixerr.CodeNotFound, "no health checks configured for this app"))
	}

	checker := health.NewChecker()
	var endpoints []*pb.EndpointHealthStatus
	for _, name := range sortedNames(config.Endpoints) {
		ep := config.Endpoints[name]
		if c.GetEndpointId() != "" && ep.ID != c.GetEndpointId() {
			continue
		}
		r := checker.Check(ep)
		status := &pb.EndpointHealthStatus{
			EndpointId: ep.ID,
			Name:       ep.Name,
			Url:        ep.URL,
			Status:     r.Status,
			CheckedAt:  r.CheckedAt.UnixMilli(),
		}
		if r.StatusCode != nil {
			status.StatusCode = int32(*r.StatusCode)
		}
		if r.LatencyMs != nil {
			status.LatencyMs = *r.LatencyMs
		}
		if r.Error != nil {
			status.Error = phelixerr.Redact(*r.Error)
		}
		endpoints = append(endpoints, status)
	}
	if c.GetEndpointId() != "" && len(endpoints) == 0 {
		return healthErr(cmd, start, phelixerr.Newf(phelixerr.CodeNotFound, "endpoint %q not found", c.GetEndpointId()))
	}

	result := newHealthOK(cmd, start)
	result.Result = &pb.HealthResult_StatusResult{
		StatusResult: &pb.HealthResultStatusResult{Endpoints: endpoints},
	}
	return result
}

// processHealthList returns all configured endpoints.
func processHealthList(cmd *pb.HealthCommand, c *pb.HealthListCommand, start time.Time) *pb.HealthResult {
	appID, err := resolveAppIDFromRequest(c.GetAppId(), c.GetAppName())
	if err != nil {
		return healthErr(cmd, start, err)
	}
	configMgr, err := health.InitConfigManager()
	if err != nil {
		return healthErr(cmd, start, phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to initialize config", err))
	}

	config := configMgr.GetConfig(appID)
	result := newHealthOK(cmd, start)
	if config == nil || len(config.Endpoints) == 0 {
		result.Result = &pb.HealthResult_ListResult{ListResult: &pb.HealthResultListResult{}}
		return result
	}

	var endpoints []*pb.EndpointConfig
	for _, name := range sortedNames(config.Endpoints) {
		ep := config.Endpoints[name]
		endpoints = append(endpoints, &pb.EndpointConfig{
			EndpointId: ep.ID, Name: ep.Name, Url: ep.URL, Interval: ep.Interval,
			Retries: int32(ep.Retries), ExpectedCodes: ep.ExpectedCodes, Timeout: ep.Timeout,
		})
	}
	var deployTier *pb.DeployTierConfigProto
	if config.DeployTier != nil {
		deployTier = &pb.DeployTierConfigProto{
			Mode: string(config.DeployTier.Mode), Path: config.DeployTier.Path,
			Interval: config.DeployTier.Interval, Retries: int32(config.DeployTier.Retries),
			Timeout: config.DeployTier.Timeout,
		}
	}
	result.Result = &pb.HealthResult_ListResult{
		ListResult: &pb.HealthResultListResult{Endpoints: endpoints, DeployTier: deployTier},
	}
	return result
}

// processHealthStatus returns the current health status, built from the
// canonical snapshot so every CONFIGURED endpoint is listed (an unprobed one
// reports UNKNOWN rather than being omitted).
func processHealthStatus(cmd *pb.HealthCommand, c *pb.HealthStatusCommand, start time.Time) *pb.HealthResult {
	appID, err := resolveAppIDFromRequest(c.GetAppId(), c.GetAppName())
	if err != nil {
		return healthErr(cmd, start, err)
	}
	globalDaemon := health.GetGlobalDaemon()
	daemon, err := globalDaemon.GetDaemon()
	if err != nil {
		return healthErr(cmd, start, phelixerr.Wrap(phelixerr.CodeUnavailable, "health daemon not ready", err))
	}

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
				ep.Error = phelixerr.Redact(*r.Error)
			}
		}
		endpoints = append(endpoints, ep)
	}

	result := newHealthOK(cmd, start)
	result.Result = &pb.HealthResult_StatusResult{
		StatusResult: &pb.HealthResultStatusResult{Endpoints: endpoints},
	}
	return result
}

// processHealthDaemonStatus returns the daemon status.
func processHealthDaemonStatus(cmd *pb.HealthCommand, start time.Time) *pb.HealthResult {
	globalDaemon := health.GetGlobalDaemon()
	running := globalDaemon.IsRunning()

	result := newHealthOK(cmd, start)
	daemonResult := &pb.HealthResultDaemonStatusResult{DaemonRunning: running}

	if running {
		if daemon, err := globalDaemon.GetDaemon(); err == nil {
			allStatus := daemon.GetAllStatus()
			totalEndpoints, healthyEndpoints := 0, 0
			for _, endpoints := range allStatus {
				totalEndpoints += len(endpoints)
				for _, r := range endpoints {
					if r.Status == health.StatusUp {
						healthyEndpoints++
					}
				}
			}
			daemonResult.TotalApps = int32(len(allStatus))
			daemonResult.TotalEndpoints = int32(totalEndpoints)
			daemonResult.HealthyEndpoints = int32(healthyEndpoints)
			daemonResult.UnhealthyEndpoints = int32(totalEndpoints - healthyEndpoints)
			result.Message = formatDaemonSummary(len(allStatus), healthyEndpoints, totalEndpoints)
		}
	} else {
		result.Message = "daemon not running"
	}

	result.Result = &pb.HealthResult_DaemonStatusResult{DaemonStatusResult: daemonResult}
	return result
}

// processHealthCheck performs a one-shot health check on an arbitrary URL.
func processHealthCheck(cmd *pb.HealthCommand, c *pb.HealthCheckCommand, start time.Time) *pb.HealthResult {
	if c.GetUrl() == "" {
		return healthErr(cmd, start, phelixerr.New(phelixerr.CodeInvalidArgument, "URL is required"))
	}
	codes := c.GetExpectedCodes()
	if codes == "" {
		codes = "200-299"
	}
	checker := health.NewChecker()
	healthResult := checker.Check(&health.HealthCheckConfig{
		Name: "remote-check", URL: c.GetUrl(), ExpectedCodes: codes, Timeout: c.GetTimeout(),
	})

	result := newHealthOK(cmd, start)
	result.Success = healthResult.Status == health.StatusUp
	result.AppId = c.GetAppId()
	result.AppName = c.GetAppName()
	checkResult := &pb.HealthResultCheckResult{Url: c.GetUrl(), Status: healthResult.Status}
	if healthResult.StatusCode != nil {
		checkResult.StatusCode = int32(*healthResult.StatusCode)
	}
	if healthResult.LatencyMs != nil {
		checkResult.LatencyMs = *healthResult.LatencyMs
	}
	if healthResult.Error != nil {
		checkResult.Error = phelixerr.Redact(*healthResult.Error)
	}
	result.Result = &pb.HealthResult_CheckResult{CheckResult: checkResult}
	return result
}

// ProcessHealthWatch returns a channel that streams health updates for the app
// and a cleanup func that MUST be called when the watch ends — it removes the
// daemon event listener this watch registered. Forgetting it is what used to
// leak one listener per remote watch for the life of the monitor process.
func ProcessHealthWatch(appID, appName string) (<-chan *pb.HealthStatusUpdate, func(), error) {
	if err := app.Manager.LoadState(); err != nil {
		return nil, nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load app state", err)
	}
	resolvedID, err := resolveAppIDFromRequest(appID, appName)
	if err != nil {
		return nil, nil, err
	}
	daemon, err := health.GetGlobalDaemon().GetDaemon()
	if err != nil {
		return nil, nil, phelixerr.Wrap(phelixerr.CodeUnavailable, "health daemon not ready", err)
	}

	updateChan := make(chan *pb.HealthStatusUpdate, 100)
	// Live view for one interactive watch session — NOT the sync channel. The
	// backend persists endpoint state from AppHealthSnapshot, not from here.
	listener := func(event interface{}) {
		snap := daemon.SnapshotForApp(resolvedID, appName)
		for _, e := range snap.EndpointList() {
			if e.Result == nil {
				continue
			}
			update := &pb.HealthStatusUpdate{
				AppId: resolvedID, AppName: snap.AppName, EndpointId: e.Config.ID,
				EndpointName: e.Config.Name, Url: e.Config.URL, Status: e.Result.Status,
				CheckedAt: e.Result.CheckedAt.UnixMilli(), IsHealthy: e.Result.Status == health.StatusUp,
			}
			if e.Result.StatusCode != nil {
				update.StatusCode = int32(*e.Result.StatusCode)
			}
			if e.Result.LatencyMs != nil {
				update.LatencyMs = *e.Result.LatencyMs
			}
			if e.Result.Error != nil {
				update.Error = phelixerr.Redact(*e.Result.Error)
			}
			select {
			case updateChan <- update:
			default: // channel full, skip (bounded, drop-newest)
			}
		}
	}
	id := daemon.AddEventListener(listener)
	return updateChan, func() { daemon.RemoveEventListener(id) }, nil
}

// ============================================================================
// Helpers
// ============================================================================

// loadApp loads app state and resolves (app_id|app_name) to the live app.
func loadApp(appID, appName string) (*app.AppListItem, error) {
	if err := app.Manager.LoadState(); err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load app state", err)
	}
	resolvedID, err := resolveAppIDFromRequest(appID, appName)
	if err != nil {
		return nil, err
	}
	appInfo := findAppByID(resolvedID)
	if appInfo == nil {
		hint := appID
		if hint == "" {
			hint = appName
		}
		return nil, phelixerr.Newf(phelixerr.CodeNotFound, "app not found: %s", hint)
	}
	return appInfo, nil
}

// resolveAppIDFromRequest resolves an app ID or name to the actual app ID.
// app_id wins when it names a known app; otherwise the name resolves it.
// Anything else is returned verbatim so the caller's lookup yields "not found".
func resolveAppIDFromRequest(appID, appName string) (string, error) {
	if appID == "" && appName == "" {
		return "", phelixerr.New(phelixerr.CodeInvalidArgument, "app_id or app_name is required")
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

// findAppByID finds an app by its ID.
func findAppByID(appID string) *app.AppListItem {
	for _, a := range app.Manager.ListApplications() {
		if a.ID == appID {
			return &a
		}
	}
	return nil
}

// syncHealthDaemon nudges the health daemon to reconcile immediately after a
// configuration change so a new/removed endpoint takes effect without waiting
// out the reconcile interval. A no-op without a running daemon; Sync is
// idempotent, so racing the periodic reconcile is harmless.
func syncHealthDaemon() {
	if d := health.RunningDaemon(); d != nil {
		go d.Sync()
	}
}

// normalizeEndpointParams validates and defaults the monitoring-daemon probe
// knobs, identically to the local CLI (cmd/health.go), so a bad remote value is
// rejected rather than persisted to break a later deploy.
func normalizeEndpointParams(interval, timeout string, retries int, codes string) (string, string, int, string, error) {
	if interval == "" {
		interval = "10s"
	} else if _, err := time.ParseDuration(interval); err != nil {
		return "", "", 0, "", phelixerr.Newf(phelixerr.CodeInvalidArgument, "invalid interval format: %s", interval)
	}
	if timeout == "" {
		timeout = "10s"
	} else if _, err := time.ParseDuration(timeout); err != nil {
		return "", "", 0, "", phelixerr.Newf(phelixerr.CodeInvalidArgument, "invalid timeout format: %s", timeout)
	}
	if retries <= 0 {
		retries = 3
	}
	if codes == "" {
		codes = "200-299"
	}
	return interval, timeout, retries, codes, nil
}

// parseTierMode validates a deploy-tier mode, matching cmd/health.go.
func parseTierMode(mode string) (health.DeployTierMode, error) {
	if mode == "" {
		return health.TierModeAuto, nil
	}
	switch health.DeployTierMode(mode) {
	case health.TierModeAuto, health.TierModeHTTP, health.TierModeTCPOnly, health.TierModeNone:
		return health.DeployTierMode(mode), nil
	}
	return "", phelixerr.Newf(phelixerr.CodeInvalidArgument, "invalid mode %q: must be one of auto, http, tcp-only, none", mode)
}

func validateOptionalDuration(field, v string) error {
	if v == "" {
		return nil
	}
	if _, err := time.ParseDuration(v); err != nil {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "invalid %s format: %s", field, v)
	}
	return nil
}

func endpointConfigEqual(ep *health.HealthCheckConfig, url, interval string, retries int, codes, timeout string) bool {
	return ep.URL == url && ep.Interval == interval && ep.Retries == retries &&
		ep.ExpectedCodes == codes && ep.Timeout == timeout
}

func localhostURL(port int, path string) string {
	return fmt.Sprintf("http://localhost:%d%s", port, path)
}

func sortedNames(m map[string]*health.HealthCheckConfig) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func formatDaemonSummary(apps, healthy, total int) string {
	return fmt.Sprintf("daemon running: %d apps, %d/%d endpoints healthy", apps, healthy, total)
}

// newHealthOK builds a success HealthResult skeleton; the caller attaches any
// result payload.
func newHealthOK(cmd *pb.HealthCommand, start time.Time) *pb.HealthResult {
	return &pb.HealthResult{
		RequestId:     cmd.GetRequestId(),
		ServerId:      cmd.GetServerId(),
		Command:       commandName(cmd),
		Success:       true,
		ExecutionTime: time.Since(start).Milliseconds(),
		Timestamp:     time.Now().UnixMilli(),
		Diagnostics:   "ok",
	}
}

// healthErr builds a failed HealthResult carrying the structured, stable error
// code and a redacted message — never raw tool output or secrets, and never a
// substring-matched guess (the code is authoritative).
func healthErr(cmd *pb.HealthCommand, start time.Time, err error) *pb.HealthResult {
	code := string(phelixerr.CodeOf(err))
	return &pb.HealthResult{
		RequestId:     cmd.GetRequestId(),
		ServerId:      cmd.GetServerId(),
		Command:       commandName(cmd),
		Success:       false,
		ExecutionTime: time.Since(start).Milliseconds(),
		Timestamp:     time.Now().UnixMilli(),
		Error:         phelixerr.Redact(err.Error()),
		ErrorCode:     code,
		Diagnostics:   code,
	}
}

func commandName(cmd *pb.HealthCommand) string {
	switch cmd.Command.(type) {
	case *pb.HealthCommand_Set:
		return "health_set"
	case *pb.HealthCommand_Add:
		return "health_add"
	case *pb.HealthCommand_Remove:
		return "health_remove"
	case *pb.HealthCommand_Update:
		return "health_update"
	case *pb.HealthCommand_SetEnabled:
		return "health_set_enabled"
	case *pb.HealthCommand_Recheck:
		return "health_recheck"
	case *pb.HealthCommand_List:
		return "health_list"
	case *pb.HealthCommand_Status:
		return "health_status"
	case *pb.HealthCommand_Watch:
		return "health_watch"
	case *pb.HealthCommand_DaemonStatus:
		return "health_daemon_status"
	case *pb.HealthCommand_Check:
		return "health_check"
	}
	return "unknown"
}
