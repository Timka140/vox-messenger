package services

import (
	"crypto/tls"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"vox_app/internal/network"
)

// GetSignalServers возвращает список сигнальных серверов
func (s *NetworkService) GetSignalServers() ([]SignalServerInfo, error) {
	const op = "NetworkService.GetSignalServers"

	entries, err := s.getServerEntries()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	s.mu.RLock()
	connectedAddr := ""
	if s.client != nil && s.client.IsConnected() {
		connectedAddr = s.client.Addr()
	}
	s.mu.RUnlock()

	servers := make([]SignalServerInfo, 0, len(entries))
	for _, e := range entries {
		servers = append(servers, SignalServerInfo{
			Addr:    e.Addr,
			ShortID: shortHash(e.Addr),
			Online:  e.Addr == connectedAddr,
			Pubkey:  e.Pubkey,
		})
	}
	return servers, nil
}

// AddSignalServer добавляет сигнальный сервер в БД.
// pubkey может быть пустым (TOFU при первом подключении), либо hex Ed25519 (64 hex char).
func (s *NetworkService) AddSignalServer(addr, pubkey string) error {
	const op = "NetworkService.AddSignalServer"

	if addr == "" {
		return fmt.Errorf("%s: пустой адрес", op)
	}
	if pubkey != "" && !isValidHexPubkey(pubkey) {
		return fmt.Errorf("%s: pubkey должен быть 64 hex символа Ed25519 ключа", op)
	}

	_, err := s.store.DB().Exec(
		"INSERT OR IGNORE INTO signal_servers (address, pubkey) VALUES (?, ?)", addr, pubkey)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	// Пользователь явно добавил адрес, пришедший из server_list, — теперь он
	// ручной и следующий список его не удалит.
	if _, err := s.store.DB().Exec(
		"UPDATE signal_servers SET source = 'manual' WHERE address = ? AND source = 'cluster'", addr,
	); err != nil {
		s.log.Warn("mark server manual failed", slog.String("addr", addr), slog.String("error", err.Error()))
	}
	// Если pubkey передан и запись уже была с пустым — обновим (опционально fix legacy).
	if pubkey != "" {
		if _, err := s.store.DB().Exec(
			"UPDATE signal_servers SET pubkey = ? WHERE address = ? AND pubkey = ''",
			pubkey, addr,
		); err != nil {
			s.log.Warn("update pubkey for existing server failed",
				slog.String("addr", addr), slog.String("error", err.Error()))
		}
	}

	s.log.Info("добавлен сигнальный сервер",
		slog.String("addr", addr),
		slog.String("pubkey", safeShort(pubkey)))
	s.emitEvent("vox:signal-updated", nil)

	// Автоматически подключаемся если ещё не подключены
	s.mu.RLock()
	connected := s.client != nil && s.client.IsConnected()
	s.mu.RUnlock()
	if !connected {
		go func() {
			if err := s.Connect("", "", ""); err != nil {
				s.log.Warn("авто-подключение не удалось", slog.String("error", err.Error()))
			}
			// Повторно уведомляем фронтенд после Connect (обновит статус online)
			s.emitEvent("vox:signal-updated", nil)
		}()
	}

	return nil
}

// RemoveSignalServer удаляет сигнальный сервер из БД
func (s *NetworkService) RemoveSignalServer(addr string) error {
	const op = "NetworkService.RemoveSignalServer"

	_, err := s.store.DB().Exec(
		"DELETE FROM signal_servers WHERE address = ?", addr)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}

	// Если были подключены к этому серверу — отключаемся
	s.mu.RLock()
	connected := s.client != nil && s.client.IsConnected() && s.client.Addr() == addr
	s.mu.RUnlock()

	if connected {
		s.Disconnect()
	}

	s.log.Info("удалён сигнальный сервер", slog.String("addr", addr))
	s.emitEvent("vox:signal-updated", nil)
	return nil
}

// GetCustomSeeds возвращает адреса серверов (алиас для совместимости с фронтендом)
func (s *NetworkService) GetCustomSeeds() ([]string, error) {
	return s.getServerAddresses()
}

// tofuStorePubkey записывает pubkey впервые увиденного сервера (TOFU).
// Обновляет только если в БД pubkey пустой — чтобы не перезатереть hardcoded pin.
func (s *NetworkService) tofuStorePubkey(addr, pubkey string) {
	if pubkey == "" {
		return
	}
	if _, err := s.store.DB().Exec(
		"UPDATE signal_servers SET pubkey = ? WHERE address = ? AND pubkey = ''",
		pubkey, addr,
	); err != nil {
		s.log.Warn("TOFU writeback failed",
			slog.String("addr", addr), slog.String("error", err.Error()))
		return
	}
	s.log.Info("TOFU: server pubkey saved",
		slog.String("addr", addr), slog.String("pubkey", safeShort(pubkey)))
}

