package services

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"vox_app/internal/session"
	"vox_app/internal/storage"
)

// FriendsService — управление друзьями и заявками
type FriendsService struct {
	log     *slog.Logger
	sess    *session.Session
	store   *storage.Storage
	network *NetworkService
}

func NewFriendsService(log *slog.Logger, sess *session.Session, store *storage.Storage, network *NetworkService) *FriendsService {
	return &FriendsService{
		log:     log,
		sess:    sess,
		store:   store,
		network: network,
	}
}

// --- Типы для фронтенда ---

type FriendRequest struct {
	ID        int    `json:"id"`
	PeerID    string `json:"peerId"`
	Nickname  string `json:"nickname"`
	Direction int    `json:"direction"` // 0 = incoming, 1 = outgoing
	Timestamp int64  `json:"timestamp"` // milliseconds
}

type Friend struct {
	PeerID   string `json:"peerId"`
	Nickname string `json:"nickname"`
	Online   bool   `json:"online"`
	Unread   int    `json:"unread"`
	Since    int64  `json:"since"` // unix seconds: contacts.created_at («В друзьях с» в профиле)
}

// --- Публичные методы (вызываются фронтендом) ---

// SendFriendRequest отправляет заявку в друзья
func (s *FriendsService) SendFriendRequest(peerID string) error {
	const op = "FriendsService.SendFriendRequest"

	if err := validatePeerID(peerID); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}

	// Проверяем что не отправляем себе
	myID, err := s.sess.PeerID()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if peerID == myID {
		return fmt.Errorf("%s: нельзя добавить себя", op)
	}

	// Проверяем что уже не друзья
	var count int
	if err := s.store.DB().QueryRow(
		"SELECT COUNT(*) FROM contacts WHERE peer_id = ? AND status = 'accepted'", peerID).Scan(&count); err != nil {
		s.log.Warn("не удалось проверить статус контакта", slog.String("peer", safeShort(peerID)), slog.String("error", err.Error()))
	}
	if count > 0 {
		return fmt.Errorf("%s: уже в друзьях", op)
	}

	// Проверяем что нет активной исходящей заявки
	if err := s.store.DB().QueryRow(
		"SELECT COUNT(*) FROM friend_requests WHERE peer_id = ? AND direction = 'outgoing' AND status = 'pending'",
		peerID).Scan(&count); err != nil {
		s.log.Warn("не удалось проверить заявки", slog.String("peer", safeShort(peerID)), slog.String("error", err.Error()))
	}
	if count > 0 {
		return fmt.Errorf("%s: заявка уже отправлена", op)
	}

	// Получаем свой nick#tag для отправки
	nickTag, err := s.getNickTag()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}

	// Отправляем через signal server
	client := s.network.Client()
	if client == nil || !client.IsConnected() {
		return fmt.Errorf("%s: не подключён к сигнальному серверу", op)
	}

	if err := client.SendFriendRequest(peerID, nickTag); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}

	// Сохраняем исходящую заявку в БД
	_, err = s.store.DB().Exec(
		"INSERT INTO friend_requests (peer_id, direction, status) VALUES (?, 'outgoing', 'pending')",
		peerID)
	if err != nil {
		return fmt.Errorf("%s: db: %w", op, err)
	}

	s.log.Info("заявка в друзья отправлена", slog.String("to", peerID))
	return nil
}

