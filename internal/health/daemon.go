package health

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/logs"
)

// Daemon manages health checks for all applications
type Daemon struct {
	mu             sync.RWMutex
	endpointStates map[string]map[string]*EndpointState // app ID -> endpoint name -> state
	// running is the set of live endpoint checkers, keyed by
	// checkerKey(appID, endpointName). It is reconciled against the persisted
	// configuration by Sync, so it is the daemon's view of "what is actually
	// being checked right now" — as opposed to endpointStates, which is "what
	// has been observed".
	running         map[string]*checkHandle
	checker         *Checker
	configManager   *ConfigManager
	stopChan        chan struct{}
	wg              sync.WaitGroup
	eventListeners  []EventListener
	reporter        HealthReporter
	isRunning       bool
	broadcastChan   chan *HealthCheckResult
	autoRestartChan chan *AutoRestartRecord
	lastCleanup     time.Time
}

// checkHandle is one running endpoint checker. cfg is held BY VALUE: a
// configuration change is a checker restart, never an in-place mutation of a
// struct another goroutine is reading.
type checkHandle struct {
	cfg  HealthCheckConfig
	stop chan struct{}
}

// checkerKey identifies a checker. The NUL separator keeps the key unambiguous
// for endpoint names containing the separator character.
func checkerKey(appID, endpointName string) string {
	return appID + "\x00" + endpointName
}

// syncInterval is how often the daemon reconciles its running checkers against
// the persisted health configuration. Configs are written by other processes
// (`phelix health set/add/remove`, `phelix build`) and by backend-issued health
// commands, so enumerating them once at startup would miss every change made
// afterwards. It is a var so tests can shorten it.
var syncInterval = 15 * time.Second

// EventListener is called when health check events occur
type EventListener func(event interface{})

// HealthReporter sends health data to the backend.
//
// It carries only auto-restart EVENTS. Configuration and runtime status reach the
// backend as AppHealthSnapshot messages over the monitor stream instead (see
// SnapshotForApp and docs/health-backend-contract.md): a snapshot is complete and
// idempotent, so it survives a disconnect, propagates deletions and needs no
// ordering against configuration. An auto-restart is a point-in-time fact that
// cannot be re-derived from a later snapshot, so it keeps its own channel.
type HealthReporter interface {
	SendAutoRestartEvent(record *AutoRestartRecord) error
}

// daemonPtr holds the singleton. It is atomic because the monitor process reads
// it from the gRPC snapshot sender while the health daemon is being initialized
// on another goroutine.
var daemonPtr atomic.Pointer[Daemon]

// InitDaemon initializes the health check daemon
func InitDaemon() (*Daemon, error) {
	configMgr, err := InitConfigManager()
	if err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "failed to initialize config manager")
	}

	d := &Daemon{
		endpointStates:  make(map[string]map[string]*EndpointState),
		running:         make(map[string]*checkHandle),
		checker:         NewChecker(),
		configManager:   configMgr,
		stopChan:        make(chan struct{}),
		broadcastChan:   make(chan *HealthCheckResult, 100),
		autoRestartChan: make(chan *AutoRestartRecord, 50),
	}
	daemonPtr.Store(d)

	// Start background workers
	d.startBroadcaster()
	d.startAutoRestarter()
	d.startMaintenanceWorker()

	return d, nil
}

// SetReporter sets the health reporter for sending results to the backend.
func (d *Daemon) SetReporter(r HealthReporter) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reporter = r
}

// GetDaemon returns the singleton daemon instance
func GetDaemon() *Daemon {
	d := daemonPtr.Load()
	if d == nil {
		panic("Daemon not initialized")
	}
	return d
}

// RunningDaemon returns the singleton daemon only when one is initialized AND
// running in this process, and nil otherwise. Callers outside the health package
// use it instead of GetDaemon so a plain CLI command — which has no daemon — is a
// no-op rather than a panic.
func RunningDaemon() *Daemon {
	d := daemonPtr.Load()
	if d == nil || !d.IsRunning() {
		return nil
	}
	return d
}

// Start begins health checking for all configured apps
func (d *Daemon) Start() error {
	d.mu.Lock()
	if d.isRunning {
		d.mu.Unlock()
		return phelixerr.New(phelixerr.CodeServer, "health daemon is already running")
	}
	d.isRunning = true
	d.mu.Unlock()

	logs.Info("health", "daemon starting...")

	if err := app.Manager.LoadState(); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "failed to load app state")
	}

	// One reconcile brings up every checker the persisted configuration asks
	// for; the worker keeps doing it as configuration changes underneath.
	d.Sync()
	d.startSyncWorker()

	return nil
}

