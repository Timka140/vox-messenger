package meshtastic

import (
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	meshpb "vox_app/internal/meshtastic/meshpb"
)

// DefaultMeshtasticTCPPort — стандартный порт TCP-сервера прошивки Meshtastic
// (https://meshtastic.org/docs/software/device-api/client-api/#tcp).
const DefaultMeshtasticTCPPort = 4403

// TCPConfig configures a TCP-connected Meshtastic device. Typical case —
// устройство подключено к WiFi (WifiEnabled=true + валидный IP), прошивка
// сама слушает на порту 4403.
type TCPConfig struct {
	Host string         // "192.168.1.42"
	Port int            // default 4403
	PortNum meshpb.PortNum
	Logger  *slog.Logger

	RecvBuffer int
	HopLimit   uint32
	Tracker    *NodeTracker

	// DialTimeout — ограничение на установку TCP-соединения. Default 10s.
	DialTimeout time.Duration
}

// NewTCPAdapter dials host:port (или host:4403 если Port==0) и возвращает
// Adapter, готовый к работе. В отличие от serial — WakeDelay не нужен:
// TCP-соединение установлено в момент возврата Dial, прошивка готова
// принимать запросы сразу. Отправляем want_config_id немедленно.
func NewTCPAdapter(cfg TCPConfig) (*Adapter, error) {
	if cfg.Logger == nil {
		return nil, fmt.Errorf("meshtastic.NewTCPAdapter: Logger is required")
	}
	if cfg.Host == "" {
		return nil, fmt.Errorf("meshtastic.NewTCPAdapter: Host is required")
	}
	if cfg.Port == 0 {
		cfg.Port = DefaultMeshtasticTCPPort
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 10 * time.Second
	}

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	conn, err := net.DialTimeout("tcp", addr, cfg.DialTimeout)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}

	a := newAdapter(conn, AdapterConfig{
		PortNum:    cfg.PortNum,
		Logger:     cfg.Logger,
		RecvBuffer: cfg.RecvBuffer,
		HopLimit:   cfg.HopLimit,
		Tracker:    cfg.Tracker,
	}, addr)

	a.log.Info("opened", "transport", "tcp", "portnum", a.cfg.PortNum)
	go a.readLoop()
	// На TCP прошивка Meshtastic ждёт периодический ToRadio.Heartbeat,
	// иначе через ~15 секунд тишины закрывает socket как "мёртвый клиент".
	// Интервал 5 сек — безопасный запас относительно лимита прошивки.
	go a.startHeartbeat(5 * time.Second)
	// TCP connect без warmup — прошивка сразу готова отвечать.
	a.sendWantConfigId()

	return a, nil
}

// ParseTCPAddress разбирает строку "host" / "host:port" в пару (host, port).
// Если порт не указан — возвращает DefaultMeshtasticTCPPort. Пустые/некорректные
// значения → ошибка.
func ParseTCPAddress(s string) (host string, port int, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", 0, fmt.Errorf("empty address")
	}
	// IPv6 с [brackets]:port
	if strings.HasPrefix(s, "[") {
		h, p, splitErr := net.SplitHostPort(s)
		if splitErr != nil {
			return "", 0, splitErr
		}
		pi, convErr := strconv.Atoi(p)
		if convErr != nil {
			return "", 0, fmt.Errorf("invalid port %q: %w", p, convErr)
		}
		return h, pi, nil
	}
	// Обычный host[:port] — если одно двоеточие, делим. Если ни одного — порт дефолт.
	if idx := strings.LastIndex(s, ":"); idx >= 0 && strings.Count(s, ":") == 1 {
		h := s[:idx]
		p := s[idx+1:]
		pi, convErr := strconv.Atoi(p)
		if convErr != nil {
			return "", 0, fmt.Errorf("invalid port %q: %w", p, convErr)
		}
		if h == "" {
			return "", 0, fmt.Errorf("empty host in %q", s)
		}
		return h, pi, nil
	}
	// Голый host / IPv4 без порта.
	return s, DefaultMeshtasticTCPPort, nil
}
