package services

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"
	"vox_app/internal/session"
	"vox_app/internal/storage"

	"github.com/google/uuid"
)

// pendingGroupMsg — group_message, пришедший раньше чем мы узнали отправителя
// (через group_invite или member_added). Хранится в памяти с TTL и replay'ится
// когда членство становится известным.
type pendingGroupMsg struct {
	fromPubKey string
	msg        P2PMessage
	receivedAt time.Time
}

const (
	pendingGroupMsgTTL       = 60 * time.Second
	pendingGroupMsgMaxPerGrp = 50
)

// GroupsService — управление группами и групповыми сообщениями через P2P
type GroupsService struct {
	log     *slog.Logger
	sess    *session.Session
	store   *storage.Storage
	network *NetworkService

	// Буфер out-of-order group_message. Ключ — groupID.
	// См. bufferPendingGroupMessage / flushPendingGroupMessages.
	pendingMu  sync.Mutex
	pendingMsg map[string][]pendingGroupMsg

	// Server relay settings — форс и пороговое значение участников.
	// См. groups_relay.go. Загружаются из settings через LoadRelaySettings.
	relayMu        sync.RWMutex
	forceRelay     bool
	relayThreshold int

	// Повтор неподтверждённых server-relay сообщений (groups_relay_retry.go):
	// таймер на сообщение, ключ — client_msg_id.
	relayRetryMu sync.Mutex
	relayRetry   map[string]*time.Timer

	// Колбэк для очистки attachments при удалении/деактивации group_message.
	// Wired в main.go через SetUnlinkAttachments после создания обоих сервисов.
	// Подпись совместима с MessagingService.UnlinkAttachmentsForMessage.
	onUnlinkAttachments func(msgID, msgKind string)

	// Колбэк для подгрузки attachments в GetGroupMessages (msgKind="group").
	// Wired в main.go: groupsSvc.SetLoadAttachments(messagingSvc.LoadAttachmentsForMessage).
	onLoadAttachments func(msgID, msgKind string) []AttachmentMeta

	// Колбэк для перезапуска chunked-доставки attachment-сообщения конкретному
	// peer'у (используется в DeliverPendingGroupMessages при reconnect).
	// Wired в main.go: groupsSvc.SetRestartAttachmentDelivery(messagingSvc.sendAttachmentsToPeer).
	restartAttachmentDelivery func(peerID, msgID, caption string)

	// Колбэк повторной отправки FILE_MANIFEST для групповых вложений нового
	// формата (message_files). Wired в main.go: filesSvc.RetryUploadGroup.
	filesRetryUploadGroup func(peerID, msgID string)

	// Вступление по ссылке-приглашению (group_invite_link.go): заявки, ждущие
	// ответа владельца, ключ — groupID. Дублируются в settings
	// pending_group_joins, чтобы пережить перезапуск.
	joinMu      sync.Mutex
	joins       map[string]*pendingJoin
	joinsLoaded bool
}

// SetUnlinkAttachments инжектит хук очистки blob'ов на удалении group_message.
// Вызывается из main.go: groupsSvc.SetUnlinkAttachments(messagingSvc.UnlinkAttachmentsForMessage).
func (s *GroupsService) SetUnlinkAttachments(fn func(msgID, msgKind string)) {
	s.onUnlinkAttachments = fn
}

// SetLoadAttachments инжектит хук загрузки []AttachmentMeta для GroupChatMessage.
func (s *GroupsService) SetLoadAttachments(fn func(msgID, msgKind string) []AttachmentMeta) {
	s.onLoadAttachments = fn
}

// SetRestartAttachmentDelivery инжектит хук перезапуска chunked-передачи attachment'а
// конкретному peer'у. Используется в DeliverPendingGroupMessages при reconnect:
// для msg.type="attachment" нельзя просто отправить JSON — нужен полноценный
// sendAttachmentsToPeer (offer + chunks + complete).
func (s *GroupsService) SetRestartAttachmentDelivery(fn func(peerID, msgID, caption string)) {
	s.restartAttachmentDelivery = fn
}