// isValidHexPubkey проверяет что s — 64 hex символа Ed25519 публичного ключа.
func isValidHexPubkey(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

func (s *NetworkService) getServerAddresses() ([]string, error) {
	entries, err := s.getServerEntries()
	if err != nil {
		return nil, err
	}
	addrs := make([]string, 0, len(entries))
	for _, e := range entries {
		addrs = append(addrs, e.Addr)
	}
	return addrs, nil
}

// getServerEntries возвращает все signal_servers записи (адрес + ожидаемый pubkey).
// Перед чтением всегда merge'ит hardcoded-список из network.KnownSignalServers
// (idempotent INSERT OR IGNORE + UPDATE backfill пустых pubkey). Это значит
// что любая правка KnownSignalServers (новый сервер, ротация ключа, фикс порта)
// автоматически подтягивается в существующие БД при следующем подключении —
// не только при first-run на пустой таблице.
func (s *NetworkService) getServerEntries() ([]signalServerEntry, error) {
	if dev := devSignalServers(); dev != nil {
		return dev, nil
	}
	if len(network.KnownSignalServers) > 0 {
		s.seedKnownServers()
	}
	return s.readServerEntries()
}

// devSignalServers — список сигнальных серверов для тестовых экземпляров из
// VOX_SIGNAL_SERVERS="host:port=pubkey,host:port=pubkey" (только ENV=local,
// как VOX_REMOTE_DEBUG_PORT). Задаёт список целиком и в этом порядке, без
// KnownSignalServers и БД: так тестовый клиент можно посадить на нужный
// узел кластера. nil — переменная не задана.
func devSignalServers() []signalServerEntry {
	if os.Getenv("ENV") != "local" {
		return nil
	}
	raw := strings.TrimSpace(os.Getenv("VOX_SIGNAL_SERVERS"))
	if raw == "" {
		return nil
	}
	var out []signalServerEntry
	for _, item := range strings.Split(raw, ",") {
		addr, pub, _ := strings.Cut(strings.TrimSpace(item), "=")
		if addr = strings.TrimSpace(addr); addr == "" {
			continue
		}
		out = append(out, signalServerEntry{Addr: addr, Pubkey: strings.TrimSpace(pub)})
	}
	return out
}

func (s *NetworkService) readServerEntries() ([]signalServerEntry, error) {
	rows, err := s.store.DB().Query("SELECT address, pubkey FROM signal_servers ORDER BY added_at")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []signalServerEntry
	for rows.Next() {
		var e signalServerEntry
		if err := rows.Scan(&e.Addr, &e.Pubkey); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// seedKnownServers вставляет hardcoded-список в БД (idempotent через INSERT OR IGNORE).
// Вызывается когда signal_servers пуст. Также backfill'ит pubkey для уже существующих
// записей с тем же address но пустым pubkey (миграция legacy → pinned).
func (s *NetworkService) seedKnownServers() {
	for _, k := range network.KnownSignalServers {
		if _, err := s.store.DB().Exec(
			"INSERT OR IGNORE INTO signal_servers (address, pubkey, source) VALUES (?, ?, 'builtin')",
			k.Address, k.Pubkey,
		); err != nil {
			s.log.Warn("seed known signal server failed",
				slog.String("addr", k.Address), slog.String("error", err.Error()))
			continue
		}
		// Запись могла появиться раньше (старый сид, server_list) — встроенный
		// сервер не должен удаляться новым server_list.
		if _, err := s.store.DB().Exec(
			"UPDATE signal_servers SET source = 'builtin' WHERE address = ? AND source <> 'builtin'",
			k.Address,
		); err != nil {
			s.log.Warn("mark builtin signal server failed",
				slog.String("addr", k.Address), slog.String("error", err.Error()))
		}
		// Backfill: если запись уже была с пустым pubkey — заполняем из hardcoded.
		if _, err := s.store.DB().Exec(
			"UPDATE signal_servers SET pubkey = ? WHERE address = ? AND pubkey = ''",
			k.Pubkey, k.Address,
		); err != nil {
			s.log.Warn("backfill signal_server pubkey failed",
				slog.String("addr", k.Address), slog.String("error", err.Error()))
		}
	}
	s.dropRetiredBuiltinServers()
}

// dropRetiredBuiltinServers удаляет встроенные записи, которых больше нет в
// KnownSignalServers: узел сменил адрес (IP → имя), и без этого у обновившихся
// клиентов остаётся дубль того же сервера под старым адресом. Ручные и
// cluster-записи не трогает.
func (s *NetworkService) dropRetiredBuiltinServers() {
	if len(network.KnownSignalServers) == 0 {
		return
	}
	args := make([]any, 0, len(network.KnownSignalServers))
	for _, k := range network.KnownSignalServers {
		args = append(args, k.Address)
	}
	q := "DELETE FROM signal_servers WHERE source = 'builtin' AND address NOT IN (?" +
		strings.Repeat(", ?", len(args)-1) + ")"
	res, err := s.store.DB().Exec(q, args...)
	if err != nil {
		s.log.Warn("drop retired builtin signal servers failed", slog.String("error", err.Error()))
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		s.log.Info("retired builtin signal servers removed", slog.Int64("count", n))
	}
}

// maxClusterNodes — предел узлов из одного server_list.
const maxClusterNodes = 32

// mergeClusterServers — список узлов от сервера (server_list,
// docs/cluster-contract.md §6.1). Записи source='cluster' заменяются им
// целиком; manual и builtin не трогаются. Pubkey существующей записи
// заполняется, только если он пустой: закреплённый ключ главнее списка.
func (s *NetworkService) mergeClusterServers(nodes []network.ServerNode) error {
	const op = "NetworkService.mergeClusterServers"

	valid := make(map[string]string, len(nodes))
	for _, n := range nodes {
		if len(valid) >= maxClusterNodes {
			break
		}
		addr := strings.TrimSpace(n.Addr)
		pub := strings.ToLower(strings.TrimSpace(n.Pubkey))
		if !validServerAddr(addr) || !isValidHexPubkey(pub) {
			s.log.Warn("server_list: пропущен невалидный узел",
				slog.String("addr", addr), slog.String("pubkey", safeShort(pub)))
			continue
		}
		valid[addr] = pub
	}

	type row struct{ pubkey, source string }
	existing := make(map[string]row)
	rows, err := s.store.DB().Query("SELECT address, pubkey, source FROM signal_servers")
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	for rows.Next() {
		var addr string
		var r row
		if err := rows.Scan(&addr, &r.pubkey, &r.source); err != nil {
			rows.Close()
			return fmt.Errorf("%s: %w", op, err)
		}
		existing[addr] = r
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("%s: %w", op, err)
	}
	rows.Close()

	tx, err := s.store.DB().Begin()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	defer tx.Rollback()

	added, removed, filled := 0, 0, 0
	for addr, r := range existing {
		if _, keep := valid[addr]; r.source == "cluster" && !keep {
			if _, err := tx.Exec("DELETE FROM signal_servers WHERE address = ? AND source = 'cluster'", addr); err != nil {
				return fmt.Errorf("%s: %w", op, err)
			}
			removed++
		}
	}
	for addr, pub := range valid {
		r, ok := existing[addr]
		switch {
		case !ok:
			if _, err := tx.Exec(
				"INSERT OR IGNORE INTO signal_servers (address, pubkey, source) VALUES (?, ?, 'cluster')",
				addr, pub,
			); err != nil {
				return fmt.Errorf("%s: %w", op, err)
			}
			added++
		case r.pubkey == "":
			if _, err := tx.Exec(
				"UPDATE signal_servers SET pubkey = ? WHERE address = ? AND pubkey = ''", pub, addr,
			); err != nil {
				return fmt.Errorf("%s: %w", op, err)
			}
			filled++
		case !strings.EqualFold(r.pubkey, pub):
			s.log.Warn("server_list: ключ узла отличается от закреплённого, оставляю закреплённый",
				slog.String("addr", addr),
				slog.String("pinned", safeShort(r.pubkey)),
				slog.String("server_list", safeShort(pub)))
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}

	s.log.Info("server_list применён",
		slog.Int("nodes", len(valid)),
		slog.Int("added", added),
		slog.Int("removed", removed),
		slog.Int("pubkey_filled", filled))
	if added+removed+filled > 0 {
		s.emitEvent("vox:signal-updated", nil)
	}
	return nil
}

// validServerAddr — "host:port" с числовым портом, без схемы и пути.
func validServerAddr(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" || strings.ContainsAny(host, "/ ") {
		return false
	}
	p, err := strconv.Atoi(port)
	return err == nil && p > 0 && p < 65536
}

// shortHash генерирует короткий ID из строки (первые 8 символов sha256)
func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:4])
}

