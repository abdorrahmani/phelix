package health

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ConfigManager handles persistence of health check configurations
type ConfigManager struct {
	basePath string
	mu       sync.RWMutex
	configs  map[string]*AppHealthConfig // key is app ID
}

var configMgr *ConfigManager

// InitConfigManager initializes the config manager
func InitConfigManager() (*ConfigManager, error) {
	if configMgr != nil {
		return configMgr, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	// Persist per-app under ~/.phelix/apps/<AppName>/health.json
	basePath := filepath.Join(home, ".phelix", "apps")
	if err := os.MkdirAll(basePath, 0755); err != nil {
		return nil, err
	}

	configMgr = &ConfigManager{
		basePath: basePath,
		configs:  make(map[string]*AppHealthConfig),
	}

	// Load all existing configs
	if err := configMgr.loadAllConfigs(); err != nil {
		return nil, fmt.Errorf("failed to load health configs: %w", err)
	}

	return configMgr, nil
}

// GetConfigManager returns the singleton instance
func GetConfigManager() *ConfigManager {
	if configMgr == nil {
		panic("ConfigManager not initialized")
	}
	return configMgr
}

// SaveConfig persists a health check configuration to disk
func (cm *ConfigManager) SaveConfig(appID string, config *AppHealthConfig) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	config.UpdatedAt = time.Now()
	cm.configs[appID] = config

	// Use app name folder so configs are stored under ~/.phelix/apps/<AppName>/health.json
	appFolder := config.AppName
	if appFolder == "" {
		appFolder = appID
	}
	appConfigPath := filepath.Join(cm.basePath, appFolder)
	if err := os.MkdirAll(appConfigPath, 0755); err != nil {
		return err
	}

	filePath := filepath.Join(appConfigPath, "health.json")
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(filePath, data, 0644)
}

// GetConfig retrieves a health check configuration
func (cm *ConfigManager) GetConfig(appID string) *AppHealthConfig {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	if config, exists := cm.configs[appID]; exists {
		return config
	}
	return nil
}

// DeleteConfig removes a health check configuration
func (cm *ConfigManager) DeleteConfig(appID string) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	delete(cm.configs, appID)

	appConfigPath := filepath.Join(cm.basePath, appID)
	return os.RemoveAll(appConfigPath)
}

// loadAllConfigs loads all health check configs from disk
func (cm *ConfigManager) loadAllConfigs() error {
	entries, err := os.ReadDir(cm.basePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // basePath doesn't exist yet
		}
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() {
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
			cm.configs[key] = &config
		}
	}

	return nil
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
		return err
	}

	filePath := filepath.Join(appHistoryPath, endpointName+".json")
	data, err := json.MarshalIndent(history, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(filePath, data, 0644)
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
		return err
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
	return os.WriteFile(recordsPath, data, 0644)
}