// SetFilesRetryUploadGroup инжектит FilesService.RetryUploadGroup: при реконнекте
// участника групповые вложения из message_files уходят повторным манифестом,
// а не legacy-путём, который их не видит.
func (s *GroupsService) SetFilesRetryUploadGroup(fn func(peerID, msgID string)) {
	s.filesRetryUploadGroup = fn
}

func NewGroupsService(log *slog.Logger, sess *session.Session, store *storage.Storage, network *NetworkService) *GroupsService {
	return &GroupsService{
		log:            log,
		sess:           sess,
		store:          store,
		network:        network,
		pendingMsg:     make(map[string][]pendingGroupMsg),
		relayThreshold: defaultRelayThreshold,
		joins:          make(map[string]*pendingJoin),
	}
}

// --- Типы ---

type Group struct {
	GroupID       string `json:"groupId"`
	Name          string `json:"name"`
	CreatorID     string `json:"creatorId"`
	Members       int    `json:"members"`
	Unread        int    `json:"unread"`
	Type          int    `json:"type"`          // 0 = текстовая группа, 1 = голосовой канал
	Left          bool   `json:"left"`          // true = покинута пользователем
	VoiceEnabled  bool   `json:"voiceEnabled"`  // true = голосовой канал включён
	LastMessageAt int64  `json:"lastMessageAt"` // timestamp последнего сообщения (ms), 0 если нет
	// LastMessage — превью в списке групп (last_message.go): автор, текст,
	// голосовое, вложения. nil — сообщений нет.
	LastMessage *LastMessage `json:"lastMessage,omitempty"`
	CreatedAt   int64        `json:"createdAt"` // когда группа появилась у нас (ms) — «создана 8 сен» в панели
}

type GroupMember struct {
	PeerID   string `json:"peerId"`
	Nickname string `json:"nickname"`
	Role     string `json:"role"`
	Online   bool   `json:"online"`
}

type GroupChatMessage struct {
	ID            string           `json:"id"`
	GroupID       string           `json:"groupId"`
	SenderID      string           `json:"senderId"`
	SenderName    string           `json:"senderName"`
	Content       string           `json:"content"`
	Type          string           `json:"type"`
	MsgType       string           `json:"msgType"` // "text", "voice" или "attachment"
	Outbound      bool             `json:"outbound"`
	Delivered     bool             `json:"delivered"`
	Edited        bool             `json:"edited"` // редактировалось — «изм.» в пузыре
	Timestamp     int64            `json:"timestamp"`
	Duration      int              `json:"duration,omitempty"`
	Waveform      string           `json:"waveform,omitempty"`
	Attachments   []AttachmentMeta `json:"attachments,omitempty"`
	ReplyTo       *ReplyRef        `json:"replyTo,omitempty"`
	ForwardedFrom *ForwardRef      `json:"forwardedFrom,omitempty"`
	// SendFailed — код окончательного отказа сервера для своего сообщения
	// (message_too_large, not_group_member, group_not_found); пусто — не было.
	SendFailed string `json:"sendFailed,omitempty"`
}

// GroupInvitePayload — данные приглашения в группу (передаётся в P2PMessage.Content как JSON)
type GroupInvitePayload struct {
	GroupID   string   `json:"groupId"`
	Name      string   `json:"name"`
	CreatorID string   `json:"creatorId"`
	Members   []string `json:"members"`
	Type      int      `json:"type"`
	// Хеш фото группы: новичок сравнит со своим и попросит group_avatar_get.
	// Старые клиенты лишнее поле игнорируют.
	AvatarHash string `json:"avatarHash,omitempty"`
}

// --- Публичные методы (вызываются из фронтенда) ---

