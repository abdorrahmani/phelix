package health

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
)

// Daemon manages health checks for all applications
type Daemon struct {
	mu              sync.RWMutex
	endpointStates  map[string]map[string]*EndpointState // app ID -> endpoint name -> state
	checker         *Checker
	configManager   *ConfigManager
	stopChan        chan struct{}
	wg              sync.WaitGroup
	eventListeners  []EventListener
	websocketClient WebSocketClient
	isRunning       bool
	broadcastChan   chan *HealthCheckResult
	autoRestartChan chan *AutoRestartRecord
	lastCleanup     time.Time
}

// EventListener is called when health check events occur
type EventListener func(event interface{})

// WebSocketClient interface for sending health data to backend
type WebSocketClient interface {
	SendHealthCheckResult(result *HealthCheckResult, appID string, appName string) error
	SendAutoRestartEvent(record *AutoRestartRecord) error
	IsConnected() bool
}

var daemon *Daemon

// InitDaemon initializes the health check daemon
func InitDaemon(wsClient WebSocketClient) (*Daemon, error) {
	configMgr, err := InitConfigManager()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize config manager: %w", err)
	}

	daemon = &Daemon{
		endpointStates:  make(map[string]map[string]*EndpointState),
		checker:         NewChecker(),
		configManager:   configMgr,
		stopChan:        make(chan struct{}),
		websocketClient: wsClient,
		broadcastChan:   make(chan *HealthCheckResult, 100),
		autoRestartChan: make(chan *AutoRestartRecord, 50),
	}

	// Start background workers
	daemon.startBroadcaster()
	daemon.startAutoRestarter()
	daemon.startMaintenanceWorker()

	return daemon, nil
}

// GetDaemon returns the singleton daemon instance
func GetDaemon() *Daemon {
	if daemon == nil {
		panic("Daemon not initialized")
	}
	return daemon
}

// Start begins health checking for all configured apps
func (d *Daemon) Start() error {
	d.mu.Lock()
	if d.isRunning {
		d.mu.Unlock()
		return fmt.Errorf("daemon is already running")
	}
	d.isRunning = true
	d.mu.Unlock()

	log.Println("[Health] Daemon starting...")

	// Get all apps
	if err := app.Manager.LoadState(); err != nil {
		return fmt.Errorf("failed to load app state: %w", err)
	}

	apps := app.Manager.ListApplications()
	for _, appItem := range apps {
		config := d.configManager.GetConfig(appItem.ID)
		if config == nil || !config.Enabled {
			continue
		}

		// Initialize endpoint states for this app
		d.mu.Lock()
		if _, exists := d.endpointStates[appItem.ID]; !exists {
			d.endpointStates[appItem.ID] = make(map[string]*EndpointState)
		}

		for endpointName := range config.Endpoints {
			if _, exists := d.endpointStates[appItem.ID][endpointName]; !exists {
				d.endpointStates[appItem.ID][endpointName] = &EndpointState{
					CrashHistory: make([]time.Time, 0),
				}
			}
		}
		d.mu.Unlock()

		// Start health check goroutines for each endpoint
		for endpointName, endpointConfig := range config.Endpoints {
			d.wg.Add(1)
			go d.checkEndpoint(appItem.ID, appItem.Name, endpointName, endpointConfig)
		}
	}

	return nil
}

// Stop halts all health checking
func (d *Daemon) Stop() error {
	d.mu.Lock()
	if !d.isRunning {
		d.mu.Unlock()
		return fmt.Errorf("daemon is not running")
	}
	d.isRunning = false
	d.mu.Unlock()

	close(d.stopChan)
	d.wg.Wait()

	log.Println("[Health] Daemon stopped")
	return nil
}

// checkEndpoint continuously checks a single endpoint
func (d *Daemon) checkEndpoint(appID, appName, endpointName string, config *HealthCheckConfig) {
	defer d.wg.Done()

	interval := 10 * time.Second
	if config.Interval != "" {
		if d, err := time.ParseDuration(config.Interval); err == nil {
			interval = d
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-d.stopChan:
			return
		case <-ticker.C:
			result := d.checker.Check(config)
			// annotate with app info so broadcaster can route correctly
			result.AppID = appID
			result.AppName = appName
			d.broadcastChan <- result // Send for websocket streaming

			// Update state
			d.updateEndpointState(appID, appName, endpointName, config, result)

			// Log the result
			log.Printf("[Health] %s.%s: %s (latency: %v ms)", appName, endpointName, result.Status, result.LatencyMs)
		}
	}
}

