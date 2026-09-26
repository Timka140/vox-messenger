package services

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Ссылка-приглашение в группу (фаза 5d).
//
// Владелец выпускает vox://group/<токен>, токен подписан его ed25519-ключом:
//
//	ver(1) | groupUUID(16) | ownerPub(32) | expires u32 BE(4) | nonce(8) | sig(64)
//
// Подписывается "vox-group-invite" | первые 61 байт. Вступающий проверяет
// подпись локально, подключается к владельцу напрямую (сигналка пересылает
// offer любому онлайн-пиру) и шлёт group_join_request со ссылкой. Владелец
// сверяет подпись своим ключом, срок, nonce текущей ссылки (после «Сбросить»
// старая не работает) и бан, добавляет участника обычным путём
// (group_invite + member_added) и отвечает group_join_result.
//
// Правка сигнал-сервера не нужна; владелец должен быть в сети. Заявка ждёт
// его в settings pending_group_joins и повторяется по таймеру.
const (
	inviteLinkPrefix  = "vox://group/"
	inviteLinkVersion = 1
	inviteLinkTTL     = 7 * 24 * time.Hour
	inviteSigDomain   = "vox-group-invite"
	inviteTokenLen    = 1 + 16 + 32 + 4 + 8 + 64
	inviteSignedLen   = inviteTokenLen - 64

	// joinRetryConnected — повтор заявки, пока ждём ответа на живом соединении;
	// joinRetryOffline — пока владелец недоступен.
	joinRetryConnected = 15 * time.Second
	joinRetryOffline   = 60 * time.Second
	// joinAnswerTimeout — ответа нет при живом соединении: у владельца сборка
	// без протокола вступления (старые клиенты неизвестные типы молча игнорируют).
	joinAnswerTimeout = 60 * time.Second

	pendingJoinsKey = "pending_group_joins"
)

var (
	errInviteMalformed     = errors.New("invite_invalid")
	errInviteBadSig        = errors.New("invite_invalid")
	errInviteExpired       = errors.New("invite_expired")
	errInviteOwnGroup      = errors.New("invite_own_group")
	errInviteAlreadyMember = errors.New("invite_already_member")
)

type inviteToken struct {
	GroupID  string
	OwnerPub ed25519.PublicKey
	Expires  time.Time
	Nonce    [8]byte
}

func inviteSigned(head []byte) []byte {
	return append([]byte(inviteSigDomain), head...)
}