// Sync reconciles the running endpoint checkers with the persisted health
// configuration: it starts checkers for endpoints that gained a configuration,
// restarts checkers whose configuration changed, and stops checkers whose
// endpoint (or whole app) was removed.
//
// It is safe to call at any time and idempotent — calling it twice with an
// unchanged configuration does nothing the second time.
func (d *Daemon) Sync() {
	if err := d.configManager.Reload(); err != nil {
		// Keep the cached configuration rather than tearing every checker down
		// because one read failed; the next tick retries.
		logs.Warning("health", "failed to reload health configs: %v", err)
	}

	type wanted struct {
		appID, appName, endpoint string
		cfg                      HealthCheckConfig
	}
	var desired []wanted
	for _, item := range app.Manager.ListApplications() {
		config := d.configManager.GetConfig(item.ID)
		if config == nil || !config.Enabled {
			continue
		}
		for _, name := range sortedEndpointNames(config.Endpoints) {
			ep := config.Endpoints[name]
			if ep == nil {
				continue
			}
			desired = append(desired, wanted{item.ID, item.Name, name, *ep})
		}
	}

	type launch struct {
		wanted
		handle *checkHandle
	}

	d.mu.Lock()
	keep := make(map[string]bool, len(desired))
	var starting []launch
	for _, w := range desired {
		key := checkerKey(w.appID, w.endpoint)
		keep[key] = true
		if h := d.running[key]; h != nil {
			if h.cfg == w.cfg {
				continue // unchanged
			}
			close(h.stop) // configuration edited: replace the checker
		}
		h := &checkHandle{cfg: w.cfg, stop: make(chan struct{})}
		d.running[key] = h
		starting = append(starting, launch{w, h})
	}
	for key, h := range d.running {
		if keep[key] {
			continue
		}
		close(h.stop)
		delete(d.running, key)
	}
	// Observations for endpoints that no longer exist must go too, or `health
	// status`, the daemon rollup and the backend snapshot would keep reporting a
	// frozen last-known state for something nobody configured.
	for appID, states := range d.endpointStates {
		for name := range states {
			if !keep[checkerKey(appID, name)] {
				delete(states, name)
			}
		}
		if len(states) == 0 {
			delete(d.endpointStates, appID)
		}
	}
	d.mu.Unlock()

	for _, l := range starting {
		d.wg.Add(1)
		go d.checkEndpoint(l.appID, l.appName, l.endpoint, l.handle)
	}
}

// startSyncWorker re-reconciles configuration on a fixed interval.
func (d *Daemon) startSyncWorker() {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		ticker := time.NewTicker(syncInterval)
		defer ticker.Stop()
		for {
			select {
			case <-d.stopChan:
				return
			case <-ticker.C:
				d.Sync()
			}
		}
	}()
}

// Stop halts all health checking
func (d *Daemon) Stop() error {
	d.mu.Lock()
	if !d.isRunning {
		d.mu.Unlock()
		return phelixerr.New(phelixerr.CodeServer, "health daemon is not running")
	}
	d.isRunning = false
	d.mu.Unlock()

	close(d.stopChan)
	d.wg.Wait()

	d.mu.Lock()
	d.running = make(map[string]*checkHandle)
	d.mu.Unlock()

	logs.Info("health", "daemon stopped")
	return nil
}

