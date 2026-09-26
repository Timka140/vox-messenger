package services

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // DecodeConfig для JPEG
	"log/slog"
	"sync"
	"time"

	"vox_app/internal/session"
	"vox_app/internal/storage"
)

// AvatarService — фото профиля и фото групп (фаза 5a редизайна).
//
// Своё фото и фото пиров лежат в peer_profiles (у не-друзей нет строки в
// contacts), фото группы — в groups.avatar. Формат один: JPEG 256×256 не
// больше 40 КБ — base64 такого файла вместе с JSON влезает в одно сообщение
// DataChannel (SCTP max message 64 КиБ, а SendData не режет на куски).
//
// Обмен между пирами:
//   - на каждое открытие DataChannel и после ICE-restart шлём avatar_hash;
//   - получатель сравнивает с сохранённым хешем этого пира и при отличии
//     просит avatar_get → получает avatar_data;
//   - фото группы владелец рассылает group_avatar (как rename: онлайн по
//     P2P, офлайн через pending_group_actions); новичок узнаёт хеш из
//     приглашения и просит group_avatar_get.
type AvatarService struct {
	log     *slog.Logger
	sess    *session.Session
	store   *storage.Storage
	network *NetworkService
	groups  *GroupsService

	mu       sync.Mutex
	inflight map[string]time.Time // peerID → когда отправили avatar_get
}

const (
	avatarSide     = 256
	maxAvatarBytes = 40 * 1024
	// avatarGetRetry — повторный avatar_get тому же пиру не раньше этого:
	// открытие DC и смена фото приходят подряд, ответ весит 55 КБ.
	avatarGetRetry = 30 * time.Second
)

var errAvatarInvalid = errors.New("avatar_invalid")

func NewAvatarService(log *slog.Logger, sess *session.Session, store *storage.Storage, network *NetworkService, groups *GroupsService) *AvatarService {
	a := &AvatarService{
		log:      log,
		sess:     sess,
		store:    store,
		network:  network,
		groups:   groups,
		inflight: make(map[string]time.Time),
	}
	network.SetOnProfileSync(a.syncToPeer)
	return a
}

// SetAvatarService подключает обработку avatar_* / group_avatar* в роутинг P2P.
func (s *MessagingService) SetAvatarService(a *AvatarService) {
	s.avatars = a
}

// validateAvatarJPEG — JPEG 256×256 не больше maxAvatarBytes.
func validateAvatarJPEG(data []byte) error {
	if len(data) == 0 || len(data) > maxAvatarBytes {
		return fmt.Errorf("%w: size %d", errAvatarInvalid, len(data))
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("%w: %v", errAvatarInvalid, err)
	}
	if format != "jpeg" || cfg.Width != avatarSide || cfg.Height != avatarSide {
		return fmt.Errorf("%w: %s %dx%d", errAvatarInvalid, format, cfg.Width, cfg.Height)
	}
	return nil
}

func avatarHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func decodeAvatar(b64 string) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("%w: base64", errAvatarInvalid)
	}
	return data, validateAvatarJPEG(data)
}

// --- Публичные методы (фронт) ---

// SetMyAvatar сохраняет своё фото (base64 JPEG из редактора) и рассылает
// новый хеш подключённым пирам. Возвращает хеш для URL /vox-avatar/…?v=.
func (a *AvatarService) SetMyAvatar(b64 string) (string, error) {
	const op = "AvatarService.SetMyAvatar"
	data, err := decodeAvatar(b64)
	if err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}
	myID, err := a.sess.PeerID()
	if err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}
	hash := avatarHash(data)
	if err := a.saveProfileAvatar(myID, hash, data); err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}
	a.emitUpdated(myID, hash)
	a.broadcastHash(hash)
	a.log.Info("avatar: своё фото обновлено", slog.Int("bytes", len(data)))
	return hash, nil
}

// ClearMyAvatar удаляет своё фото; пиры получат пустой хеш и вернут буквы.
func (a *AvatarService) ClearMyAvatar() error {
	const op = "AvatarService.ClearMyAvatar"
	myID, err := a.sess.PeerID()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if err := a.saveProfileAvatar(myID, "", nil); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	a.emitUpdated(myID, "")
	a.broadcastHash("")
	a.log.Info("avatar: своё фото удалено")
	return nil
}

// SetGroupAvatar — фото группы (только владелец). Участники получат его
// как rename: онлайн сразу, офлайн — из очереди при подключении.
func (a *AvatarService) SetGroupAvatar(groupID, b64 string) (string, error) {
	const op = "AvatarService.SetGroupAvatar"
	data, err := decodeAvatar(b64)
	if err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}
	myID, err := a.requireGroupOwner(groupID)
	if err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}
	hash := avatarHash(data)
	if _, err := a.store.DB().Exec("UPDATE groups SET avatar_hash = ?, avatar = ? WHERE group_id = ?", hash, data, groupID); err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}
	a.emitUpdated("g:"+groupID, hash)
	a.broadcastGroupAvatar(groupID, myID, b64)
	a.log.Info("avatar: фото группы обновлено", slog.String("group", safeShort8(groupID)), slog.Int("bytes", len(data)))
	return hash, nil
}

