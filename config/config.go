package config

import (
	"fmt"
	"sync"

	"github.com/spf13/viper"
)

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
		viper.SetConfigName("config")
		viper.SetConfigType("yaml")
		viper.AddConfigPath(".")

		if readErr := viper.ReadInConfig(); readErr != nil {
			err = readErr
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
