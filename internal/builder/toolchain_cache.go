package builder

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// toolchainCacheFilePrefix is the prefix for toolchain version cache files.
const toolchainCacheFilePrefix = "toolchain-"

// versionCacheEntry represents a cached toolchain version for a specific language.
type versionCacheEntry struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	MtimeNS int64  `json:"mtime_ns"`
	Version string `json:"version"`
}

// toolchainCacheDir returns the directory holding toolchain version caches.
var toolchainCacheDir = func() string {
	return filepath.Join(agentDataDir(), "cache")
}

// agentDataDir mirrors internal/server's resolution without importing it, so
// the builder package stays free of the server dependency. PHELIX_DATA_DIR
// wins, matching every other on-disk store.
func agentDataDir() string {
	if dir := os.Getenv("PHELIX_DATA_DIR"); dir != "" {
		return dir
	}

	if _, err := os.Stat("/.dockerenv"); err == nil {
		return "/var/lib/phelix"
	}

	home := os.Getenv("HOME")
	if home == "" {
		home = os.Getenv("USERPROFILE")
	}

	return filepath.Join(home, ".phelix")
}

// loadCachedVersion loads a version cache entry for the given language, returning the cached version and a boolean indicating whether the cache was valid.
func loadCachedVersion(lang Language, resolvedPath string, size, mtimeNS int64) (string, bool) {
	data, err := os.ReadFile(cacheFilePath(lang))
	if err != nil {
		return "", false
	}

	var e versionCacheEntry
	if err := json.Unmarshal(data, &e); err != nil {
		return "", false
	}

	if e.Path != resolvedPath || e.Size != size || e.MtimeNS != mtimeNS || e.Version == "" {
		return "", false
	}

	return e.Version, true
}

// storeCachedVersion stores a version cache entry for the given language.
func storeCachedVersion(lang Language, e versionCacheEntry) {
	path := cacheFilePath(lang)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}

	data, err := json.Marshal(e)
	if err != nil {
		return
	}

	tmp := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}

	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}

// cacheFilePath is the on-disk location of one language's version cache.
func cacheFilePath(lang Language) string {
	return filepath.Join(toolchainCacheDir(), toolchainCacheFilePrefix+string(lang)+".json")
}
