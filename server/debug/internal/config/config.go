package config

import (
	"flag"
	"fmt"
	"os"

	"github.com/ilyakaznacheev/cleanenv"
)

type Config struct {
	Env string `yaml:"env" env:"ENV" env-default:"local"`
	// HTTPHost — по умолчанию только localhost: API без авторизации отдаёт
	// логи, снаружи к нему ходят через SSH (build/fetch-logs.sh).
	HTTPHost        string `yaml:"http_host" env:"DEBUG_HTTP_HOST" env-default:"127.0.0.1"`
	HTTPPort        int    `yaml:"http_port" env:"HTTP_PORT" env-default:"9750"`
	SignalDebugAddr string `yaml:"signal_debug_addr" env:"SIGNAL_DEBUG_ADDR" env-default:"localhost:9742"`
	LogDir          string `yaml:"log_dir" env:"LOG_DIR" env-default:"./debug-logs"`
}

func MustLoad() *Config {
	path := fetchConfigPath()

	cfg := &Config{}
	var err error

	if path == "" {
		err = cleanenv.ReadEnv(cfg)
	} else {
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			panic(fmt.Errorf("config file does not exist: %s", path))
		}
		err = cleanenv.ReadConfig(path, cfg)
	}

	if err != nil {
		panic(fmt.Errorf("load config: %w", err))
	}

	return cfg
}

func fetchConfigPath() string {
	var res string
	flag.StringVar(&res, "config", "", "path to config")
	flag.Parse()
	if res == "" {
		res = os.Getenv("CONFIG_PATH")
	}
	return res
}