// updateEndpointState updates the state based on check result
func (d *Daemon) updateEndpointState(appID, appName, endpointName string, config *HealthCheckConfig, result *HealthCheckResult) {
	d.mu.Lock()
	defer d.mu.Unlock()

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
		state.CrashHistory = append(state.CrashHistory, time.Now())
	} else {
		state.ConsecutiveFailures++
		state.LastFailTime = time.Now()

		// Check if we've exceeded the retry threshold
		if state.ConsecutiveFailures >= config.Retries {
			// Trigger auto-restart if not already on backoff
			if state.BackoffLevel == 0 || time.Now().After(state.NextRestartTime) {
				d.triggerAutoRestart(appID, appName, endpointName, config, state)
			}
		}
	}

	// Save history
	history := d.configManager.GetHistory(appID, endpointName)
	if history == nil {
		history = &HealthCheckHistory{
			EndpointName: endpointName,
			Results:      make([]HealthCheckResult, 0),
		}
	}

	// Ring buffer - keep last 100
	history.Results = append(history.Results, *result)
	if len(history.Results) > 100 {
		history.Results = history.Results[1:]
	}
	history.LastChecked = time.Now()

	d.configManager.SaveHistory(appID, endpointName, history)
}

// triggerAutoRestart initiates an automatic restart of the application
func (d *Daemon) triggerAutoRestart(appID, appName, endpointName string, config *HealthCheckConfig, state *EndpointState) {
	backoffSeconds := d.calculateBackoff(state.BackoffLevel)

	// Clean up crash history older than 24h
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
		Reason:             fmt.Sprintf("%d consecutive health check failures on %s", state.ConsecutiveFailures, endpointName),
		BackoffNextSeconds: backoffSeconds,
		RestartedAt:        time.Now(),
		CrashCount24h:      len(state.CrashHistory),
	}

	// Check if we should actually restart (not too many crashes)
	if len(state.CrashHistory) > 10 {
		log.Printf("[Health] App %s has crashed %d times in 24h, skipping auto-restart", appName, len(state.CrashHistory))
		record.ExitCode = 1
		d.autoRestartChan <- record
		return
	}

	// Update state
	state.BackoffLevel++
	state.NextRestartTime = time.Now().Add(time.Duration(backoffSeconds) * time.Second)
	state.CrashHistory = append(state.CrashHistory, time.Now())

	// Queue for restart
	d.autoRestartChan <- record

	log.Printf("[Health] Auto-restart triggered for %s (backoff: %ds)", appName, backoffSeconds)
}

// calculateBackoff returns the backoff duration in seconds
func (d *Daemon) calculateBackoff(level int) int {
	backoffs := []int{5, 15, 60, 300} // 5s, 15s, 1m, 5m
	if level >= len(backoffs) {
		return backoffs[len(backoffs)-1]
	}
	return backoffs[level]
}

// startBroadcaster sends health check results to websocket
func (d *Daemon) startBroadcaster() {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		for {
			select {
			case <-d.stopChan:
				return
			case result := <-d.broadcastChan:
				// Notify listeners regardless of websocket availability
				d.mu.RLock()
				listeners := make([]EventListener, len(d.eventListeners))
				copy(listeners, d.eventListeners)
				d.mu.RUnlock()
				for _, l := range listeners {
					go l(result)
				}

				if d.websocketClient != nil {
					// Send via websocket (buffering client will handle disconnected state)
					if err := d.websocketClient.SendHealthCheckResult(result, result.AppID, result.AppName); err != nil {
						// log but keep going
						log.Printf("[Health] websocket send error: %v", err)
					}
				}
			}
		}
	}()
}

// startAutoRestarter handles automatic app restarts
func (d *Daemon) startAutoRestarter() {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		for {
			select {
			case <-d.stopChan:
				return
			case record := <-d.autoRestartChan:
				if d.websocketClient != nil {
					_ = d.websocketClient.SendAutoRestartEvent(record)
				}

				if err := d.configManager.SaveAutoRestartRecord(record.AppID, record); err != nil {
					log.Printf("[Health] Failed to save restart record: %v", err)
				}

				// Perform the restart
				if record.CrashCount24h <= 10 {
					log.Printf("[Health] Auto-restarting %s...", record.AppName)
					if err := app.Manager.RestartApplication(record.AppID); err != nil {
						log.Printf("[Health] Failed to restart %s: %v", record.AppName, err)
						record.ExitCode = 1
					} else {
						record.ExitCode = 0
					}
				}
			}
		}
	}()
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

	// Reset backoff for healthy endpoints
	for appID := range d.endpointStates {
		for endpointName, state := range d.endpointStates[appID] {
			if state.LastSuccessTime.After(state.LastFailTime) &&
				now.Sub(state.LastSuccessTime) > successThreshold &&
				state.BackoffLevel > 0 {
				log.Printf("[Health] Resetting backoff for %s.%s", appID, endpointName)
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