// ActiveSignalTLS — TLS-конфиг для HTTP к текущему узлу: принимает только
// сертификат, подтверждённый его ключом при входе. nil — узел без TLS (ws://).
func (s *NetworkService) ActiveSignalTLS() *tls.Config {
	s.mu.RLock()
	c := s.client
	s.mu.RUnlock()
	if c == nil {
		return nil
	}
	h := c.TLSCertHash()
	if len(h) == 0 {
		return nil
	}
	return network.PinnedTLS(h)
}

// ActiveSignalHTTPBase возвращает HTTP base URL текущего активного signal server
// (например "https://n1.northwindcdn.com:9743"). Пустая строка если нет подключения.
// Используется UpdateService для HTTP endpoints /api/update/*.
func (s *NetworkService) ActiveSignalHTTPBase() string {
	s.mu.RLock()
	c := s.client
	s.mu.RUnlock()
	if c == nil {
		return ""
	}
	addr := c.Addr() // "host:port" (TLS) или "ws://host:port" (тестовый узел)
	if addr == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(addr, "wss://"):
		addr = "https://" + strings.TrimPrefix(addr, "wss://")
	case strings.HasPrefix(addr, "ws://"):
		addr = "http://" + strings.TrimPrefix(addr, "ws://")
	case strings.HasPrefix(addr, "https://"), strings.HasPrefix(addr, "http://"):
		// уже со схемой
	default:
		addr = "https://" + addr // адрес без схемы — TLS-порт узла
	}
	// Убираем path (/ws)
	schemeEnd := strings.Index(addr, "//") + 2
	if idx := strings.Index(addr[schemeEnd:], "/"); idx >= 0 {
		addr = addr[:schemeEnd+idx]
	}
	return addr
}