// ClearGroupAvatar удаляет фото группы у всех участников.
func (a *AvatarService) ClearGroupAvatar(groupID string) error {
	const op = "AvatarService.ClearGroupAvatar"
	myID, err := a.requireGroupOwner(groupID)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if _, err := a.store.DB().Exec("UPDATE groups SET avatar_hash = '', avatar = NULL WHERE group_id = ?", groupID); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	a.emitUpdated("g:"+groupID, "")
	a.broadcastGroupAvatar(groupID, myID, "")
	return nil
}

// GetAvatarHashes — хеши всех известных фото: peerId → hash и "g:<groupId>" → hash.
func (a *AvatarService) GetAvatarHashes() (map[string]string, error) {
	const op = "AvatarService.GetAvatarHashes"
	out := make(map[string]string)
	rows, err := a.store.DB().Query("SELECT peer_id, avatar_hash FROM peer_profiles WHERE avatar_hash != ''")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	for rows.Next() {
		var id, h string
		if rows.Scan(&id, &h) == nil {
			out[id] = h
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	grows, err := a.store.DB().Query("SELECT group_id, avatar_hash FROM groups WHERE avatar_hash != ''")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer grows.Close()
	for grows.Next() {
		var id, h string
		if grows.Scan(&id, &h) == nil {
			out["g:"+id] = h
		}
	}
	return out, grows.Err()
}

// --- Хранилище ---

// saveProfileAvatar — hash "" и data nil = удалить фото (ник не трогаем).
func (a *AvatarService) saveProfileAvatar(peerID, hash string, data []byte) error {
	var blob any
	if len(data) > 0 {
		blob = data
	}
	_, err := a.store.DB().Exec(
		`INSERT INTO peer_profiles (peer_id, avatar_hash, avatar, updated_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(peer_id) DO UPDATE SET avatar_hash = excluded.avatar_hash, avatar = excluded.avatar, updated_at = excluded.updated_at`,
		peerID, hash, blob, time.Now().Unix())
	return err
}

func (a *AvatarService) storedHash(peerID string) string {
	var h string
	a.store.DB().QueryRow("SELECT avatar_hash FROM peer_profiles WHERE peer_id = ?", peerID).Scan(&h)
	return h
}

func (a *AvatarService) profileAvatar(peerID string) (string, []byte) {
	var h string
	var data []byte
	a.store.DB().QueryRow("SELECT avatar_hash, avatar FROM peer_profiles WHERE peer_id = ?", peerID).Scan(&h, &data)
	return h, data
}

// requireGroupOwner — фото группы меняет только владелец, который в ней состоит.
func (a *AvatarService) requireGroupOwner(groupID string) (string, error) {
	myID, err := a.sess.PeerID()
	if err != nil {
		return "", err
	}
	var ownerID, status string
	a.store.DB().QueryRow("SELECT owner_id FROM groups WHERE group_id = ?", groupID).Scan(&ownerID)
	a.store.DB().QueryRow("SELECT status FROM group_members WHERE group_id = ? AND peer_id = ?", groupID, myID).Scan(&status)
	if ownerID != myID || status != "active" {
		return "", errors.New("только владелец группы")
	}
	return myID, nil
}

// --- P2P ---

func (a *AvatarService) send(peerID string, msg P2PMessage) {
	if msg.Time == 0 {
		msg.Time = time.Now().Unix()
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	if err := a.network.SendP2PData(peerID, data); err != nil {
		a.log.Debug("avatar: не отправлено", slog.String("type", msg.Type), slog.String("peer", safeShort(peerID)), slog.String("error", err.Error()))
	}
}

func (a *AvatarService) connectedPeers() []string {
	a.network.mu.RLock()
	pm := a.network.peerMgr
	a.network.mu.RUnlock()
	if pm == nil {
		return nil
	}
	return pm.GetConnectedPeers()
}

// syncToPeer — DataChannel с пиром готов: сообщаем хеш своего фото.
func (a *AvatarService) syncToPeer(peerID string) {
	myID, err := a.sess.PeerID()
	if err != nil {
		return
	}
	a.send(peerID, P2PMessage{Type: "avatar_hash", Content: a.storedHash(myID)})
}

func (a *AvatarService) broadcastHash(hash string) {
	for _, pid := range a.connectedPeers() {
		a.send(pid, P2PMessage{Type: "avatar_hash", Content: hash})
	}
}

func (a *AvatarService) broadcastGroupAvatar(groupID, myID, b64 string) {
	// Прежнее фото, не доставленное офлайн-участникам, заменяем новым —
	// иначе каждая смена копила бы в очереди по 55 КБ на участника.
	a.store.DB().Exec("DELETE FROM pending_group_actions WHERE group_id = ? AND type = 'group_avatar'", groupID)
	a.groups.broadcastGroupActionWithPayload("group_avatar", groupID, myID, a.groups.GetGroupMemberIDs(groupID), b64)
}

func (a *AvatarService) emitUpdated(id, hash string) {
	a.network.emitEvent("vox:avatar-updated", map[string]string{"id": id, "hash": hash})
}

// beginGet — true, если avatar_get этому пиру можно отправить сейчас.
func (a *AvatarService) beginGet(peerID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if t, ok := a.inflight[peerID]; ok && time.Since(t) < avatarGetRetry {
		return false
	}
	a.inflight[peerID] = time.Now()
	return true
}

func (a *AvatarService) endGet(peerID string) {
	a.mu.Lock()
	delete(a.inflight, peerID)
	a.mu.Unlock()
}

// handleP2P — avatar_* и group_avatar* из MessagingService.dispatchP2P
// (только прямой P2P, не mesh).
func (a *AvatarService) handleP2P(from string, msg *P2PMessage) {
	switch msg.Type {
	case "avatar_hash":
		a.onAvatarHash(from, msg.Content)
	case "avatar_get":
		a.onAvatarGet(from)
	case "avatar_data":
		a.onAvatarData(from, msg)
	case "group_avatar":
		a.onGroupAvatar(from, msg)
	case "group_avatar_get":
		a.onGroupAvatarGet(from, msg.GroupID)
	}
}

func (a *AvatarService) onAvatarHash(from, hash string) {
	stored := a.storedHash(from)
	if hash == stored {
		return
	}
	if hash == "" {
		if err := a.saveProfileAvatar(from, "", nil); err == nil {
			a.emitUpdated(from, "")
		}
		return
	}
	if a.beginGet(from) {
		a.send(from, P2PMessage{Type: "avatar_get"})
	}
}

func (a *AvatarService) onAvatarGet(from string) {
	myID, err := a.sess.PeerID()
	if err != nil {
		return
	}
	hash, data := a.profileAvatar(myID)
	if len(data) == 0 {
		a.send(from, P2PMessage{Type: "avatar_hash", Content: ""})
		return
	}
	a.send(from, P2PMessage{Type: "avatar_data", Content: hash, Data: base64.StdEncoding.EncodeToString(data)})
}

func (a *AvatarService) onAvatarData(from string, msg *P2PMessage) {
	a.endGet(from)
	data, err := decodeAvatar(msg.Data)
	if err != nil {
		a.log.Warn("avatar: невалидное фото от пира", slog.String("peer", safeShort(from)), slog.String("error", err.Error()))
		return
	}
	hash := avatarHash(data)
	if msg.Content != "" && msg.Content != hash {
		a.log.Warn("avatar: хеш не совпал", slog.String("peer", safeShort(from)))
		return
	}
	if err := a.saveProfileAvatar(from, hash, data); err != nil {
		a.log.Warn("avatar: не сохранено", slog.String("peer", safeShort(from)), slog.String("error", err.Error()))
		return
	}
	a.emitUpdated(from, hash)
	a.log.Info("avatar: получено фото пира", slog.String("peer", safeShort(from)), slog.Int("bytes", len(data)))
}

func (a *AvatarService) onGroupAvatar(from string, msg *P2PMessage) {
	if !a.groups.senderIsGroupAdmin(msg.GroupID, from) {
		a.log.Warn("group_avatar от не-админа, игнорируем",
			slog.String("group", safeShort8(msg.GroupID)), slog.String("peer", safeShort(from)))
		return
	}
	if msg.Content == "" {
		a.store.DB().Exec("UPDATE groups SET avatar_hash = '', avatar = NULL WHERE group_id = ?", msg.GroupID)
		a.emitUpdated("g:"+msg.GroupID, "")
		return
	}
	data, err := decodeAvatar(msg.Content)
	if err != nil {
		a.log.Warn("group_avatar: невалидное фото", slog.String("error", err.Error()))
		return
	}
	hash := avatarHash(data)
	a.store.DB().Exec("UPDATE groups SET avatar_hash = ?, avatar = ? WHERE group_id = ?", hash, data, msg.GroupID)
	a.emitUpdated("g:"+msg.GroupID, hash)
}

// onGroupAvatarGet — новичок просит фото группы; отвечает только админ,
// и только активному участнику.
func (a *AvatarService) onGroupAvatarGet(from, groupID string) {
	myID, err := a.sess.PeerID()
	if err != nil || !a.groups.senderIsGroupAdmin(groupID, myID) {
		return
	}
	var status string
	a.store.DB().QueryRow("SELECT status FROM group_members WHERE group_id = ? AND peer_id = ?", groupID, from).Scan(&status)
	if status != "active" {
		return
	}
	var data []byte
	a.store.DB().QueryRow("SELECT avatar FROM groups WHERE group_id = ?", groupID).Scan(&data)
	if len(data) == 0 {
		return
	}
	a.send(from, P2PMessage{Type: "group_avatar", GroupID: groupID, Content: base64.StdEncoding.EncodeToString(data)})
}