// encodeInviteLink — ссылка для группы с UUID-идентификатором.
func encodeInviteLink(groupID string, priv ed25519.PrivateKey, expires time.Time, nonce [8]byte) (string, error) {
	u, err := uuid.Parse(groupID)
	if err != nil {
		return "", fmt.Errorf("группу %q нельзя пригласить по ссылке: %w", safeShort8(groupID), err)
	}
	raw := make([]byte, 0, inviteTokenLen)
	raw = append(raw, inviteLinkVersion)
	raw = append(raw, u[:]...)
	raw = append(raw, priv.Public().(ed25519.PublicKey)...)
	raw = binary.BigEndian.AppendUint32(raw, uint32(expires.Unix()))
	raw = append(raw, nonce[:]...)
	raw = append(raw, ed25519.Sign(priv, inviteSigned(raw))...)
	return inviteLinkPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// parseInviteLink проверяет формат и подпись. Истёкший токен возвращается
// вместе с errInviteExpired (владельцу нужен его groupID для ответа).
func parseInviteLink(link string, now time.Time) (inviteToken, error) {
	var tok inviteToken
	link = strings.TrimSpace(link)
	if !strings.HasPrefix(strings.ToLower(link), inviteLinkPrefix) {
		return tok, errInviteMalformed
	}
	raw, err := base64.RawURLEncoding.DecodeString(link[len(inviteLinkPrefix):])
	if err != nil || len(raw) != inviteTokenLen || raw[0] != inviteLinkVersion {
		return tok, errInviteMalformed
	}
	u, err := uuid.FromBytes(raw[1:17])
	if err != nil {
		return tok, errInviteMalformed
	}
	pub := ed25519.PublicKey(append([]byte(nil), raw[17:49]...))
	if !ed25519.Verify(pub, inviteSigned(raw[:inviteSignedLen]), raw[inviteSignedLen:]) {
		return tok, errInviteBadSig
	}
	tok.GroupID = u.String()
	tok.OwnerPub = pub
	tok.Expires = time.Unix(int64(binary.BigEndian.Uint32(raw[49:53])), 0)
	copy(tok.Nonce[:], raw[53:61])
	if now.After(tok.Expires) {
		return tok, errInviteExpired
	}
	return tok, nil
}

// --- Владелец: выпуск ссылки ---

// InviteLinkInfo — ссылка и её срок (мс).
type InviteLinkInfo struct {
	Link    string `json:"link"`
	Expires int64  `json:"expires"`
}

// GetGroupInviteLink — действующая ссылка группы (только владелец). Живую
// ссылку возвращает ту же — подпись ed25519 детерминирована; новую выпускает,
// если её не было или до конца срока меньше суток.
func (s *GroupsService) GetGroupInviteLink(groupID string) (InviteLinkInfo, error) {
	return s.inviteLink(groupID, false)
}

// ResetGroupInviteLink — новая ссылка; прежняя перестаёт работать.
func (s *GroupsService) ResetGroupInviteLink(groupID string) (InviteLinkInfo, error) {
	return s.inviteLink(groupID, true)
}

func (s *GroupsService) inviteLink(groupID string, reset bool) (InviteLinkInfo, error) {
	const op = "GroupsService.GetGroupInviteLink"
	myID, err := s.sess.PeerID()
	if err != nil {
		return InviteLinkInfo{}, fmt.Errorf("%s: %w", op, err)
	}
	priv, err := s.sess.PrivateKey()
	if err != nil {
		return InviteLinkInfo{}, fmt.Errorf("%s: %w", op, err)
	}
	var ownerID, nonceHex string
	var expires int64
	if err := s.store.DB().QueryRow(
		"SELECT owner_id, invite_nonce, invite_expires FROM groups WHERE group_id = ?", groupID,
	).Scan(&ownerID, &nonceHex, &expires); err != nil {
		return InviteLinkInfo{}, fmt.Errorf("%s: %w", op, err)
	}
	if ownerID != myID || s.joinMemberStatus(groupID, myID) != "active" {
		return InviteLinkInfo{}, fmt.Errorf("%s: только владелец группы", op)
	}
	now := time.Now()
	if reset || len(nonceHex) != 16 || time.Unix(expires, 0).Before(now.Add(24*time.Hour)) {
		var n [8]byte
		if _, err := rand.Read(n[:]); err != nil {
			return InviteLinkInfo{}, fmt.Errorf("%s: %w", op, err)
		}
		nonceHex = hex.EncodeToString(n[:])
		expires = now.Add(inviteLinkTTL).Unix()
		if _, err := s.store.DB().Exec("UPDATE groups SET invite_nonce = ?, invite_expires = ? WHERE group_id = ?",
			nonceHex, expires, groupID); err != nil {
			return InviteLinkInfo{}, fmt.Errorf("%s: %w", op, err)
		}
		s.log.Info("ссылка-приглашение выпущена", slog.String("group", safeShort8(groupID)), slog.Bool("reset", reset))
	}
	var nonce [8]byte
	nb, _ := hex.DecodeString(nonceHex)
	copy(nonce[:], nb)
	link, err := encodeInviteLink(groupID, priv, time.Unix(expires, 0), nonce)
	if err != nil {
		return InviteLinkInfo{}, fmt.Errorf("%s: %w", op, err)
	}
	return InviteLinkInfo{Link: link, Expires: expires * 1000}, nil
}

func (s *GroupsService) joinMemberStatus(groupID, peerID string) string {
	var st string
	s.store.DB().QueryRow("SELECT status FROM group_members WHERE group_id = ? AND peer_id = ?", groupID, peerID).Scan(&st)
	return st
}

// --- Вступающий ---

// InviteLinkPreview — что показать в карточке «Вступить».
type InviteLinkPreview struct {
	GroupID   string `json:"groupId"`
	GroupName string `json:"groupName"` // известно, если группа уже была у нас
	OwnerID   string `json:"ownerId"`
	OwnerName string `json:"ownerName"`
	Expires   int64  `json:"expires"` // мс
	Member    bool   `json:"member"`  // уже состоим
}

// ParseGroupInviteLink — проверка ссылки без отправки чего-либо.
func (s *GroupsService) ParseGroupInviteLink(link string) (InviteLinkPreview, error) {
	tok, err := parseInviteLink(link, time.Now())
	if err != nil {
		return InviteLinkPreview{}, err
	}
	ownerID := hex.EncodeToString(tok.OwnerPub)
	p := InviteLinkPreview{
		GroupID:   tok.GroupID,
		OwnerID:   ownerID,
		OwnerName: s.network.PeerNickname(ownerID),
		Expires:   tok.Expires.Unix() * 1000,
	}
	s.store.DB().QueryRow("SELECT name FROM groups WHERE group_id = ?", tok.GroupID).Scan(&p.GroupName)
	if myID, err := s.sess.PeerID(); err == nil {
		p.Member = s.joinMemberStatus(tok.GroupID, myID) == "active"
	}
	return p, nil
}

// pendingJoin — заявка на вступление, ждущая ответа владельца.
type pendingJoin struct {
	GroupID string `json:"groupId"`
	OwnerID string `json:"ownerId"`
	Link    string `json:"link"`
	Expires int64  `json:"expires"` // unix

	sentAt time.Time   // первая отправка на живом соединении
	timer  *time.Timer // повтор
}

// JoinGroupByLink — отправить владельцу заявку на вступление. Результат
// придёт событием vox:group-join-result {groupId, code}.
func (s *GroupsService) JoinGroupByLink(link string) error {
	tok, err := parseInviteLink(link, time.Now())
	if err != nil {
		return err
	}
	myID, err := s.sess.PeerID()
	if err != nil {
		return err
	}
	ownerID := hex.EncodeToString(tok.OwnerPub)
	if ownerID == myID {
		return errInviteOwnGroup
	}
	if s.joinMemberStatus(tok.GroupID, myID) == "active" {
		return errInviteAlreadyMember
	}
	s.joinMu.Lock()
	s.ensureJoinsLoadedLocked()
	if old := s.joins[tok.GroupID]; old != nil && old.timer != nil {
		old.timer.Stop()
	}
	s.joins[tok.GroupID] = &pendingJoin{GroupID: tok.GroupID, OwnerID: ownerID, Link: strings.TrimSpace(link), Expires: tok.Expires.Unix()}
	s.saveJoinsLocked()
	s.joinMu.Unlock()
	s.log.Info("заявка на вступление по ссылке", slog.String("group", safeShort8(tok.GroupID)), slog.String("owner", safeShort(ownerID)))
	s.trySendJoin(tok.GroupID)
	return nil
}

// OnJoinPeerReady — владелец появился в сети или открылся DataChannel с ним:
// отправить ожидающие заявки. Зовётся из main.go по TopicPeerOnline/TopicP2PReady.
func (s *GroupsService) OnJoinPeerReady(peerID string) {
	s.joinMu.Lock()
	s.ensureJoinsLoadedLocked()
	var gids []string
	for gid, j := range s.joins {
		if j.OwnerID == peerID {
			gids = append(gids, gid)
		}
	}
	s.joinMu.Unlock()
	for _, gid := range gids {
		s.trySendJoin(gid)
	}
}

func (s *GroupsService) trySendJoin(groupID string) {
	now := time.Now()
	s.joinMu.Lock()
	j := s.joins[groupID]
	if j == nil {
		s.joinMu.Unlock()
		return
	}
	if now.Unix() > j.Expires {
		s.dropJoinLocked(groupID)
		s.joinMu.Unlock()
		s.emitJoinResult(groupID, "expired")
		return
	}
	connected := s.network.IsP2PConnected(j.OwnerID)
	if connected && !j.sentAt.IsZero() && now.Sub(j.sentAt) > joinAnswerTimeout {
		s.dropJoinLocked(groupID)
		s.joinMu.Unlock()
		s.emitJoinResult(groupID, "no_answer")
		return
	}
	if connected {
		if j.sentAt.IsZero() {
			j.sentAt = now
		}
	} else {
		// Таймаут ответа считаем только на живом соединении.
		j.sentAt = time.Time{}
	}
	interval := joinRetryOffline
	if connected {
		interval = joinRetryConnected
	}
	if j.timer != nil {
		j.timer.Stop()
	}
	j.timer = time.AfterFunc(interval, func() { s.trySendJoin(groupID) })
	owner, link := j.OwnerID, j.Link
	s.joinMu.Unlock()

	if connected {
		if data := s.marshalP2P(P2PMessage{Type: "group_join_request", GroupID: groupID, Content: link, Time: now.Unix()}); data != nil {
			if err := s.network.SendP2PData(owner, data); err != nil {
				s.log.Debug("group_join_request не отправлен", slog.String("error", err.Error()))
			}
		}
		return
	}
	// Соединение для вступления — CallOnly: CloseCallOnlyPeers рвёт его на
	// завершении любого звонка, поэтому заявку повторяет таймер выше.
	s.network.ConnectToCallPeer(owner)
}

func (s *GroupsService) pendingJoinFor(groupID string) *pendingJoin {
	s.joinMu.Lock()
	defer s.joinMu.Unlock()
	s.ensureJoinsLoadedLocked()
	return s.joins[groupID]
}

// completeJoin — пришло приглашение от владельца: заявка выполнена.
func (s *GroupsService) completeJoin(groupID string) {
	s.joinMu.Lock()
	s.dropJoinLocked(groupID)
	s.joinMu.Unlock()
	s.emitJoinResult(groupID, "ok")
}

func (s *GroupsService) dropJoinLocked(groupID string) {
	if j := s.joins[groupID]; j != nil && j.timer != nil {
		j.timer.Stop()
	}
	delete(s.joins, groupID)
	s.saveJoinsLocked()
}

func (s *GroupsService) emitJoinResult(groupID, code string) {
	s.network.emitEvent("vox:group-join-result", map[string]string{"groupId": groupID, "code": code})
}

// ensureJoinsLoadedLocked — заявки из прошлой сессии. Сигналка не сообщает о
// тех, кто уже был в сети до нашего входа, поэтому по каждой загруженной
// заявке сразу пробуем отправить.
func (s *GroupsService) ensureJoinsLoadedLocked() {
	if s.joinsLoaded {
		return
	}
	s.joinsLoaded = true
	var raw string
	s.store.DB().QueryRow("SELECT value FROM settings WHERE key = ?", pendingJoinsKey).Scan(&raw)
	if raw == "" {
		return
	}
	var list []*pendingJoin
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return
	}
	now := time.Now().Unix()
	for _, j := range list {
		if j != nil && j.Expires > now && j.GroupID != "" {
			s.joins[j.GroupID] = j
			gid := j.GroupID
			time.AfterFunc(0, func() { s.trySendJoin(gid) })
		}
	}
}

