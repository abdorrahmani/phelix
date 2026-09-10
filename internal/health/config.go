package health

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// ConfigManager handles persistence of health check configurations
type ConfigManager struct {
	basePath string
	mu       sync.RWMutex
	configs  map[string]*AppHealthConfig // key is app ID
}

var (
	configMgr   *ConfigManager
	configMgrMu sync.Mutex
)

// InitConfigManager initializes the config manager for the current persistent
// configuration root. HOME may change in embedded/test processes, so a manager
// cached for a different root must never be reused.
func InitConfigManager() (*ConfigManager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to resolve home directory", err)
	}
	basePath := filepath.Join(home, ".phelix", "apps")

	configMgrMu.Lock()
	defer configMgrMu.Unlock()
	if configMgr != nil && configMgr.basePath == basePath {
		return configMgr, nil
	}

	if err := os.MkdirAll(basePath, 0755); err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to create health config directory %s", basePath)
	}
	cm := &ConfigManager{
		basePath: basePath,
		configs:  make(map[string]*AppHealthConfig),
	}
	if err := cm.loadAllConfigs(); err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to load health configs", err)
	}
	configMgr = cm
	return configMgr, nil
}

// GetConfigManager returns the singleton instance.
func GetConfigManager() *ConfigManager {
	configMgrMu.Lock()
	defer configMgrMu.Unlock()
	if configMgr == nil {
		panic("ConfigManager not initialized")
	}
	return configMgr
}

// SaveConfig persists a health check configuration to disk.
//
// SaveConfig is the single authority for endpoint identity: any endpoint without
// an ID gets one here, reusing the ID of the endpoint that previously held the
// same name so an edit stays an update rather than becoming delete+create. That
// covers every writer — `health set`, `health add`, a backend-issued health
// command, and the phelix.yaml sync that rebuilds the whole endpoint map on each
// build — without any of them having to think about it.
func (cm *ConfigManager) SaveConfig(appID string, config *AppHealthConfig) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	assignEndpointIDs(cm.configs[appID], config)

	config.UpdatedAt = time.Now()
	cm.configs[appID] = config

	// Use app name folder so configs are stored under ~/.phelix/apps/<AppName>/health.json
	appFolder := config.AppName
	if appFolder == "" {
		appFolder = appID
	}
	appConfigPath := filepath.Join(cm.basePath, appFolder)
	if err := os.MkdirAll(appConfigPath, 0755); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to create app config directory %s", appConfigPath)
	}

	filePath := filepath.Join(appConfigPath, "health.json")
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to marshal health config", err)
	}

	if err := os.WriteFile(filePath, data, 0644); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to write health config %s", filePath)
	}
	return nil
}

// assignEndpointIDs fills in a stable ID for every endpoint in next, carrying
// over prev's ID for endpoints that keep the same name. An ID already set on an
// endpoint is never overwritten, so a caller that knows the identity (a rename)
// can pass it through.
func assignEndpointIDs(prev, next *AppHealthConfig) {
	if next == nil {
		return
	}
	for name, ep := range next.Endpoints {
		if ep == nil || ep.ID != "" {
			continue
		}
		if prev != nil {
			if old, ok := prev.Endpoints[name]; ok && old != nil && old.ID != "" {
				ep.ID = old.ID
				continue
			}
		}
		ep.ID = newEndpointID()
	}
}

// newEndpointID mints an RFC 4122 version 4 identifier for a health endpoint,
// matching how app IDs are generated (internal/app.GenerateAppID).
func newEndpointID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failures are exceptional; keep the ID unique so identity
		// never silently collapses onto another endpoint.
		return fmt.Sprintf("hep-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// GetConfig retrieves a health check configuration.
//
// The result is a deep copy: several goroutines in the monitor process read
// configs (health daemon reconcile, snapshot builder, deploy tier resolution)
// while others mutate-then-save them (CLI commands, backend health commands), so
// handing out the cached pointer would be a data race on the endpoint map.
func (cm *ConfigManager) GetConfig(appID string) *AppHealthConfig {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	return cm.configs[appID].clone()
}

// clone deep-copies a config so callers can mutate it freely.
func (c *AppHealthConfig) clone() *AppHealthConfig {
	if c == nil {
		return nil
	}
	out := *c
	out.Endpoints = make(map[string]*HealthCheckConfig, len(c.Endpoints))
	for name, ep := range c.Endpoints {
		if ep == nil {
			continue
		}
		cp := *ep
		out.Endpoints[name] = &cp
	}
	if c.DeployTier != nil {
		dt := *c.DeployTier
		out.DeployTier = &dt
	}
	if c.HTTPMetrics != nil {
		hm := *c.HTTPMetrics
		out.HTTPMetrics = &hm
	}
	return &out
}

// ConfiguredAppIDs returns the app IDs that currently have a health
// configuration, so callers can enumerate them without copying every config.
func (cm *ConfigManager) ConfiguredAppIDs() []string {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	ids := make([]string, 0, len(cm.configs))
	for id := range cm.configs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Reload re-reads every health.json from disk, replacing the in-memory cache.
//
// Health configs are written by other processes (`phelix health set/add/remove`,
// `phelix build`) as well as by this one, so a cache populated once at startup
// goes stale in both directions: it misses endpoints added later and keeps
// serving endpoints that were removed.
func (cm *ConfigManager) Reload() error {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.loadAllConfigsLocked()
}

// DeleteConfig removes a health check configuration
func (cm *ConfigManager) DeleteConfig(appID string) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	// Configs live under the app-NAME folder (see SaveConfig), so the directory
	// has to be resolved from the cached config; using appID would delete
	// nothing, or the wrong app's directory if a name ever matched an ID.
	appFolder := appID
	if cfg, ok := cm.configs[appID]; ok && cfg.AppName != "" {
		appFolder = cfg.AppName
	}
	delete(cm.configs, appID)

	if err := os.RemoveAll(filepath.Join(cm.basePath, appFolder)); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to remove health config directory for %s", appID)
	}
	return nil
}

