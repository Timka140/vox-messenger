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

	WS      WSConfig      `yaml:"ws"`
	TURN    TURNConfig    `yaml:"turn"`
	Debug   DebugConfig   `yaml:"debug"`
	SFU     SFUConfig     `yaml:"sfu"`
	Groups  GroupConfig   `yaml:"groups"`
	Cluster ClusterConfig `yaml:"cluster"`
	Admin   AdminConfig   `yaml:"admin"`

	// ReleasesDir — директория на диске с manifest.json и артефактами обновлений.
	// Если пусто — endpoints /api/update/* возвращают 503. Auth_ok отправляется без LatestRelease.
	ReleasesDir string `yaml:"releases_dir" env:"RELEASES_DIR" env-default:""`
}

// GroupConfig — параметры server relay для больших групп.
// Если DBPath пусто — функция server relay отключена, всё идёт через P2P.
type GroupConfig struct {
	DBPath string `yaml:"db_path" env:"GROUP_DB_PATH" env-default:""`

	// P2PThreshold — порог в участниках, после которого группа должна перейти на server relay.
	// Ниже порога — P2P mesh. На этом значении и выше — server relay.
	P2PThreshold int `yaml:"p2p_threshold" env:"GROUP_P2P_THRESHOLD" env-default:"50"`

	// E2EThreshold — порог отказа от e2e в пользу plaintext+compression на сервере.
	// Группы >= E2EThreshold принимают только plaintext payload (Encrypted=false).
	E2EThreshold int `yaml:"e2e_threshold" env:"GROUP_E2E_THRESHOLD" env-default:"200"`

	// Rate limit defaults (token bucket per user per group).
	RateBurst    int           `yaml:"rate_burst" env:"GROUP_RATE_BURST" env-default:"30"`
	RateRefill   int           `yaml:"rate_refill" env:"GROUP_RATE_REFILL" env-default:"5"` // токенов/сек
	MaxMsgBytes  int           `yaml:"max_msg_bytes" env:"GROUP_MAX_MSG_BYTES" env-default:"32768"`
	MaxSyncLimit int           `yaml:"max_sync_limit" env:"GROUP_MAX_SYNC_LIMIT" env-default:"500"`
	Retention    time.Duration `yaml:"retention" env:"GROUP_RETENTION" env-default:"168h"` // 7 дней
}

// Enabled проверяет включён ли server relay (установлен путь к БД).
func (g GroupConfig) Enabled() bool {
	return g.DBPath != ""
}

// ClusterConfig — узлы кластера (docs/cluster-contract.md §2). Пустой
// Peers — одиночный сервер. Peers: "ws://host:port/ws=<hex pubkey>;…",
// себя можно не исключать — узел сам себя пропустит.
type ClusterConfig struct {
	SelfAddr  string `yaml:"self_addr" env:"CLUSTER_SELF_ADDR" env-default:""`
	Peers     string `yaml:"peers" env:"CLUSTER_PEERS" env-default:""`
	SoftLimit int    `yaml:"soft_limit_clients" env:"CLUSTER_SOFT_LIMIT" env-default:"400"`
}

type DebugConfig struct {
	// Host — адрес debug-порта; по умолчанию только localhost, снаружи не виден.
	Host string `yaml:"debug_host" env:"DEBUG_HOST" env-default:"127.0.0.1"`
	Port int    `yaml:"debug_port" env:"DEBUG_PORT" env-default:"0"`
	Dir  string `yaml:"debug_dir" env:"DEBUG_DIR" env-default:"./debug-logs"`
	// Пределы хранения логов клиентов (диск узла маленький).
	MaxTotalMB    int `yaml:"debug_max_total_mb" env:"DEBUG_MAX_TOTAL_MB" env-default:"1024"`
	MaxAgeDays    int `yaml:"debug_max_age_days" env:"DEBUG_MAX_AGE_DAYS" env-default:"14"`
	KeepPerSource int `yaml:"debug_keep_per_source" env:"DEBUG_KEEP_PER_SOURCE" env-default:"10"`
}

// AdminConfig — admin API /admin/v1/* (design/northwind-site/ADMIN-CONTRACT.md §1).
// Token короче 32 символов — API выключен, все /admin/v1/* отвечают 404.
type AdminConfig struct {
	Token string `yaml:"admin_token" env:"ADMIN_TOKEN" env-default:""`
	// ReleasePubKey — base64 Ed25519 ключ релизов (тот же, что в клиентах).
	// Пусто — PUT /admin/v1/releases отвечает 503.
	ReleasePubKey string `yaml:"release_pubkey" env:"RELEASE_PUBKEY" env-default:""`
}

type TURNConfig struct {
	URL        string `yaml:"turn_url" env:"TURN_URL"`
	Username   string `yaml:"turn_username" env:"TURN_USERNAME"`
	Credential string `yaml:"turn_credential" env:"TURN_CREDENTIAL"`
}

// HasTURN проверяет настроен ли TURN-сервер
func (t TURNConfig) HasTURN() bool {
	return t.URL != ""
}

type SFUConfig struct {
	Endpoint string `yaml:"sfu_endpoint" env:"SFU_ENDPOINT" env-default:""`
	Secret   string `yaml:"sfu_secret" env:"SFU_SECRET" env-default:""`
}

type WSConfig struct {
	Host string `yaml:"host" env:"WS_HOST" env-default:"0.0.0.0"`
	Port int    `yaml:"port" env:"WS_PORT" env-default:"8080"`
	Path string `yaml:"path" env:"WS_PATH" env-default:"/ws"`

	ReadTimeout  time.Duration `yaml:"read_timeout" env:"WS_READ_TIMEOUT" env-default:"10s"`
	WriteTimeout time.Duration `yaml:"write_timeout" env:"WS_WRITE_TIMEOUT" env-default:"10s"`
	IdleTimeout  time.Duration `yaml:"idle_timeout" env:"WS_IDLE_TIMEOUT" env-default:"60s"`
}

// Addr возвращает host:port для ListenAndServe
func (ws WSConfig) Addr() string {
	return fmt.Sprintf("%s:%d", ws.Host, ws.Port)
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
		return fmt.Errorf("%s: config file does not exist: %s, %w", op, path, err)
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