// AcceptFriendRequest принимает входящую заявку
func (s *FriendsService) AcceptFriendRequest(requestID int) error {
	const op = "FriendsService.AcceptFriendRequest"

	// Получаем заявку
	var peerID string
	err := s.store.DB().QueryRow(
		"SELECT peer_id FROM friend_requests WHERE id = ? AND direction = 'incoming' AND status = 'pending'",
		requestID).Scan(&peerID)
	if err != nil {
		return fmt.Errorf("%s: заявка не найдена: %w", op, err)
	}

	// Обновляем статус заявки
	if _, err := s.store.DB().Exec(
		"UPDATE friend_requests SET status = 'accepted' WHERE id = ?", requestID); err != nil {
		return fmt.Errorf("%s: ошибка обновления заявки: %w", op, err)
	}

	// Добавляем в контакты
	nickname := s.network.PeerNickname(peerID)
	if _, err := s.store.DB().Exec(
		"INSERT OR REPLACE INTO contacts (peer_id, nickname, status) VALUES (?, ?, 'accepted')",
		peerID, nickname); err != nil {
		return fmt.Errorf("%s: ошибка добавления контакта: %w", op, err)
	}

	// Отправляем подтверждение через signal server
	nickTag, _ := s.getNickTag()
	client := s.network.Client()
	if client != nil && client.IsConnected() {
		client.SendFriendAccept(peerID, nickTag)
	}

	s.log.Info("заявка принята", slog.String("peer", peerID))
	s.network.emitEvent("vox:friend-accepted", map[string]string{
		"peerId":   peerID,
		"nickname": nickname,
	})

	// Инициируем P2P соединение с новым другом
	s.network.ConnectToPeer(peerID)

	return nil
}

// RejectFriendRequest отклоняет входящую заявку
func (s *FriendsService) RejectFriendRequest(requestID int) error {
	const op = "FriendsService.RejectFriendRequest"

	var peerID string
	err := s.store.DB().QueryRow(
		"SELECT peer_id FROM friend_requests WHERE id = ? AND direction = 'incoming' AND status = 'pending'",
		requestID).Scan(&peerID)
	if err != nil {
		return fmt.Errorf("%s: заявка не найдена: %w", op, err)
	}

	if _, err := s.store.DB().Exec(
		"UPDATE friend_requests SET status = 'rejected' WHERE id = ?", requestID); err != nil {
		s.log.Error("ошибка отклонения заявки", slog.String("id", fmt.Sprintf("%d", requestID)), slog.String("error", err.Error()))
	}

	// Отправляем отказ через signal server
	client := s.network.Client()
	if client != nil && client.IsConnected() {
		client.SendFriendReject(peerID)
	}

	s.log.Info("заявка отклонена", slog.String("peer", peerID))
	return nil
}

// RemoveFriend удаляет друга и уведомляет другую сторону
func (s *FriendsService) RemoveFriend(peerID string) error {
	if _, err := s.store.DB().Exec("DELETE FROM contacts WHERE peer_id = ?", peerID); err != nil {
		s.log.Error("ошибка удаления контакта", slog.String("peer", peerID), slog.String("error", err.Error()))
	}
	if _, err := s.store.DB().Exec("DELETE FROM friend_requests WHERE peer_id = ?", peerID); err != nil {
		s.log.Error("ошибка удаления заявок", slog.String("peer", peerID), slog.String("error", err.Error()))
	}

	// Уведомляем другую сторону
	client := s.network.Client()
	if client != nil && client.IsConnected() {
		client.SendFriendRemove(peerID)
	}

	s.log.Info("друг удалён", slog.String("peer", peerID))
	s.network.emitEvent("vox:friend-removed", nil)
	return nil
}

// HandleFriendRemoved обрабатывает удаление нас из друзей другим пиром
func (s *FriendsService) HandleFriendRemoved(fromPubKey string) {
	if _, err := s.store.DB().Exec("DELETE FROM contacts WHERE peer_id = ?", fromPubKey); err != nil {
		s.log.Error("ошибка удаления контакта", slog.String("peer", fromPubKey), slog.String("error", err.Error()))
	}
	if _, err := s.store.DB().Exec("DELETE FROM friend_requests WHERE peer_id = ?", fromPubKey); err != nil {
		s.log.Error("ошибка удаления заявок", slog.String("peer", fromPubKey), slog.String("error", err.Error()))
	}

	s.log.Info("удалён из друзей", slog.String("by", fromPubKey))
	s.network.emitEvent("vox:friend-removed", nil)
}