// checkEndpoint continuously checks a single endpoint until its handle is
// retired (configuration changed or removed) or the daemon stops.
//
// The first probe runs immediately: waiting a full interval left a freshly
// configured endpoint reporting nothing for up to its interval, which the
// backend cannot distinguish from an endpoint that is never checked at all.
func (d *Daemon) checkEndpoint(appID, appName, endpointName string, handle *checkHandle) {
	defer d.wg.Done()

	config := handle.cfg
	interval := 10 * time.Second
	if config.Interval != "" {
		if parsed, err := time.ParseDuration(config.Interval); err == nil && parsed > 0 {
			interval = parsed
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		result := d.checker.Check(&config)
		result.AppID = appID
		result.AppName = appName

		select {
		case d.broadcastChan <- result:
		default:
			// A saturated channel means listeners are behind; the state update
			// below still lands, so the snapshot stays correct.
			logs.Debug("health", "broadcast channel full, dropping result for %s.%s", appName, endpointName)
		}

		d.updateEndpointState(appID, appName, endpointName, &config, result, handle)
		logs.Debug("health", "%s.%s: %s (latency: %v ms)", appName, endpointName, result.Status, result.LatencyMs)

		select {
		case <-d.stopChan:
			return
		case <-handle.stop:
			return
		case <-ticker.C:
		}
	}
}

// updateEndpointState records a check result.
//
// from identifies the checker that produced the result. A checker retired
// mid-probe (its endpoint was removed or reconfigured while it was in flight)
// still has one result to deliver; recording it would resurrect the state Sync
// just dropped and could fire an auto-restart for an endpoint that no longer
// exists. A nil from means the caller is not a checker and is always recorded.
func (d *Daemon) updateEndpointState(appID, appName, endpointName string, config *HealthCheckConfig, result *HealthCheckResult, from *checkHandle) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if from != nil && d.running[checkerKey(appID, endpointName)] != from {
		return
	}

	if _, exists := d.endpointStates[appID]; !exists {
		d.endpointStates[appID] = make(map[string]*EndpointState)
	}

	state := d.endpointStates[appID][endpointName]
	if state == nil {
		state = &EndpointState{CrashHistory: make([]time.Time, 0)}
		d.endpointStates[appID][endpointName] = state
	}

	state.LastResult = result

	if result.Status == "UP" {
		state.ConsecutiveFailures = 0
		state.LastSuccessTime = time.Now()
		state.BackoffLevel = 0
	} else {
		state.ConsecutiveFailures++
		state.LastFailTime = time.Now()

		if state.ConsecutiveFailures >= config.Retries {
			if state.BackoffLevel == 0 || time.Now().After(state.NextRestartTime) {
				d.triggerAutoRestart(appID, appName, endpointName, config, state)
			}
		}
	}

	history := d.configManager.GetHistory(appID, endpointName)
	if history == nil {
		history = &HealthCheckHistory{
			EndpointName: endpointName,
			Results:      make([]HealthCheckResult, 0),
		}
	}

	history.Results = append(history.Results, *result)
	if len(history.Results) > 100 {
		history.Results = history.Results[1:]
	}
	history.LastChecked = time.Now()

	d.configManager.SaveHistory(appID, endpointName, history)
}

// triggerAutoRestart initiates an automatic restart of the application.
//
// CrashHistory records auto-restarts, not checks: it is appended to here (and
// only here), so crash_count_24h on the wire counts restart attempts in the last
// 24h rather than growing on every successful probe.
func (d *Daemon) triggerAutoRestart(appID, appName, endpointName string, config *HealthCheckConfig, state *EndpointState) {
	backoffSeconds := d.calculateBackoff(state.BackoffLevel)

	now := time.Now()
	filteredCrashes := make([]time.Time, 0)
	for _, t := range state.CrashHistory {
		if now.Sub(t) < 24*time.Hour {
			filteredCrashes = append(filteredCrashes, t)
		}
	}
	state.CrashHistory = filteredCrashes

	record := &AutoRestartRecord{
		AppID:              appID,
		AppName:            appName,
		EndpointID:         config.ID,
		EndpointName:       endpointName,
		Reason:             fmt.Sprintf("%d consecutive health check failures on %s", state.ConsecutiveFailures, endpointName),
		BackoffNextSeconds: backoffSeconds,
		RestartedAt:        time.Now(),
		CrashCount24h:      len(state.CrashHistory),
	}

	if len(state.CrashHistory) > 10 {
		logs.Warning("health", "app %s has crashed %d times in 24h, skipping auto-restart", appName, len(state.CrashHistory))
		record.ExitCode = 1
		d.autoRestartChan <- record
		return
	}

	state.BackoffLevel++
	state.NextRestartTime = time.Now().Add(time.Duration(backoffSeconds) * time.Second)
	state.CrashHistory = append(state.CrashHistory, time.Now())

	d.autoRestartChan <- record

	logs.Info("health", "auto-restart triggered for %s (backoff: %ds)", appName, backoffSeconds)
}

// calculateBackoff returns the backoff duration in seconds
func (d *Daemon) calculateBackoff(level int) int {
	backoffs := []int{5, 15, 60, 300}
	if level >= len(backoffs) {
		return backoffs[len(backoffs)-1]
	}
	return backoffs[level]
}

// startBroadcaster fans health check results out to local listeners (the live
// terminal display, the remote `health watch` stream). Results do NOT go to the
// backend from here: the backend receives them inside the app's health snapshot,
// where they arrive with the endpoint's identity and configuration attached.
func (d *Daemon) startBroadcaster() {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		for {
			select {
			case <-d.stopChan:
				return
			case result := <-d.broadcastChan:
				d.mu.RLock()
				listeners := make([]EventListener, len(d.eventListeners))
				copy(listeners, d.eventListeners)
				d.mu.RUnlock()

				for _, l := range listeners {
					go l(result)
				}
			}
		}
	}()
}