func (s *GroupsService) saveJoinsLocked() {
	list := make([]*pendingJoin, 0, len(s.joins))
	for _, j := range s.joins {
		list = append(list, j)
	}
	data, _ := json.Marshal(list)
	if _, err := s.store.DB().Exec("INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)", pendingJoinsKey, string(data)); err != nil {
		s.log.Warn("pending_group_joins не сохранены", slog.String("error", err.Error()))
	}
}

// handleJoinResult — ответ владельца. «ok» ничего не делает: приглашение
// приходит раньше ответа тем же упорядоченным каналом и закрывает заявку.
func (s *GroupsService) handleJoinResult(from string, msg *P2PMessage) {
	s.joinMu.Lock()
	j := s.joins[msg.GroupID]
	if j == nil || j.OwnerID != from || msg.Content == "ok" {
		s.joinMu.Unlock()
		return
	}
	s.dropJoinLocked(msg.GroupID)
	s.joinMu.Unlock()
	s.log.Info("вступление по ссылке отклонено", slog.String("group", safeShort8(msg.GroupID)), slog.String("code", msg.Content))
	s.emitJoinResult(msg.GroupID, msg.Content)
}

// --- Владелец: приём заявки ---

func (s *GroupsService) handleJoinRequest(from string, msg *P2PMessage) {
	reply := func(code string) {
		if data := s.marshalP2P(P2PMessage{Type: "group_join_result", GroupID: msg.GroupID, Content: code, Time: time.Now().Unix()}); data != nil {
			_ = s.network.SendP2PData(from, data)
		}
		s.log.Info("заявка на вступление по ссылке",
			slog.String("group", safeShort8(msg.GroupID)), slog.String("peer", safeShort(from)), slog.String("result", code))
	}
	tok, err := parseInviteLink(msg.Content, time.Now())
	if errors.Is(err, errInviteExpired) {
		reply("expired")
		return
	}
	if err != nil || tok.GroupID != msg.GroupID {
		reply("revoked")
		return
	}
	myID, err := s.sess.PeerID()
	if err != nil || hex.EncodeToString(tok.OwnerPub) != myID {
		reply("revoked")
		return
	}
	var ownerID, nonceHex string
	if err := s.store.DB().QueryRow("SELECT owner_id, invite_nonce FROM groups WHERE group_id = ?", tok.GroupID).
		Scan(&ownerID, &nonceHex); err != nil || ownerID != myID || s.joinMemberStatus(tok.GroupID, myID) != "active" {
		reply("gone")
		return
	}
	if nonceHex == "" || nonceHex != hex.EncodeToString(tok.Nonce[:]) {
		reply("revoked")
		return
	}
	var banned int
	s.store.DB().QueryRow("SELECT COUNT(*) FROM group_bans WHERE group_id = ? AND peer_id = ?", tok.GroupID, from).Scan(&banned)
	if banned > 0 {
		reply("banned")
		return
	}
	// Уже состоящему тоже шлём приглашение заново: заявка повторяется, пока
	// приглашение не дошло.
	if err := s.addMember(tok.GroupID, from, true); err != nil {
		if strings.Contains(err.Error(), "заполнена") {
			reply("full")
		} else {
			reply("gone")
		}
		return
	}
	reply("ok")
}