// GetGroups возвращает список групп текущего пользователя
func (s *GroupsService) GetGroups() ([]Group, error) {
	const op = "GroupsService.GetGroups"

	myID, err := s.sess.PeerID()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	rows, err := s.store.DB().Query(`
		SELECT g.group_id, g.name, g.owner_id, g.type,
		       (SELECT COUNT(*) FROM group_members WHERE group_id = g.group_id AND status = 'active'),
		       CASE WHEN gm.status = 'active' THEN
		           (SELECT COUNT(*) FROM group_messages
		            WHERE group_id = g.group_id AND deleted = 0
		              AND sender_id != ? AND created_at > gm.last_read_at)
		       ELSE 0 END,
		       gm.status,
		       g.voice_enabled,
		       g.created_at
		FROM groups g
		JOIN group_members gm ON gm.group_id = g.group_id AND gm.peer_id = ?
		WHERE gm.status IN ('active', 'left')
		ORDER BY g.name`, myID, myID)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer rows.Close()

	var groups []Group
	for rows.Next() {
		var g Group
		var status string
		var voiceEnabled int
		var createdAt int64
		if err := rows.Scan(&g.GroupID, &g.Name, &g.CreatorID, &g.Type, &g.Members, &g.Unread, &status, &voiceEnabled, &createdAt); err != nil {
			continue
		}
		g.Left = status == "left"
		g.VoiceEnabled = voiceEnabled != 0
		g.CreatedAt = createdAt * 1000

		// Последнее сообщение: превью в списке и timestamp для сортировки
		g.LastMessage = lastGroupMessage(s.store.DB(), g.GroupID, myID)
		if g.LastMessage != nil {
			g.LastMessageAt = g.LastMessage.Timestamp
		}

		groups = append(groups, g)
	}

	if groups == nil {
		groups = []Group{}
	}

	// Логируем сводку по группам
	totalUnread := 0
	for _, g := range groups {
		totalUnread += g.Unread
		if g.Unread > 0 {
			s.log.Debug("📋 ГРУППА",
				slog.String("group_id", safeShort8(g.GroupID)),
				slog.Int("unread", g.Unread),
				slog.Int("members", g.Members))
		}
	}
	if totalUnread > 0 {
		s.log.Info("📋 ВСЕГО НЕПРОЧИТАННЫХ GROUP",
			slog.Int("total_unread", totalUnread),
			slog.Int("groups", len(groups)))
	}

	return groups, rows.Err()
}

// CreateGroup создаёт группу и приглашает участников
const maxGroupMembers = 100