// loadAllConfigs loads all health check configs from disk
func (cm *ConfigManager) loadAllConfigs() error {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.loadAllConfigsLocked()
}

// loadAllConfigsLocked rebuilds the cache from disk. The caller holds cm.mu.
//
// It builds a fresh map and swaps it in rather than merging, so a config file
// that was deleted disappears from the cache too. Endpoints persisted before
// identities existed get one minted and written back here, once.
func (cm *ConfigManager) loadAllConfigsLocked() error {
	entries, err := os.ReadDir(cm.basePath)
	if err != nil {
		if os.IsNotExist(err) {
			cm.configs = make(map[string]*AppHealthConfig)
			return nil // basePath doesn't exist yet
		}
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to list health config directory %s", cm.basePath)
	}

	loaded := make(map[string]*AppHealthConfig, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		configPath := filepath.Join(cm.basePath, entry.Name(), "health.json")
		data, err := os.ReadFile(configPath)
		if err != nil {
			continue // Skip if file doesn't exist
		}

		var config AppHealthConfig
		if err := json.Unmarshal(data, &config); err != nil {
			continue // Skip malformed configs
		}

		// Index by AppID (from file), fall back to directory name
		key := config.AppID
		if key == "" {
			key = entry.Name()
		}
		if migrateEndpointIDs(&config) {
			if data, err := json.MarshalIndent(&config, "", "  "); err == nil {
				// Best effort: a failed write only means the IDs are minted
				// again next load, which is still stable for the process.
				_ = os.WriteFile(configPath, data, 0644)
			}
		}
		loaded[key] = &config
	}

	cm.configs = loaded
	return nil
}

// migrateEndpointIDs mints IDs for endpoints persisted before identity existed.
// Reports whether anything changed.
func migrateEndpointIDs(config *AppHealthConfig) bool {
	changed := false
	for _, ep := range config.Endpoints {
		if ep != nil && ep.ID == "" {
			ep.ID = newEndpointID()
			changed = true
		}
	}
	return changed
}

// SaveHistory persists health check history for an endpoint
func (cm *ConfigManager) SaveHistory(appID, endpointName string, history *HealthCheckHistory) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	// Resolve app folder name from stored config if available
	appFolder := appID
	if cfg, ok := cm.configs[appID]; ok && cfg.AppName != "" {
		appFolder = cfg.AppName
	}
	appHistoryPath := filepath.Join(cm.basePath, appFolder, "health", "history")
	if err := os.MkdirAll(appHistoryPath, 0755); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to create health history directory %s", appHistoryPath)
	}

	filePath := filepath.Join(appHistoryPath, endpointName+".json")
	data, err := json.MarshalIndent(history, "", "  ")
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to marshal health history", err)
	}

	if err := os.WriteFile(filePath, data, 0644); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to write health history %s", filePath)
	}
	return nil
}

// GetHistory retrieves health check history for an endpoint
func (cm *ConfigManager) GetHistory(appID, endpointName string) *HealthCheckHistory {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	appFolder := appID
	if cfg, ok := cm.configs[appID]; ok && cfg.AppName != "" {
		appFolder = cfg.AppName
	}
	appHistoryPath := filepath.Join(cm.basePath, appFolder, "health", "history")
	filePath := filepath.Join(appHistoryPath, endpointName+".json")

	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil
	}

	var history HealthCheckHistory
	if err := json.Unmarshal(data, &history); err != nil {
		return nil
	}

	return &history
}

// SaveAutoRestartRecord persists an auto-restart event
func (cm *ConfigManager) SaveAutoRestartRecord(appID string, record *AutoRestartRecord) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	appFolder := appID
	if cfg, ok := cm.configs[appID]; ok && cfg.AppName != "" {
		appFolder = cfg.AppName
	}
	appPath := filepath.Join(cm.basePath, appFolder, "health")
	if err := os.MkdirAll(appPath, 0755); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to create health directory %s", appPath)
	}

	// Keep last 100 restart records
	recordsPath := filepath.Join(appPath, "restarts.json")

	var records []AutoRestartRecord
	if data, err := os.ReadFile(recordsPath); err == nil {
		json.Unmarshal(data, &records)
	}

	// Append new record and keep only last 100
	records = append(records, *record)
	if len(records) > 100 {
		records = records[len(records)-100:]
	}

	data, _ := json.MarshalIndent(records, "", "  ")
	if err := os.WriteFile(recordsPath, data, 0644); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to write restart records %s", recordsPath)
	}
	return nil
}
