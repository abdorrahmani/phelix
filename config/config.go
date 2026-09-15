package config

import (
	"bytes"
	_ "embed"
	"os"
	"strings"
	"sync"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/spf13/viper"
)

//go:embed config.yml
var embeddedConfig []byte

type Config struct {
	App   AppConfig
	Alert AlertConfig
}

type AppConfig struct {
	Mode    string `yaml:"mode"`
	API     string `yaml:"api"`
	GRPCUrl string `yaml:"grpcUrl"`
}

// AlertConfig holds the alert thresholds the agent reports as this server's
// real configuration. Zero means "use the built-in default" so an unset
// config.yml section never reports literal zero thresholds.
type AlertConfig struct {
	CPUThreshold  float64 `yaml:"cpuThreshold"`
	RAMThreshold  float64 `yaml:"ramThreshold"`
	DiskThreshold float64 `yaml:"diskThreshold"`
}

var (
	cfg  *Config
	once sync.Once
)

func Load() error {
	var err error

	once.Do(func() {
		viper.SetConfigType("yml")

		if readErr := viper.ReadConfig(
			bytes.NewBuffer(embeddedConfig),
		); readErr != nil {
			err = phelixerr.Wrap(phelixerr.CodeConfiguration, "unable to read embedded config", readErr)
			return
		}

		var c Config
		// Classified as CodeConfiguration: this is the embedded config, so an
		// unmarshal failure is a malformed-config problem, never a missing-file
		// one.
		if unmarshalErr := viper.Unmarshal(&c); unmarshalErr != nil {
			err = phelixerr.Wrap(phelixerr.CodeConfiguration, "unable to decode config", unmarshalErr)
			return
		}
		applyEnvOverrides(&c)
		cfg = &c
	})

	return err
}

// applyEnvOverrides applies the documented runtime overrides on top of the
// embedded config: PHELIX_MODE, PHELIX_API and PHELIX_GRPC_URL. Empty values
// are ignored, so setting a variable to "" keeps the embedded value. Mode is
// normalized to lowercase to match the config's own vocabulary ("dev",
// "production").
func applyEnvOverrides(c *Config) {
	if v := strings.TrimSpace(os.Getenv("PHELIX_MODE")); v != "" {
		c.App.Mode = strings.ToLower(v)
	}
	if v := strings.TrimSpace(os.Getenv("PHELIX_API")); v != "" {
		c.App.API = v
	}
	if v := strings.TrimSpace(os.Getenv("PHELIX_GRPC_URL")); v != "" {
		c.App.GRPCUrl = v
	}
}

func Get() *Config {
	return cfg
}