// startAutoRestarter handles automatic app restarts.
//
// The restart is attempted BEFORE the event is reported, so exit_code describes
// what actually happened: 0 when the app came back, non-zero when the restart
// failed or was skipped for exceeding the crash-loop ceiling. Reporting first
// (as this used to) always told the backend exit_code 0.
func (d *Daemon) startAutoRestarter() {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		for {
			select {
			case <-d.stopChan:
				return
			case record := <-d.autoRestartChan:
				if record.CrashCount24h <= 10 {
					logs.Info("health", "auto-restarting %s...", record.AppName)
					if err := restartForAutoRestart(record.AppID); err != nil {
						logs.Error("health", "failed to restart %s: %v", record.AppName, err)
						record.ExitCode = 1
					} else {
						record.ExitCode = 0
					}
				}

				if err := d.configManager.SaveAutoRestartRecord(record.AppID, record); err != nil {
					logs.Error("health", "failed to save restart record: %v", err)
				}

				d.mu.RLock()
				r := d.reporter
				d.mu.RUnlock()
				if r != nil {
					if err := r.SendAutoRestartEvent(record); err != nil {
						// Local restart already happened; the report is
						// best-effort and must not stall the worker.
						logs.Error("health", "failed to report auto-restart for %s: %v", record.AppName, err)
					}
				}
			}
		}
	}()
}

// restartForAutoRestart restarts an unhealthy app. Apps managed by a
// zero-downtime deployment own their instances, their internal ports and the
// proxy route, so the single-PID RestartApplication would kill the serving
// instance and then try to bind the proxy-owned public port — turning an
// unhealthy app into a down one. Those go through the deploy-aware restart the
// cmd layer wires up (health cannot import deploy: deploy already imports
// health).
func restartForAutoRestart(appID string) error {
	if app.DeployedAppSkipper == nil || !app.DeployedAppSkipper(appID) {
		return app.Manager.RestartApplication(appID)
	}
	if app.DeployedAppRestarter == nil {
		return phelixerr.Newf(phelixerr.CodeProcessFailed,
			"app %s is managed by a zero-downtime deployment and no deploy-aware restart is available", appID)
	}
	return app.DeployedAppRestarter(appID)
}

// startMaintenanceWorker performs periodic cleanup
func (d *Daemon) startMaintenanceWorker() {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()

		for {
			select {
			case <-d.stopChan:
				return
			case <-ticker.C:
				d.performMaintenance()
			}
		}
	}()
}

// performMaintenance cleans up old data
func (d *Daemon) performMaintenance() {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()
	successThreshold := 5 * time.Minute

	for appID := range d.endpointStates {
		for endpointName, state := range d.endpointStates[appID] {
			if state.LastSuccessTime.After(state.LastFailTime) &&
				now.Sub(state.LastSuccessTime) > successThreshold &&
				state.BackoffLevel > 0 {
				logs.Debug("health", "resetting backoff for %s.%s", appID, endpointName)
				state.BackoffLevel = 0
				state.NextRestartTime = time.Time{}
			}
		}
	}

	d.lastCleanup = now
}

// GetStatus returns the current health status for an app
func (d *Daemon) GetStatus(appID string) map[string]*HealthCheckResult {
	d.mu.RLock()
	defer d.mu.RUnlock()

	results := make(map[string]*HealthCheckResult)
	if states, exists := d.endpointStates[appID]; exists {
		for endpointName, state := range states {
			if state.LastResult != nil {
				results[endpointName] = state.LastResult
			}
		}
	}
	return results
}

// AddEventListener registers a listener for health events
func (d *Daemon) AddEventListener(listener EventListener) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.eventListeners = append(d.eventListeners, listener)
}

// GetAllStatus returns status for all apps
func (d *Daemon) GetAllStatus() map[string]map[string]*HealthCheckResult {
	d.mu.RLock()
	defer d.mu.RUnlock()

	results := make(map[string]map[string]*HealthCheckResult)
	for appID := range d.endpointStates {
		results[appID] = make(map[string]*HealthCheckResult)
		for endpointName, state := range d.endpointStates[appID] {
			if state.LastResult != nil {
				results[appID][endpointName] = state.LastResult
			}
		}
	}
	return results
}

// IsRunning returns whether the daemon is running
func (d *Daemon) IsRunning() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.isRunning
}
