package services

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Блокировка пользователя (фаза 7b редизайна, решения Тима 2026-09-15).
//
// Полный игнор: от заблокированного не принимаются сигналы (offer/ICE,
// заявки, звонки, peer_online), P2P-соединения (HandleOffer), личные
// сообщения, голос и демонстрация; сами к нему не подключаемся (Connect*).
// Дружба и история сохраняются, разблокировка всё возвращает. Ему ничего
// не сообщаем. В группах его сообщения скрывает фронт с пометкой.
//
// Список — таблица blocked_peers (миграция 63) и копия в памяти для горячих
// путей (голосовые кадры, диспетчеры).

type blockList struct {
	mu     sync.RWMutex
	ids    map[string]bool
	loaded bool
}

// BlockedPeer — строка списка «Заблокированные» в настройках.
type BlockedPeer struct {
	PeerID    string `json:"peerId"`
	Nickname  string `json:"nickname"`
	BlockedAt int64  `json:"blockedAt"` // unix, сек
}

// ensureBlockedLoaded — ленивая загрузка списка; при ошибке БД — повтор при
// следующем обращении.
func (s *NetworkService) ensureBlockedLoaded() {
	s.blocks.mu.RLock()
	loaded := s.blocks.loaded
	s.blocks.mu.RUnlock()
	if loaded || s.store == nil || s.store.DB() == nil {
		return
	}
	rows, err := s.store.DB().Query("SELECT peer_id FROM blocked_peers")
	if err != nil {
		return
	}
	defer rows.Close()
	ids := make(map[string]bool)
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids[id] = true
		}
	}
	s.blocks.mu.Lock()
	if !s.blocks.loaded {
		s.blocks.ids = ids
		s.blocks.loaded = true
	}
	s.blocks.mu.Unlock()
}

// IsBlocked — заблокирован ли пир.
func (s *NetworkService) IsBlocked(peerID string) bool {
	if peerID == "" {
		return false
	}
	s.ensureBlockedLoaded()
	s.blocks.mu.RLock()
	defer s.blocks.mu.RUnlock()
	return s.blocks.ids[peerID]
}

// BlockPeer — заблокировать: запись в таблицу, разрыв текущего соединения,
// событие vox:peer-blocked.
func (s *NetworkService) BlockPeer(peerID string) error {
	const op = "NetworkService.BlockPeer"
	if peerID == "" {
		return fmt.Errorf("%s: пустой peerID", op)
	}
	if me, _ := s.sess.PeerID(); peerID == me {
		return fmt.Errorf("%s: нельзя заблокировать себя", op)
	}
	s.ensureBlockedLoaded()
	nick := s.peerNickname(peerID)
	if _, err := s.store.DB().Exec(
		"INSERT OR REPLACE INTO blocked_peers (peer_id, nickname, blocked_at) VALUES (?, ?, ?)",
		peerID, nick, time.Now().Unix()); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	s.blocks.mu.Lock()
	if s.blocks.ids == nil {
		s.blocks.ids = make(map[string]bool)
	}
	s.blocks.ids[peerID] = true
	s.blocks.mu.Unlock()
	s.log.Info("пир заблокирован", slog.String("peer", safeShort(peerID)))
	s.dropPeerConnection(peerID)
	s.emitEvent("vox:peer-blocked", map[string]string{"peerId": peerID})
	return nil
}

// UnblockPeer — разблокировать; друга сразу пробуем подключить снова.
func (s *NetworkService) UnblockPeer(peerID string) error {
	const op = "NetworkService.UnblockPeer"
	if _, err := s.store.DB().Exec("DELETE FROM blocked_peers WHERE peer_id = ?", peerID); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	s.blocks.mu.Lock()
	delete(s.blocks.ids, peerID)
	s.blocks.mu.Unlock()
	s.log.Info("пир разблокирован", slog.String("peer", safeShort(peerID)))
	s.emitEvent("vox:peer-unblocked", map[string]string{"peerId": peerID})
	var status string
	if err := s.store.DB().QueryRow("SELECT status FROM contacts WHERE peer_id = ?", peerID).Scan(&status); err == nil && status == "accepted" {
		go s.ConnectToPeer(peerID)
	}
	return nil
}

// GetBlockedPeers — список для настроек, свежие сверху.
func (s *NetworkService) GetBlockedPeers() ([]BlockedPeer, error) {
	rows, err := s.store.DB().Query("SELECT peer_id, nickname, blocked_at FROM blocked_peers ORDER BY blocked_at DESC")
	if err != nil {
		return nil, fmt.Errorf("NetworkService.GetBlockedPeers: %w", err)
	}
	defer rows.Close()
	out := []BlockedPeer{}
	for rows.Next() {
		var p BlockedPeer
		if rows.Scan(&p.PeerID, &p.Nickname, &p.BlockedAt) == nil {
			out = append(out, p)
		}
	}
	return out, nil
}

// dropPeerConnection — разрыв P2P как при peer_offline (network_signal.go):
// ClosePeer удаляет пира из map до PC.Close(), поэтому события
// OnConnectionStateChange не будет — шлём сами.
func (s *NetworkService) dropPeerConnection(peerID string) {
	s.mu.RLock()
	pm := s.peerMgr
	s.mu.RUnlock()
	if pm != nil {
		pm.ClosePeer(peerID)
	}
	s.emitEvent("vox:peer-disconnected", map[string]string{"id": peerID})
	if s.onVoicePeerDisconnected != nil {
		s.onVoicePeerDisconnected(peerID)
	}
	s.emitBus(TopicPeerOffline, peerID)
	s.cancelReconnect(peerID)
}

// dropFromBlocked — какие P2P-типы от заблокированного отбрасываются
// (страховка в dispatchP2P, если канал всё же открыт). Состав групп и
// голосового канала (group_*, voice_join/leave/status/members) пропускаем,
// чтобы списки участников оставались верными; сообщения групп скрывает фронт.
func dropFromBlocked(msgType string) bool {
	if strings.HasPrefix(msgType, "group_") {
		return false
	}
	switch msgType {
	case "voice_join", "voice_leave", "voice_status", "voice_members":
		return false
	}
	return true
}