// GetPendingRequests возвращает ожидающие заявки
func (s *FriendsService) GetPendingRequests() ([]FriendRequest, error) {
	const op = "FriendsService.GetPendingRequests"

	rows, err := s.store.DB().Query(
		"SELECT id, peer_id, direction, created_at FROM friend_requests WHERE status = 'pending' ORDER BY created_at DESC")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer rows.Close()

	var requests []FriendRequest
	for rows.Next() {
		var r FriendRequest
		var dir string
		var createdAt int64
		if err := rows.Scan(&r.ID, &r.PeerID, &dir, &createdAt); err != nil {
			return nil, fmt.Errorf("%s: scan: %w", op, err)
		}
		if dir == "incoming" {
			r.Direction = 0
		} else {
			r.Direction = 1
		}
		r.Timestamp = createdAt * 1000 // секунды → миллисекунды
		r.Nickname = s.network.PeerNickname(r.PeerID)
		requests = append(requests, r)
	}

	if requests == nil {
		requests = []FriendRequest{}
	}
	return requests, rows.Err()
}

// GetFriends возвращает список друзей
func (s *FriendsService) GetFriends() ([]Friend, error) {
	const op = "FriendsService.GetFriends"

	rows, err := s.store.DB().Query(
		"SELECT peer_id, nickname, COALESCE(created_at, 0) FROM contacts WHERE status = 'accepted' ORDER BY nickname, peer_id")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer rows.Close()

	var friends []Friend
	for rows.Next() {
		var f Friend
		if err := rows.Scan(&f.PeerID, &f.Nickname, &f.Since); err != nil {
			return nil, fmt.Errorf("%s: scan: %w", op, err)
		}
		if s.network != nil {
			f.Online = s.network.IsP2PConnected(f.PeerID)
		}
		f.Unread = 0
		friends = append(friends, f)
	}

	if friends == nil {
		friends = []Friend{}
	}
	return friends, rows.Err()
}

// GetContactSharing читает флаг "разрешить пирам видеть мой список контактов"
// из settings. Раньше был stub return (false, nil) — UI checkbox в settings.js
// показывал галочку, юзер её щёлкал, но значение никуда не сохранялось.
// Теперь персистится в settings('contact_sharing') как "0"/"1".
func (s *FriendsService) GetContactSharing() (bool, error) {
	var v string
	err := s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'contact_sharing'").Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil // дефолт — не делиться
	}
	if err != nil {
		return false, fmt.Errorf("GetContactSharing: %w", err)
	}
	return v == "1", nil
}

// SetContactSharing сохраняет флаг в settings.
func (s *FriendsService) SetContactSharing(share bool) error {
	v := "0"
	if share {
		v = "1"
	}
	_, err := s.store.DB().Exec(
		"INSERT OR REPLACE INTO settings (key, value) VALUES ('contact_sharing', ?)",
		v)
	if err != nil {
		return fmt.Errorf("SetContactSharing: %w", err)
	}
	s.log.Info("contact sharing changed", slog.Bool("share", share))
	return nil
}
func (s *FriendsService) DeleteConversation(peerID string) error {
	_, err := s.store.DB().Exec("UPDATE messages SET deleted = 1 WHERE peer_id = ?", peerID)
	if err != nil {
		s.log.Warn("не удалось удалить переписку",
			slog.String("peer", peerID),
			slog.String("error", err.Error()))
		return err
	}
	s.log.Info("переписка удалена", slog.String("peer", safeShort(peerID)))
	return nil
}

// --- Методы вызываемые из NetworkService при входящих сообщениях ---

