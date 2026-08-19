package config

import (
	"bytes"
	_ "embed"
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
		cfg = &c
	})

	return err
}

func Get() *Config {
	return cfg
}
