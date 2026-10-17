package builder

import (
	"os"
	"path/filepath"
	"testing"
)

// withTempCache points the cache at a fresh directory for one test.
func withTempCache(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", dir)
	return dir
}

// The happy path: a matching entry is returned.
func TestLoadCachedVersion_Hit(t *testing.T) {
	withTempCache(t)
	want := versionCacheEntry{Path: "/usr/bin/go", Size: 10, MtimeNS: 20, Version: "1.27"}
	storeCachedVersion(Go, want)

	got, ok := loadCachedVersion(Go, "/usr/bin/go", 10, 20)
	if !ok || got != "1.27" {
		t.Fatalf("loadCachedVersion = (%q, %v), want (%q, true)", got, ok, "1.27")
	}
}

// Every identity field must match. A changed path, size or mtime is an
// upgrade/reinstall/shim-retarget and must miss.
func TestLoadCachedVersion_MissOnAnyFieldChange(t *testing.T) {
	const (
		goodPath  = "/usr/bin/go"
		goodSize  = int64(10)
		goodMtime = int64(20)
	)
	cases := []struct {
		name  string
		path  string
		size  int64
		mtime int64
	}{
		{"different path", "/opt/go", goodSize, goodMtime},
		{"different size", goodPath, goodSize + 1, goodMtime},
		{"different mtime", goodPath, goodSize, goodMtime + 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withTempCache(t)
			storeCachedVersion(Go, versionCacheEntry{
				Path: goodPath, Size: goodSize, MtimeNS: goodMtime, Version: "1.27"})

			if _, ok := loadCachedVersion(Go, tc.path, tc.size, tc.mtime); ok {
				t.Fatalf("a changed %s must be a miss", tc.name)
			}
		})
	}
}

// The cache must never break a build: absent, empty, malformed and
// permission-denied entries are all misses.
func TestLoadCachedVersion_BadStateIsAMiss(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		withTempCache(t)
		if _, ok := loadCachedVersion(Go, "/x", 1, 1); ok {
			t.Fatal("absent cache must miss")
		}
	})
	t.Run("empty", func(t *testing.T) {
		withTempCache(t)
		if err := os.MkdirAll(toolchainCacheDir(), 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(cacheFilePath(Go), nil, 0o644)
		if _, ok := loadCachedVersion(Go, "/x", 1, 1); ok {
			t.Fatal("empty cache must miss")
		}
	})
	t.Run("malformed", func(t *testing.T) {
		withTempCache(t)
		os.MkdirAll(toolchainCacheDir(), 0o755)
		os.WriteFile(cacheFilePath(Go), []byte("not json"), 0o644)
		if _, ok := loadCachedVersion(Go, "/x", 1, 1); ok {
			t.Fatal("malformed cache must miss")
		}
	})
	t.Run("empty version", func(t *testing.T) {
		withTempCache(t)
		storeCachedVersion(Go, versionCacheEntry{Path: "/x", Size: 1, MtimeNS: 1})
		if _, ok := loadCachedVersion(Go, "/x", 1, 1); ok {
			t.Fatal("an entry with no version must miss")
		}
	})
}

// A store that cannot write must not panic or return an error.
func TestStoreCachedVersion_UnwritableIsSilent(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", "/proc/definitely-not-writable")
	storeCachedVersion(Go, versionCacheEntry{Path: "/x", Size: 1, MtimeNS: 1, Version: "1.27"})
}

// Languages must not share a file.
func TestCacheFilePath_PerLanguage(t *testing.T) {
	withTempCache(t)
	if cacheFilePath(Go) == cacheFilePath(Rust) {
		t.Fatal("each language needs its own cache file")
	}
	for _, lang := range []Language{Go, Rust} {
		if filepath.Dir(cacheFilePath(lang)) != toolchainCacheDir() {
			t.Fatalf("%s cache must live in the cache dir", lang)
		}
	}
}

// A stale temp file from an interrupted write must not be read as an entry.
func TestStoreCachedVersion_LeavesNoTempBehind(t *testing.T) {
	withTempCache(t)
	storeCachedVersion(Go, versionCacheEntry{Path: "/x", Size: 1, MtimeNS: 1, Version: "1.27"})

	entries, err := os.ReadDir(toolchainCacheDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			t.Fatalf("unexpected leftover file %q", e.Name())
		}
	}
}
