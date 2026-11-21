package supplychain

import (
	"github.com/abdorrahmani/phelix/internal/security"
	"github.com/pelletier/go-toml/v2"
	"os"
	"path/filepath"
)

func rustPackageHashes(dir string) map[string]string {
	hashes := map[string]string{}
	raw, err := os.ReadFile(filepath.Join(dir, "Cargo.lock"))
	if err != nil {
		return hashes
	}
	var lock struct {
		Packages []struct {
			Name     string `toml:"name"`
			Version  string `toml:"version"`
			Checksum string `toml:"checksum"`
		} `toml:"package"`
	}
	if toml.Unmarshal(raw, &lock) != nil {
		return hashes
	}
	for _, p := range lock.Packages {
		if digestPattern.MatchString(p.Checksum) {
			hashes[security.ModuleKey(p.Name, p.Version)] = p.Checksum
		}
	}
	return hashes
}