func (s *GroupsService) CreateGroup(name string, memberIDs []string, groupType int) error {
	const op = "GroupsService.CreateGroup"

	if name == "" {
		return fmt.Errorf("%s: пустое имя группы", op)
	}
	if len(name) > maxNicknameLen {
		return fmt.Errorf("%s: имя группы слишком длинное (макс %d)", op, maxNicknameLen)
	}
	if len(memberIDs) > maxGroupMembers {
		return fmt.Errorf("%s: слишком много участников (макс %d)", op, maxGroupMembers)
	}

	myID, err := s.sess.PeerID()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}

	groupID := uuid.New().String()
	now := time.Now().Unix()

	// Создаём группу и участников в транзакции
	tx, err := s.store.DB().Begin()
	if err != nil {
		return fmt.Errorf("%s: begin tx: %w", op, err)
	}
	defer tx.Rollback() // no-op после успешного Commit

	if _, err := tx.Exec(
		`INSERT INTO groups (group_id, name, owner_id, type, created_at) VALUES (?, ?, ?, ?, ?)`,
		groupID, name, myID, groupType, now); err != nil {
		return fmt.Errorf("%s: insert group: %w", op, err)
	}

	if _, err := tx.Exec(
		`INSERT INTO group_members (group_id, peer_id, role, joined_at) VALUES (?, ?, 'admin', ?)`,
		groupID, myID, now); err != nil {
		return fmt.Errorf("%s: insert self: %w", op, err)
	}

	// Добавляем выбранных участников
	allMembers := []string{myID}
	for _, peerID := range memberIDs {
		if peerID == myID || peerID == "" {
			continue
		}
		_, err = tx.Exec(
			`INSERT OR IGNORE INTO group_members (group_id, peer_id, role, joined_at) VALUES (?, ?, 'member', ?)`,
			groupID, peerID, now)
		if err != nil {
			s.log.Warn("не удалось добавить участника",
				slog.String("peer", safeShort(peerID)),
				slog.String("error", err.Error()))
			continue
		}
		allMembers = append(allMembers, peerID)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: commit: %w", op, err)
	}

	// Отправляем приглашения через P2P (после успешного commit)
	invite := GroupInvitePayload{
		GroupID:   groupID,
		Name:      name,
		CreatorID: myID,
		Members:   allMembers,
		Type:      groupType,
	}
	inviteJSON, err := json.Marshal(invite)
	if err != nil {
		s.log.Error("не удалось сериализовать invite", slog.String("error", err.Error()))
		return nil // группа уже создана, приглашения отправятся при reconnect
	}

	for _, peerID := range memberIDs {
		if peerID == myID || peerID == "" {
			continue
		}
		sent := false
		if s.network.IsP2PConnected(peerID) {
			p2pMsg := P2PMessage{
				Type:    "group_invite",
				ID:      groupID,
				GroupID: groupID,
				Content: string(inviteJSON),
				Time:    now,
			}
			data := s.marshalP2P(p2pMsg)
			if data != nil {
				if err := s.network.SendP2PData(peerID, data); err != nil {
					s.log.Warn("не удалось отправить приглашение",
						slog.String("peer", safeShort(peerID)),
						slog.String("error", err.Error()))
				} else {
					sent = true
				}
			}
		}
		if !sent {
			s.persistGroupAction(groupID, groupID, peerID, "group_invite", string(inviteJSON), now)
			s.log.Debug("пир офлайн, приглашение сохранено в очередь",
				slog.String("peer", safeShort(peerID)))
		}
	}

	s.log.Info("группа создана",
		slog.String("group_id", safeShort8(groupID)),
		slog.Int("members", len(allMembers)))
	s.network.RefreshPresence() // участники новой группы видят мой статус «в сети»

	// Если force или размер ≥ threshold — регистрируем на сервере сразу.
	// Регистрация идёт в background: ошибку не пробрасываем наружу (UI уже
	// получил успешный ответ, группа создана локально).
	if s.useServerRelay(groupID) {
		go func() {
			if err := s.RegisterGroupOnServer(groupID); err != nil {
				s.log.Warn("авто-регистрация группы на сервере не удалась",
					slog.String("group_id", safeShort8(groupID)),
					slog.String("error", err.Error()))
			}
		}()
	}

	return nil
}

// CreateVoiceChannel создаёт голосовой канал (группа с type=1)
func (s *GroupsService) CreateVoiceChannel(name string, memberIDs []string) error {
	return s.CreateGroup(name, memberIDs, 1)
}

// SetVoiceEnabled включает/выключает голосовой канал в группе (только admin)
func (s *GroupsService) SetVoiceEnabled(groupID string, enabled bool) error {
	const op = "GroupsService.SetVoiceEnabled"

	myID, err := s.sess.PeerID()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}

	var role string
	s.store.DB().QueryRow(
		"SELECT role FROM group_members WHERE group_id = ? AND peer_id = ?",
		groupID, myID).Scan(&role)
	if role != "admin" {
		return fmt.Errorf("%s: нет прав администратора", op)
	}

	val := 0
	if enabled {
		val = 1
	}
	s.store.DB().Exec("UPDATE groups SET voice_enabled = ? WHERE group_id = ?", val, groupID)

	// Broadcast to members
	members := s.GetGroupMemberIDs(groupID)
	payload := "0"
	if enabled {
		payload = "1"
	}
	s.broadcastGroupActionWithPayload("group_voice_toggle", groupID, myID, members, payload)

	s.network.emitEvent("vox:group-voice-toggle", map[string]any{
		"groupId": groupID,
		"enabled": enabled,
	})

	s.log.Info("голосовой канал переключён",
		slog.String("group", groupID),
		slog.Bool("enabled", enabled))
	return nil
}

