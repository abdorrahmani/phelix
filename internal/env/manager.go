package env

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// EnvManager handles encrypted environment variable storage and retrieval
type EnvManager struct {
	mu sync.Mutex
}

// Instance is the global env manager instance
var Instance = &EnvManager{}

// EnvEntry represents a single environment variable entry
type EnvEntry struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"` // encrypted
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// EnvStore represents the storage structure for all app env vars
type EnvStore struct {
	Version int                 `json:"version"`
	AppID   string              `json:"app_id"`
	Entries map[string]EnvEntry `json:"entries"`
}

// GetMasterKey retrieves the master key from ~/.phelix/master.key
func GetMasterKey() ([]byte, error) {
	homeDir := os.Getenv("HOME")
	if homeDir == "" {
		homeDir = os.Getenv("USERPROFILE") // For Windows
	}

	keyPath := filepath.Join(homeDir, ".phelix", "master.key")

	// Try to read existing key
	if data, err := os.ReadFile(keyPath); err == nil {
		// Decode hex string to bytes
		keyString := strings.TrimSpace(string(data))
		// The key should be hex-encoded
		if len(keyString) == 64 { // 32 bytes in hex
			key := make([]byte, 32)
			if _, err := fmt.Sscanf(keyString, "%x", &key); err == nil {
				return key, nil
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to read master key %s", keyPath)
	}

	// Generate new key if it doesn't exist
	key, err := GenerateMasterKey()
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeEncryption, "failed to generate master key", err)
	}

	// Create ~/.phelix directory if it doesn't exist
	phelixDir := filepath.Dir(keyPath)
	if err := os.MkdirAll(phelixDir, 0700); err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to create .phelix directory %s", phelixDir)
	}

	// Save key in hex format
	keyHex := fmt.Sprintf("%x", key)
	if err := os.WriteFile(keyPath, []byte(keyHex), 0600); err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeEncryption, err, "failed to save master key %s", keyPath)
	}

	return key, nil
}

// GetEnvFilePath returns the path to the app's .env.enc file
func GetEnvFilePath(appID string) (string, error) {
	homeDir := os.Getenv("HOME")
	if homeDir == "" {
		homeDir = os.Getenv("USERPROFILE") // For Windows
	}

	envDir := filepath.Join(homeDir, ".phelix", "envs")
	if err := os.MkdirAll(envDir, 0755); err != nil {
		return "", phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to create envs directory %s", envDir)
	}

	return filepath.Join(envDir, fmt.Sprintf("%s.env.enc", appID)), nil
}

// LoadEnvStore loads the encrypted env store for an app
func (m *EnvManager) LoadEnvStore(appID string) (*EnvStore, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	envPath, err := GetEnvFilePath(appID)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(envPath)
	if os.IsNotExist(err) {
		// Return empty store if file doesn't exist
		return &EnvStore{
			Version: 1,
			AppID:   appID,
			Entries: make(map[string]EnvEntry),
		}, nil
	} else if err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to read env file %s", envPath)
	}

	var store EnvStore
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "failed to parse env file %s", envPath)
	}

	return &store, nil
}

// SaveEnvStore saves the encrypted env store for an app
func (m *EnvManager) SaveEnvStore(store *EnvStore) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	envPath, err := GetEnvFilePath(store.AppID)
	if err != nil {
		return err
	}

	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to marshal env store", err)
	}

	if err := os.WriteFile(envPath, data, 0644); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to write env file %s", envPath)
	}

	return nil
}

// SetEnv sets an environment variable for an app
func (m *EnvManager) SetEnv(appID, key, value string) error {
	store, err := m.LoadEnvStore(appID)
	if err != nil {
		return err
	}

	masterKey, err := GetMasterKey()
	if err != nil {
		return err
	}

	// Encrypt the value
	encryptedValue, err := EncryptData(value, masterKey)
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeEncryption, err, "failed to encrypt value for %q", key)
	}

	now := time.Now()
	store.Entries[key] = EnvEntry{
		Key:       key,
		Value:     encryptedValue,
		CreatedAt: now,
		UpdatedAt: now,
	}

	return m.SaveEnvStore(store)
}

// GetEnv retrieves an environment variable for an app
func (m *EnvManager) GetEnv(appID, key string) (string, error) {
	store, err := m.LoadEnvStore(appID)
	if err != nil {
		return "", err
	}

	entry, exists := store.Entries[key]
	if !exists {
		return "", phelixerr.Newf(phelixerr.CodeNotFound, "environment variable '%s' not found", key)
	}

	masterKey, err := GetMasterKey()
	if err != nil {
		return "", err
	}

	// Decrypt the value
	value, err := DecryptData(entry.Value, masterKey)
	if err != nil {
		return "", phelixerr.Wrapf(phelixerr.CodeEncryption, err, "failed to decrypt value for %q", key)
	}

	return value, nil
}

