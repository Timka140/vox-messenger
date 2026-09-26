package services

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"vox_app/internal/network"

	"github.com/pion/webrtc/v4"
)

// loadICEServers загружает сохранённые ICE серверы из БД и применяет к PeerManager
func (s *NetworkService) loadICEServers(pm *network.PeerManager) {
	var iceJSON string
	s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'ice_servers'").Scan(&iceJSON)
	if iceJSON == "" {
		return
	}
	var allServers []webrtc.ICEServer
	if err := json.Unmarshal([]byte(iceJSON), &allServers); err != nil {
		s.log.Warn("не удалось разобрать ICE конфиг из БД", slog.String("error", err.Error()))
		return
	}
	if len(allServers) > 0 {
		pm.SetICEServers(allServers)
		s.log.Info("ICE серверы загружены из настроек", slog.Int("count", len(allServers)))
	}
}

// SaveICEServers сохраняет конфигурацию ICE/TURN серверов
func (s *NetworkService) SaveICEServers(configJSON string) error {
	var servers []webrtc.ICEServer
	if err := json.Unmarshal([]byte(configJSON), &servers); err != nil {
		return fmt.Errorf("некорректный формат ICE серверов: %w", err)
	}
	_, err := s.store.DB().Exec("INSERT OR REPLACE INTO settings (key, value) VALUES ('ice_servers', ?)", configJSON)
	if err != nil {
		return fmt.Errorf("не удалось сохранить ICE конфиг: %w", err)
	}
	s.store.DB().Exec("INSERT OR REPLACE INTO settings (key, value) VALUES ('ice_servers_source', 'manual')")
	s.mu.RLock()
	pm := s.peerMgr
	s.mu.RUnlock()
	if pm != nil {
		pm.SetICEServers(servers)
	}
	s.log.Info("ICE серверы обновлены", slog.Int("count", len(servers)))
	return nil
}

// GetICEServers возвращает сохранённую конфигурацию ICE серверов (JSON)
func (s *NetworkService) GetICEServers() string {
	var value string
	s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'ice_servers'").Scan(&value)
	return value
}

// GetICEServersSource возвращает источник ICE конфига: "server" или "manual"
func (s *NetworkService) GetICEServersSource() string {
	var value string
	s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'ice_servers_source'").Scan(&value)
	return value
}

// ResetICEServers сбрасывает ручную конфигурацию и применяет серверную при следующем подключении
func (s *NetworkService) ResetICEServers() error {
	s.store.DB().Exec("DELETE FROM settings WHERE key = 'ice_servers'")
	s.store.DB().Exec("DELETE FROM settings WHERE key = 'ice_servers_source'")

	// Если есть активный клиент — применить серверные ICE сразу
	s.mu.RLock()
	client := s.client
	pm := s.peerMgr
	s.mu.RUnlock()

	if client != nil {
		s.applyServerICEServers(client)
		if pm != nil {
			s.loadICEServers(pm)
		}
	}

	s.log.Info("ICE серверы сброшены к серверной конфигурации")
	return nil
}

// applyServerICEServers сохраняет ICE-серверы от сигнального сервера если пользователь не настроил вручную
func (s *NetworkService) applyServerICEServers(client *network.SignalClient) {
	serverICE := client.ServerICEServers()
	if len(serverICE) == 0 {
		return
	}
	// Не перезаписываем ручные настройки
	if s.GetICEServersSource() == "manual" {
		s.log.Info("ICE серверы от сервера проигнорированы (настроены вручную)")
		return
	}
	var servers []webrtc.ICEServer
	for _, srv := range serverICE {
		ice := webrtc.ICEServer{URLs: srv.URLs}
		if srv.Username != "" {
			ice.Username = srv.Username
			ice.Credential = srv.Credential
		}
		servers = append(servers, ice)
	}
	iceJSON, _ := json.Marshal(servers)
	s.store.DB().Exec("INSERT OR REPLACE INTO settings (key, value) VALUES ('ice_servers', ?)", string(iceJSON))
	s.store.DB().Exec("INSERT OR REPLACE INTO settings (key, value) VALUES ('ice_servers_source', 'server')")
	s.log.Info("ICE серверы получены от сервера и сохранены", slog.Int("count", len(servers)))
}
