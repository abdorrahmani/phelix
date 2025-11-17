package config

import (
	"bytes"
	_ "embed"
	"fmt"
	"sync"

	"github.com/spf13/viper"
)

//go:embed config.yml
var embeddedConfig []byte

type Config struct {
	App AppConfig
}

type AppConfig struct {
	Mode   string `yaml:"mode"`
	API    string `yaml:"api"`
	WSSUrl string `yaml:"wssUrl"`
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
			err = fmt.Errorf("unable to read embedded config: %v", readErr)
			return
		}

		var c Config
		if unmarshalErr := viper.Unmarshal(&c); unmarshalErr != nil {
			err = fmt.Errorf("unable to decode config: %v", unmarshalErr)
			return
		}
		cfg = &c
	})

	return err
}

func Get() *Config {
	return cfg
}