// RenameGroup — переименование группы (только создатель)
func (s *GroupsService) RenameGroup(groupID, name string) error {
	const op = "GroupsService.RenameGroup"

	if name == "" {
		return fmt.Errorf("%s: пустое имя", op)
	}
	if len(name) > maxNicknameLen {
		return fmt.Errorf("%s: имя слишком длинное (макс %d)", op, maxNicknameLen)
	}

	myID, err := s.sess.PeerID()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}

	var ownerID string
	s.store.DB().QueryRow(
		"SELECT owner_id FROM groups WHERE group_id = ?",
		groupID).Scan(&ownerID)
	if ownerID != myID {
		return fmt.Errorf("%s: только создатель может переименовать", op)
	}

	s.store.DB().Exec("UPDATE groups SET name = ? WHERE group_id = ?", name, groupID)

	// Уведомляем участников (с очередью для офлайн)
	members := s.GetGroupMemberIDs(groupID)
	s.broadcastGroupActionWithPayload("group_rename", groupID, myID, members, name)

	s.log.Info("группа переименована",
		slog.String("group_id", safeShort8(groupID)))
	return nil
}

// --- helpers ---

// broadcastGroupAction — отправляет действие онлайн пирам, сохраняет в очередь для офлайн
func (s *GroupsService) broadcastGroupAction(actionType, groupID, myID string, members []string) {
	s.broadcastGroupActionWithPayload(actionType, groupID, myID, members, "")
}

// broadcastGroupActionWithPayload — broadcastGroupAction с произвольным payload (для invite, rename и т.д.)
func (s *GroupsService) broadcastGroupActionWithPayload(actionType, groupID, myID string, members []string, payload string) {
	msgID := uuid.New().String()
	now := time.Now().Unix()

	p2pMsg := P2PMessage{
		Type:    actionType,
		ID:      msgID,
		GroupID: groupID,
		Content: payload,
		Time:    now,
	}
	data := s.marshalP2P(p2pMsg)

	for _, peerID := range members {
		if peerID == myID {
			continue
		}
		sent := false
		if s.network.IsP2PConnected(peerID) && data != nil {
			if err := s.network.SendP2PData(peerID, data); err == nil {
				sent = true
			}
		}
		if !sent {
			s.persistGroupAction(msgID, groupID, peerID, actionType, payload, now)
		}
	}
}

// maxNicknameLen — максимальная длина никнейма / названия группы
const maxNicknameLen = 100

// validatePeerID проверяет формат Ed25519 public key (hex, 64 символа)
func validatePeerID(peerID string) error {
	if len(peerID) != 64 {
		return fmt.Errorf("некорректный peerID: длина %d, ожидается 64", len(peerID))
	}
	for _, c := range peerID {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return fmt.Errorf("некорректный peerID: недопустимый символ '%c'", c)
		}
	}
	return nil
}

// safeShort возвращает первые 16 символов строки (безопасно для коротких строк)
func safeShort(s string) string {
	if len(s) > 16 {
		return s[:16]
	}
	return s
}

// safeShort8 возвращает первые 8 символов строки (безопасно для коротких строк)
func safeShort8(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// marshalP2P сериализует P2P сообщение в JSON. Логирует ошибку и возвращает nil при сбое.
func (s *GroupsService) marshalP2P(msg P2PMessage) []byte {
	data, err := json.Marshal(msg)
	if err != nil {
		s.log.Warn("ошибка сериализации P2P сообщения", slog.String("error", err.Error()))
		return nil
	}
	return data
}

// peerNickname удалён — используется единый s.network.PeerNickname.
