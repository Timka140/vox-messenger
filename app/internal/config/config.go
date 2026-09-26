package config

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/ilyakaznacheev/cleanenv"
)

const (
	CEnvLocal = "local"
	CEnvDev   = "dev"
	CEnvProd  = "prod"
)

type Config struct {
	Env string `yaml:"env" env:"ENV" env-default:"local"`

	Timeout time.Duration `yaml:"timeout" env:"WAIT_TIMEOUT" env-default:"10s"`
}

// MustLoad - загрузка конфига
func MustLoad() *Config {
	path := fetchConfigPath()

	var err error
	cfg := &Config{}

	if path == "" {
		err = loadEnv(cfg)
	} else {
		err = loadYaml(cfg, path)
	}

	if err != nil {
		panic(fmt.Errorf("load config: %w", err))
	}

	return cfg
}

func loadYaml(cfg *Config, path string) error {
	const op = "config.loadYaml"
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return fmt.Errorf("%s: config file dose not exits: %s, %w", op, path, err)
	}

	if err := cleanenv.ReadConfig(path, cfg); err != nil {
		return fmt.Errorf("%s: config failed to read: %w", op, err)
	}

	return nil
}

func loadEnv(cfg *Config) error {
	const op = "config.loadEnv"
	err := cleanenv.ReadEnv(cfg)
	if err != nil {
		return fmt.Errorf("%s: load config failed: %w", op, err)
	}
	return nil
}

func fetchConfigPath() string {
	var res string

	// -config="path/to/config.yaml"
	flag.StringVar(&res, "config", "", "path to config")
	flag.Parse()

	if res == "" {
		res = os.Getenv("CONFIG_PATH")
	}

	return res
}