// HandleIncomingFriendRequest обрабатывает входящую заявку от другого пира
func (s *FriendsService) HandleIncomingFriendRequest(fromPubKey, nickTag string) {
	// Проверяем что уже не друзья
	var count int
	if err := s.store.DB().QueryRow(
		"SELECT COUNT(*) FROM contacts WHERE peer_id = ? AND status = 'accepted'", fromPubKey).Scan(&count); err != nil {
		s.log.Warn("не удалось проверить статус контакта", slog.String("peer", safeShort(fromPubKey)), slog.String("error", err.Error()))
	}
	if count > 0 {
		// Уже друг — значит он нас удалил и добавляет заново. Авто-принимаем.
		s.log.Info("входящая заявка от уже друга, авто-принимаем", slog.String("from", fromPubKey))
		nickTag, _ := s.getNickTag()
		client := s.network.Client()
		if client != nil && client.IsConnected() {
			client.SendFriendAccept(fromPubKey, nickTag)
		}
		s.network.ConnectToPeer(fromPubKey)
		return
	}

	// Проверяем дубликат
	if err := s.store.DB().QueryRow(
		"SELECT COUNT(*) FROM friend_requests WHERE peer_id = ? AND direction = 'incoming' AND status = 'pending'",
		fromPubKey).Scan(&count); err != nil {
		s.log.Warn("не удалось проверить дубликат заявки", slog.String("peer", safeShort(fromPubKey)), slog.String("error", err.Error()))
	}
	if count > 0 {
		return // уже есть
	}

	// Сохраняем входящую заявку
	if _, err := s.store.DB().Exec(
		"INSERT INTO friend_requests (peer_id, direction, status) VALUES (?, 'incoming', 'pending')",
		fromPubKey); err != nil {
		s.log.Error("не удалось сохранить входящую заявку", slog.String("peer", safeShort(fromPubKey)), slog.String("error", err.Error()))
	}

	// Сохраняем ник в контакты (pending)
	nickname := nickTag
	if idx := strings.Index(nickTag, "#"); idx > 0 {
		nickname = nickTag[:idx]
	}
	if _, err := s.store.DB().Exec(
		"INSERT OR IGNORE INTO contacts (peer_id, nickname, status) VALUES (?, ?, 'pending')",
		fromPubKey, nickname); err != nil {
		s.log.Error("не удалось сохранить контакт", slog.String("peer", safeShort(fromPubKey)), slog.String("error", err.Error()))
	}

	s.log.Info("входящая заявка в друзья",
		slog.String("peer", safeShort(fromPubKey)))

	s.network.emitEvent("vox:friend-request", map[string]any{
		"peerId":    fromPubKey,
		"nickname":  nickname,
		"timestamp": time.Now().UnixMilli(),
	})
}

// HandleFriendAccepted обрабатывает принятие нашей заявки
func (s *FriendsService) HandleFriendAccepted(fromPubKey, nickTag string) {
	// Обновляем статус заявки
	if _, err := s.store.DB().Exec(
		"UPDATE friend_requests SET status = 'accepted' WHERE peer_id = ? AND direction = 'outgoing' AND status = 'pending'",
		fromPubKey); err != nil {
		s.log.Warn("не удалось обновить статус заявки", slog.String("peer", safeShort(fromPubKey)), slog.String("error", err.Error()))
	}

	// Добавляем в контакты
	nickname := nickTag
	if idx := strings.Index(nickTag, "#"); idx > 0 {
		nickname = nickTag[:idx]
	}
	if _, err := s.store.DB().Exec(
		"INSERT OR REPLACE INTO contacts (peer_id, nickname, status) VALUES (?, ?, 'accepted')",
		fromPubKey, nickname); err != nil {
		s.log.Error("не удалось добавить контакт", slog.String("peer", safeShort(fromPubKey)), slog.String("error", err.Error()))
	}

	s.log.Info("заявка принята", slog.String("by", fromPubKey))

	s.network.emitEvent("vox:friend-accepted", map[string]string{
		"peerId":   fromPubKey,
		"nickname": nickname,
	})

	// Инициируем P2P — обе стороны должны пытаться, collision handling разрешит
	s.network.ConnectToPeer(fromPubKey)
}

// HandleFriendRejected обрабатывает отклонение нашей заявки
func (s *FriendsService) HandleFriendRejected(fromPubKey string) {
	if _, err := s.store.DB().Exec(
		"UPDATE friend_requests SET status = 'rejected' WHERE peer_id = ? AND direction = 'outgoing' AND status = 'pending'",
		fromPubKey); err != nil {
		s.log.Warn("не удалось обновить статус отклонённой заявки", slog.String("peer", safeShort(fromPubKey)), slog.String("error", err.Error()))
	}

	s.log.Info("заявка отклонена", slog.String("by", fromPubKey))

	s.network.emitEvent("vox:friend-rejected", nil)
}

// --- internal ---

func (s *FriendsService) getNickTag() (string, error) {
	nick := s.network.MyNickname()

	peerID, err := s.sess.PeerID()
	if err != nil {
		return "", err
	}
	shortID := peerID
	if len(shortID) > 16 {
		shortID = safeShort(shortID)
	}
	if nick == "" {
		nick = shortID
	}
	return nick + "#" + shortID, nil
}

// getPeerNickname удалён — используется единый s.network.PeerNickname.
