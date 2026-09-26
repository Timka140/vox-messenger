package services

import (
	"fmt"
	"strings"

	"vox_app/internal/network"
)

// Возможности клиента и канал звука демонстрации (фаза 5e).
//
// Новые клиенты при каждом открытии DataChannel (и после ICE-restart) шлют
// {"type":"caps","content":"screenAudio"}. Отправитель демонстрации открывает
// DataChannel "screen-audio" только тем, кто прислал caps: старый клиент
// незнакомый label отдал бы в default-ветку OnDataChannel и затёр DC
// сообщений. Старые клиенты сообщение caps молча игнорируют.
const capScreenAudio = "screenAudio"

var capsMessage = []byte(`{"type":"caps","content":"` + capScreenAudio + `"}`)

func (s *NetworkService) sendCaps(peerID string) {
	_ = s.SendP2PData(peerID, capsMessage)
}

// setPeerCaps — caps пира (content: имена через запятую), перезаписывает прежние.
func (s *NetworkService) setPeerCaps(peerID, content string) {
	m := make(map[string]bool)
	for _, c := range strings.Split(content, ",") {
		if c = strings.TrimSpace(c); c != "" {
			m[c] = true
		}
	}
	s.capsMu.Lock()
	if s.peerCaps == nil {
		s.peerCaps = make(map[string]map[string]bool)
	}
	s.peerCaps[peerID] = m
	s.capsMu.Unlock()
}

func (s *NetworkService) clearPeerCaps(peerID string) {
	s.capsMu.Lock()
	delete(s.peerCaps, peerID)
	s.capsMu.Unlock()
}

func (s *NetworkService) peerHasCap(peerID, capName string) bool {
	s.capsMu.RLock()
	defer s.capsMu.RUnlock()
	return s.peerCaps[peerID][capName]
}

// SetOnScreenAudio — callback входящих пакетов звука демонстрации.
func (s *NetworkService) SetOnScreenAudio(fn func(fromPubKey string, data []byte)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onScreenAudio = fn
	if s.peerMgr != nil {
		s.peerMgr.SetOnScreenAudio(network.OnP2PMessage(fn))
	}
}

// EnsureScreenAudioDC — канал звука на screen PC с пиром (сторона демонстрирующего).
func (s *NetworkService) EnsureScreenAudioDC(peerID string) error {
	s.mu.RLock()
	pm := s.peerMgr
	s.mu.RUnlock()
	if pm == nil {
		return fmt.Errorf("no peer manager")
	}
	return pm.EnsureScreenAudioDC(peerID)
}

// SendScreenAudio — пакет звука демонстрации пиру.
func (s *NetworkService) SendScreenAudio(peerID string, data []byte) error {
	s.mu.RLock()
	pm := s.peerMgr
	s.mu.RUnlock()
	if pm == nil {
		return fmt.Errorf("no peer manager")
	}
	return pm.SendScreenAudio(peerID, data)
}
