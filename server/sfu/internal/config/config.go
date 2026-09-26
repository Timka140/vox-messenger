package config

import (
	"flag"
	"fmt"
	"os"

	"github.com/ilyakaznacheev/cleanenv"
)

type Config struct {
	Env    string `yaml:"env" env:"ENV" env-default:"local"`
	Host   string `yaml:"host" env:"SFU_HOST" env-default:"0.0.0.0"`
	Port   int    `yaml:"port" env:"SFU_PORT" env-default:"9760"`
	Secret string `yaml:"secret" env:"SFU_SECRET" env-default:""`
}

func (c Config) Addr() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

func MustLoad() *Config {
	path := fetchConfigPath()
	cfg := &Config{}
	var err error
	if path == "" {
		err = cleanenv.ReadEnv(cfg)
	} else {
		if _, e := os.Stat(path); os.IsNotExist(e) {
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