// ListEnv lists all environment variables for an app
func (m *EnvManager) ListEnv(appID string) (map[string]string, error) {
	store, err := m.LoadEnvStore(appID)
	if err != nil {
		return nil, err
	}

	result := make(map[string]string)
	for key := range store.Entries {
		// Don't decrypt values in list, just show keys
		result[key] = "***REDACTED***"
	}

	return result, nil
}

// UnsetEnv removes an environment variable for an app
func (m *EnvManager) UnsetEnv(appID, key string) error {
	store, err := m.LoadEnvStore(appID)
	if err != nil {
		return err
	}

	if _, exists := store.Entries[key]; !exists {
		return phelixerr.Newf(phelixerr.CodeNotFound, "environment variable '%s' not found", key)
	}

	delete(store.Entries, key)
	return m.SaveEnvStore(store)
}

// GetAllEnvVars retrieves all environment variables for an app as a map
// This is used for process injection
func (m *EnvManager) GetAllEnvVars(appID string) (map[string]string, error) {
	store, err := m.LoadEnvStore(appID)
	if err != nil {
		return nil, err
	}

	masterKey, err := GetMasterKey()
	if err != nil {
		return nil, err
	}

	result := make(map[string]string)
	for key, entry := range store.Entries {
		// Decrypt each value
		value, err := DecryptData(entry.Value, masterKey)
		if err != nil {
			return nil, phelixerr.Wrapf(phelixerr.CodeEncryption, err, "failed to decrypt env var '%s'", key)
		}
		result[key] = value
	}

	return result, nil
}

// ExportEnvFile exports env vars as a .env file for manual inspection
// Values are masked for security
func (m *EnvManager) ExportEnvFile(appID string, masked bool) (string, error) {
	store, err := m.LoadEnvStore(appID)
	if err != nil {
		return "", err
	}

	var lines []string
	for key := range store.Entries {
		if masked {
			lines = append(lines, fmt.Sprintf("%s=***REDACTED***", key))
		} else {
			// Get decrypted value
			val, err := m.GetEnv(appID, key)
			if err != nil {
				return "", err
			}
			lines = append(lines, fmt.Sprintf("%s=%s", key, val))
		}
	}

	return strings.Join(lines, "\n"), nil
}

// BackupEnvStore creates a backup of all env vars for an app
// Returns a map of encrypted values for backup purposes
func (m *EnvManager) BackupEnvStore(appID string) (map[string]interface{}, error) {
	store, err := m.LoadEnvStore(appID)
	if err != nil {
		return nil, err
	}

	backup := make(map[string]interface{})
	backup["version"] = store.Version
	backup["app_id"] = store.AppID
	backup["entries_count"] = len(store.Entries)
	backup["backed_up_at"] = time.Now()

	// Store encrypted values for restoration
	encryptedEntries := make(map[string]EnvEntry)
	for key, entry := range store.Entries {
		encryptedEntries[key] = entry
	}
	backup["entries"] = encryptedEntries

	return backup, nil
}

// ClearAllEnv removes all environment variables for an app
func (m *EnvManager) ClearAllEnv(appID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	envPath, err := GetEnvFilePath(appID)
	if err != nil {
		return err
	}

	store := &EnvStore{
		Version: 1,
		AppID:   appID,
		Entries: make(map[string]EnvEntry),
	}

	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to marshal env store", err)
	}

	if err := os.WriteFile(envPath, data, 0644); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to write env file %s", envPath)
	}

	return nil
}

// ExportEnvAsJSON exports env vars as a JSON structure (masked for security)
func (m *EnvManager) ExportEnvAsJSON(appID string) (string, error) {
	store, err := m.LoadEnvStore(appID)
	if err != nil {
		return "", err
	}

	export := make(map[string]string)
	for key := range store.Entries {
		export[key] = "***REDACTED***"
	}

	data, err := json.MarshalIndent(export, "", "  ")
	if err != nil {
		return "", err
	}

	return string(data), nil
}

// GetEnvVarCount returns the number of env vars for an app
func (m *EnvManager) GetEnvVarCount(appID string) (int, error) {
	store, err := m.LoadEnvStore(appID)
	if err != nil {
		return 0, err
	}

	return len(store.Entries), nil
}
