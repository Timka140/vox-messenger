package services

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"vox_app/internal/logsafe"
	"vox_app/internal/session"
	"vox_app/internal/storage"

	"github.com/google/uuid"
)

// voiceAssembly собирает chunks голосового сообщения
type voiceAssembly struct {
	fromPubKey     string
	metadata       P2PMessage
	chunks         []string // base64 chunks в порядке
	total          int
	received       int
	assembledBytes int64 // суммарный размер принятого base64 (cap против OOM)
	createdAt      time.Time
}

// MessagingService — отправка/получение сообщений через P2P DataChannel
type MessagingService struct {
	log     *slog.Logger
	sess    *session.Session
	store   *storage.Storage
	network *NetworkService
	groups  *GroupsService
	voice   *VoiceService
	mesh    *MeshtasticService
	blobs   *storage.BlobStore // attachment blob store; инжектится через SetBlobStore
	bridge  *AssetBridge       // /vox-attach + /vox-local; инжектится через SetAssetBridge
	avatars *AvatarService     // фото профиля и групп (avatars.go); SetAvatarService

	// casCleanup — хук в FilesService.UnlinkFilesForMessage. Инжектится из
	// main.go после создания обоих сервисов; nil-safe в legacy-путях.
	casCleanup func(msgID, msgKind string)

	// filesRetryUploadDM — хук в FilesService.RetryUploadDM. Используется в
	// DeliverPendingMessages чтобы пере-отправить FILE_MANIFEST для DM-сообщений
	// типа 'attachment', созданных через CAS-сервис (см. message_files-table).
	// Без этого DeliverPendingMessages вслепую отправляет всё в legacy путь
	// sendAttachmentsToPeer, который смотрит только в message_attachments.
	filesRetryUploadDM func(peerID, msgID string)

	// Сборка голосовых сообщений из chunks
	voiceAssemblyMu sync.Mutex
	voiceAssembly   map[string]*voiceAssembly // msgId → assembly

	// Attachment chunked transfer state (см. attachments_p2p.go).
	// sendStates: ключ "<peerID>|<msgID>" → активная отправка.
	// recvAssemblies: ключ "<fromPeer>|<msgID>" → собирающийся приём.
	// peerSendMu: per-peer мьютекс для serialize'а нескольких одновременных
	//   transfers'ов в одного пира — без него они делят bandwidth и каждый
	//   уходит вдвое-втрое медленнее.
	attachMu       sync.Mutex
	sendStates     map[string]*attachSendState
	recvAssemblies map[string]*attachRecvAssembly
	peerSendMu     map[string]*sync.Mutex
}

// SendMessageViaMesh marshals a "message"-type P2PMessage and sends it
// encrypted over LoRa to the friend's mapped Meshtastic node. Caller must
// have set the friend's mesh node ID first; the call returns an explicit
// error otherwise. The persisted DB record is identical to a P2P-sent one,
// so chat history shows messages from both transports in one thread.
func (s *MessagingService) SendMessageViaMesh(peerID, text string) (SendResult, error) {
	if s.mesh == nil {
		return SendResult{}, fmt.Errorf("mesh transport not initialized")
	}
	id := uuid.New().String()
	now := time.Now().Unix()
	myID, err := s.sess.PeerID()
	if err != nil {
		return SendResult{}, fmt.Errorf("identity: %w", err)
	}

	pkt := P2PMessage{Type: "message", ID: id, Content: text, Time: now}
	body, err := json.Marshal(pkt)
	if err != nil {
		return SendResult{}, fmt.Errorf("marshal: %w", err)
	}
	if err := s.mesh.SendEncryptedDM(peerID, body); err != nil {
		return SendResult{}, err
	}
	// Persist as outgoing immediately — sender shows the message right away.
	// `delivered = 0` because mesh has no ACK in M6.2; user-facing UI can
	// show a "via mesh" badge to clarify it's best-effort.
	if _, err := s.store.DB().Exec(
		`INSERT OR IGNORE INTO messages (id, peer_id, sender_id, content, type, read, delivered, created_at)
		 VALUES (?, ?, ?, ?, 'text', 1, 0, ?)`,
		id, peerID, myID, text, now); err != nil {
		s.log.Warn("save outgoing mesh DM", slog.String("error", err.Error()))
	}
	s.log.Info("📡 DM via mesh sent",
		slog.String("peer", safeShort(peerID)),
		slog.String("id", id),
		slog.Int("bytes", len(text)))
	return SendResult{ID: id, Delivered: false}, nil
}

// NewMessagingService конструирует сервис DM + routing входящих P2P.
// voice/groups/mesh передаются сразу: voice — для воспроизведения входящих
// голосовых чанков; groups — для роутинга group_* типов через HandleGroupP2P;
// mesh — альтернативный LoRa-транспорт + приём decrypted DM из mesh-адаптера.
func NewMessagingService(log *slog.Logger, sess *session.Session, store *storage.Storage, network *NetworkService, voice *VoiceService, groups *GroupsService, mesh *MeshtasticService) *MessagingService {
	svc := &MessagingService{
		log:           log,
		sess:          sess,
		store:         store,
		network:       network,
		voice:         voice,
		groups:        groups,
		mesh:          mesh,
		voiceAssembly: make(map[string]*voiceAssembly),
	}

	// Регистрируем обработчики
	network.SetOnP2PMessage(svc.handleIncomingP2P)
	network.SetOnPeerConnected(svc.DeliverPendingMessages)
	network.SetOnAttachMessage(svc.HandleIncomingAttachBinary)
	if mesh != nil {
		mesh.SetOnDecryptedMessage(svc.handleIncomingMeshP2P)
	}

	return svc
}

// --- Типы ---

type Conversation struct {
	ID          string       `json:"id"`
	ShortID     string       `json:"shortId"`
	Nickname    string       `json:"nickname"`
	Online      bool         `json:"online"`
	Unread      int          `json:"unread"`
	LastMessage *LastMessage `json:"lastMessage,omitempty"`
}

type LastMessage struct {
	Content   string `json:"content"`
	Timestamp int64  `json:"timestamp"`
	Outbound  bool   `json:"outbound"`
	MsgType   string `json:"msgType"` // text | voice | attachment | call — превью в списке чатов
	// Превью по макету (last_message.go): длительность голосового, вложения,
	// автор в группах.
	Duration       int    `json:"duration,omitempty"`       // мс, голосовое
	Attachments    int    `json:"attachments,omitempty"`    // число вложений
	AttachmentKind string `json:"attachmentKind,omitempty"` // photo | file — первое вложение
	AttachmentName string `json:"attachmentName,omitempty"` // имя первого вложения
	SenderName     string `json:"senderName,omitempty"`     // группы: ник автора
}

type ChatMessage struct {
	ID            string           `json:"id"`
	PeerID        string           `json:"peerId"`
	SenderID      string           `json:"senderId"`
	Content       string           `json:"content"`
	Type          string           `json:"type"`
	MsgType       string           `json:"msgType"` // "text", "voice" или "attachment" (для frontend)
	Read          bool             `json:"read"`
	Delivered     bool             `json:"delivered"`
	Outbound      bool             `json:"outbound"`
	Edited        bool             `json:"edited"` // редактировалось — «изм.» в пузыре
	Timestamp     int64            `json:"timestamp"`
	Duration      int              `json:"duration,omitempty"`    // мс (для voice)
	Waveform      string           `json:"waveform,omitempty"`    // "25,50,80,..." (для voice)
	Attachments   []AttachmentMeta `json:"attachments,omitempty"` // для msgType="attachment"
	ReplyTo       *ReplyRef        `json:"replyTo,omitempty"`
	ForwardedFrom *ForwardRef      `json:"forwardedFrom,omitempty"`
}

type SearchResult struct {
	Found    bool   `json:"found"`
	PeerID   string `json:"peerId"`
	Nickname string `json:"nickname"`
	ShortID  string `json:"shortId"`
}

// P2PMessage — формат сообщения через DataChannel
type P2PMessage struct {
	Type     string `json:"type"` // "message", "voice_message", "read", "typing", "group_*", "attachment_*"
	ID       string `json:"id"`
	GroupID  string `json:"groupId,omitempty"`
	Content  string `json:"content"`
	Time     int64  `json:"time"`
	Audio    string `json:"audio,omitempty"`    // base64 WAV (для voice messages)
	Duration int    `json:"duration,omitempty"` // мс
	Waveform string `json:"waveform,omitempty"` // "25,50,80,..."
	Nickname string `json:"nickname,omitempty"` // ник отправителя (для voice_join/voice_members — чтобы не-друзья могли видеть ник)
	Data     string `json:"data,omitempty"`     // base64 JPEG для avatar_data (фото профиля)

	// Attachment-related fields (см. attachments_p2p.go).
	// Att      — индекс файла в массиве attachment_offer.atts (0-based).
	// Idx      — индекс чанка для attachment_chunk (0-based).
	// Total    — для chunk: общее число чанков; для offer payload: см. AttachmentOfferPayload.
	// Sha      — sha256 hex для attachment_complete (verify целостности).
	// Off      — resume-offset в байтах (attachment_offer_ack).
	Att   int    `json:"att,omitempty"`
	Idx   int    `json:"idx,omitempty"`
	Total int    `json:"total,omitempty"`
	Sha   string `json:"sha,omitempty"`
	Off   int64  `json:"off,omitempty"`

	// Ответ с цитатой и «переслано от» (message / group_message).
	ReplyTo       *ReplyRef   `json:"replyTo,omitempty"`
	ForwardedFrom *ForwardRef `json:"forwardedFrom,omitempty"`
}

// AttachmentOfferPayload — JSON в Content поле attachment_offer.
// Sender анонсирует пакет attachments перед чанками. Receiver проверяет
// каждый sha256 в attachments-таблице — если уже есть, на этот файл чанки
// слать не нужно (dedup), и возвращает Have=true в OfferAckEntry.
type AttachmentOfferPayload struct {
	MsgID   string           `json:"msgId"`
	Caption string           `json:"caption"`
	Atts    []AttachmentMeta `json:"atts"`  // включает Sha256, Mime, Size, Width/Height и (через kind/name) ord+name
	Names   []string         `json:"names"` // оригинальные имена per-slot
	Kinds   []string         `json:"kinds"` // "photo"/"file" per-slot
}

// AttachmentOfferAckPayload — Content поле attachment_offer_ack.
// Receiver возвращает per-attachment offset (0 = качай с начала; -1 = уже есть, dedup).
type AttachmentOfferAckPayload struct {
	MsgID   string  `json:"msgId"`
	Offsets []int64 `json:"offsets"` // длина = len(offer.atts); -1 для dedup, иначе resume-offset
}

type SendResult struct {
	ID        string `json:"id"`
	Delivered bool   `json:"delivered"`
	Waveform  string `json:"waveform,omitempty"`
}

// --- Публичные методы ---

// SendMessage отправляет сообщение через P2P DataChannel
func (s *MessagingService) SendMessage(peerID, text string) (*SendResult, error) {
	return s.SendMessageWithOptions(peerID, text, SendOptions{})
}

// SendMessageWithOptions — SendMessage с цитатой (ответ) и/или «переслано от».
func (s *MessagingService) SendMessageWithOptions(peerID, text string, opts SendOptions) (*SendResult, error) {
	const op = "MessagingService.SendMessage"
	opts = opts.normalize()

	if err := validatePeerID(peerID); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if text == "" {
		return nil, fmt.Errorf("%s: пустое сообщение", op)
	}
	if len(text) > 1_000_000 {
		return nil, fmt.Errorf("%s: сообщение слишком большое (макс 1 МБ)", op)
	}

	myID, err := s.sess.PeerID()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	msgID := uuid.New().String()
	now := time.Now().Unix()


	// Сначала сохраняем как недоставленное (save-first)
	_, err = s.store.DB().Exec(
		`INSERT INTO messages (id, peer_id, sender_id, content, type, read, delivered, created_at, reply_to, forwarded_from)
		 VALUES (?, ?, ?, ?, 'text', 1, 0, ?, ?, ?)`,
		msgID, peerID, myID, text, now, encodeRef(opts.ReplyTo), encodeRef(opts.ForwardedFrom))
	if err != nil {
		return nil, fmt.Errorf("%s: db: %w", op, err)
	}

	// Пробуем отправить через P2P
	delivered := 0
	p2pMsg := P2PMessage{
		Type:          "message",
		ID:            msgID,
		Content:       text,
		Time:          now,
		ReplyTo:       opts.ReplyTo,
		ForwardedFrom: opts.ForwardedFrom,
	}
	data := s.marshalP2P(p2pMsg)
	if data != nil {
		if err := s.network.SendP2PData(peerID, data); err != nil {
			s.log.Warn("не удалось отправить P2P",
				slog.String("peer", safeShort(peerID)),
				slog.String("error", err.Error()))
		} else {
			delivered = 1
			if _, err := s.store.DB().Exec("UPDATE messages SET delivered = 1 WHERE id = ?", msgID); err != nil {
				s.log.Error("не удалось обновить delivered", slog.String("msg_id", msgID), slog.String("error", err.Error()))
			}
		}
	}

	statusIcon := "⏳"
	if delivered == 1 {
		statusIcon = "✓"
	}
	s.log.Info("📤 DM ОТПРАВЛЕНО",
		slog.String("peer", safeShort(peerID)),
		slog.String("msg_id", msgID),
		logsafe.Len("len", text),
		slog.String("status", statusIcon),
		slog.Bool("peer_online", s.network.IsP2PConnected(peerID)),
		slog.Bool("delivered", delivered == 1))

	return &SendResult{ID: msgID, Delivered: delivered == 1}, nil
}

// GetMessages возвращает историю сообщений с пиром
func (s *MessagingService) GetMessages(peerID string) ([]ChatMessage, error) {
	return s.GetMessagesPage(peerID, 50, 0)
}

// GetMessagesPage возвращает историю с пагинацией
func (s *MessagingService) GetMessagesPage(peerID string, limit, offset int) ([]ChatMessage, error) {
	const op = "MessagingService.GetMessages"

	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	myID, err := s.sess.PeerID()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	rows, err := s.store.DB().Query(
		`SELECT id, peer_id, sender_id, content, type, read, delivered, COALESCE(edited, 0), created_at, duration, waveform, reply_to, forwarded_from
		 FROM messages WHERE peer_id = ? AND deleted = 0
		 ORDER BY created_at DESC LIMIT ? OFFSET ?`,
		peerID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer rows.Close()

	var messages []ChatMessage
	for rows.Next() {
		var m ChatMessage
		var createdAt int64
		var replyTo, fwdFrom string
		if err := rows.Scan(&m.ID, &m.PeerID, &m.SenderID, &m.Content, &m.Type, &m.Read, &m.Delivered, &m.Edited, &createdAt, &m.Duration, &m.Waveform, &replyTo, &fwdFrom); err != nil {
			return nil, fmt.Errorf("%s: scan: %w", op, err)
		}
		m.ReplyTo = decodeReply(replyTo)
		m.ForwardedFrom = decodeForward(fwdFrom)
		m.Timestamp = createdAt * 1000
		m.Outbound = m.SenderID == myID
		m.MsgType = m.Type
		messages = append(messages, m)
	}

	// Разворачиваем — фронтенд ожидает от старых к новым
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}

	// Подгружаем attachments для сообщений типа 'attachment' (отдельный запрос
	// на каждое такое сообщение — обычно их немного на страницу).
	for i := range messages {
		if messages[i].Type == "attachment" {
			messages[i].Attachments = s.LoadAttachmentsForMessage(messages[i].ID, "dm")
		}
	}

	if messages == nil {
		messages = []ChatMessage{}
	}
	return messages, rows.Err()
}

// SearchHit — совпадение поиска по чату. Offset — позиция сообщения от
// новейшего в том же порядке, что у GetMessagesPage / GetGroupMessagesPage
// (created_at DESC, deleted=0): фронт догружает страницы до неё и прыгает.
type SearchHit struct {
	ID        string `json:"id"`
	SenderID  string `json:"senderId"`
	Timestamp int64  `json:"timestamp"`
	Offset    int    `json:"offset"`
}

// searchHitLimit — больше совпадений в одном чате не показываем.
const searchHitLimit = 500

// SearchMessages — поиск по тексту сообщений одного чата (DM peerID или
// группа groupID), новые первыми. fromID — фильтр по автору ("" — все).
// Сравнение в Go через strings.ToLower: SQLite LIKE/lower не знают кириллицу.
// Ищем в тексте и подписях вложений (content), имена файлов не смотрим.
func (s *MessagingService) SearchMessages(peerID, groupID, query, fromID string) ([]SearchHit, error) {
	const op = "MessagingService.SearchMessages"
	out := []SearchHit{}
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return out, nil
	}
	var rows *sql.Rows
	var err error
	if groupID != "" {
		rows, err = s.store.DB().Query(
			"SELECT id, sender_id, content, type, created_at FROM group_messages WHERE group_id = ? AND deleted = 0 ORDER BY created_at DESC", groupID)
	} else if peerID != "" {
		rows, err = s.store.DB().Query(
			"SELECT id, sender_id, content, type, created_at FROM messages WHERE peer_id = ? AND deleted = 0 ORDER BY created_at DESC", peerID)
	} else {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer rows.Close()
	idx := 0
	for rows.Next() {
		var id, sender, content, typ string
		var createdAt int64
		if err := rows.Scan(&id, &sender, &content, &typ, &createdAt); err != nil {
			return nil, fmt.Errorf("%s: scan: %w", op, err)
		}
		offset := idx
		idx++
		if typ != "text" && typ != "attachment" {
			continue
		}
		if fromID != "" && sender != fromID {
			continue
		}
		if !strings.Contains(strings.ToLower(content), q) {
			continue
		}
		out = append(out, SearchHit{ID: id, SenderID: sender, Timestamp: createdAt * 1000, Offset: offset})
		if len(out) >= searchHitLimit {
			break
		}
	}
	return out, rows.Err()
}

// GetConversations возвращает список бесед (друзья с последним сообщением)
func (s *MessagingService) GetConversations() ([]Conversation, error) {
	const op = "MessagingService.GetConversations"

	myID, err := s.sess.PeerID()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	// Берём друзей
	rows, err := s.store.DB().Query(
		"SELECT peer_id, nickname FROM contacts WHERE status = 'accepted' ORDER BY nickname, peer_id")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer rows.Close()

	var convs []Conversation
	for rows.Next() {
		var peerID, nickname string
		if err := rows.Scan(&peerID, &nickname); err != nil {
			continue
		}

		shortID := peerID
		if len(shortID) > 16 {
			shortID = shortID[:16]
		}

		conv := Conversation{
			ID:       peerID,
			ShortID:  shortID,
			Nickname: nickname,
			Online:   s.network.IsP2PConnected(peerID),
		}

		// Считаем непрочитанные
		if err := s.store.DB().QueryRow(
			"SELECT COUNT(*) FROM messages WHERE peer_id = ? AND sender_id != ? AND read = 0 AND deleted = 0",
			peerID, myID).Scan(&conv.Unread); err != nil {
			s.log.Warn("не удалось подсчитать непрочитанные",
				slog.String("peer", shortID), slog.String("error", err.Error()))
		}

		// Последнее сообщение: если есть непрочитанные — последнее входящее,
		// иначе последнее любое (last_message.go).
		if conv.Unread > 0 {
			conv.LastMessage = lastDMMessage(s.store.DB(), peerID, myID, true)
		}
		if conv.LastMessage == nil {
			conv.LastMessage = lastDMMessage(s.store.DB(), peerID, myID, false)
		}

		convs = append(convs, conv)
	}

	if convs == nil {
		convs = []Conversation{}
	}

	// Логируем сводку по беседам
	totalUnread := 0
	for _, c := range convs {
		totalUnread += c.Unread
		if c.Unread > 0 || c.Online {
			s.log.Debug("📋 БЕСЕДА",
				slog.String("peer", safeShort(c.ID)),
				slog.Bool("online", c.Online),
				slog.Int("unread", c.Unread))
		}
	}
	if totalUnread > 0 {
		s.log.Info("📋 ВСЕГО НЕПРОЧИТАННЫХ DM",
			slog.Int("total_unread", totalUnread),
			slog.Int("conversations", len(convs)))
	}

	return convs, rows.Err()
}

// MarkMessagesRead отмечает сообщения как прочитанные
func (s *MessagingService) MarkMessagesRead(peerID string) error {
	const op = "MessagingService.MarkMessagesRead"

	myID, err := s.sess.PeerID()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}

	// Считаем сколько отметим прочитанными
	var unreadBefore int
	if err := s.store.DB().QueryRow(
		"SELECT COUNT(*) FROM messages WHERE peer_id = ? AND sender_id != ? AND read = 0 AND deleted = 0",
		peerID, myID).Scan(&unreadBefore); err != nil {
		return fmt.Errorf("%s: count unread: %w", op, err)
	}

	if _, err := s.store.DB().Exec(
		"UPDATE messages SET read = 1 WHERE peer_id = ? AND sender_id != ? AND read = 0",
		peerID, myID); err != nil {
		return fmt.Errorf("%s: update read: %w", op, err)
	}

	notifiedPeer := false

	// Уведомляем пира о прочтении через P2P
	if s.network.IsP2PConnected(peerID) {
		data := s.marshalP2P(P2PMessage{Type: "read"})
		if data != nil {
			s.network.SendP2PData(peerID, data)
			notifiedPeer = true
		}
	}

	if unreadBefore > 0 {
		s.log.Info("✅ DM ОТМЕЧЕНО ПРОЧИТАННЫМ",
			slog.String("peer", safeShort(peerID)),
			slog.Int("marked_read", unreadBefore),
			slog.Bool("notified_peer", notifiedPeer))
	}

	s.network.emitEvent("vox:messages-read", map[string]string{"peerId": peerID})
	return nil
}

// CancelMessage отменяет отправку сообщения.
// Если не доставлено — удаляет локально. Если доставлено — удаляет у обоих сторон.
func (s *MessagingService) CancelMessage(msgID string) error {
	const op = "MessagingService.CancelMessage"

	// Hard-delete: освобождаем attachments ДО DELETE messages,
	// иначе message_attachments → orphan'ы (FK нет, но логически).
	s.UnlinkAttachmentsForMessage(msgID, "dm")
	s.UnlinkAttachmentsForMessage(msgID, "group")

	// Пробуем удалить недоставленное
	res, err := s.store.DB().Exec(
		"DELETE FROM messages WHERE id = ? AND delivered = 0", msgID)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}

	affected, _ := res.RowsAffected()
	if affected > 0 {
		s.log.Info("отправка отменена (не доставлено)", slog.String("id", msgID))
		s.network.emitEvent("vox:message-deleted", map[string]string{"id": msgID})
		return nil
	}

	// Пробуем удалить из group_messages
	res, err = s.store.DB().Exec("DELETE FROM group_messages WHERE id = ?", msgID)
	if err == nil {
		if ga, _ := res.RowsAffected(); ga > 0 {
			s.log.Info("групповое сообщение удалено", slog.String("id", msgID))
			s.network.emitEvent("vox:message-deleted", map[string]string{"id": msgID})
			return nil
		}
	}

	// Уже доставлено — удаляем у обоих сторон
	var peerID string
	if err := s.store.DB().QueryRow("SELECT peer_id FROM messages WHERE id = ?", msgID).Scan(&peerID); err != nil {
		return fmt.Errorf("%s: сообщение не найдено: %w", op, err)
	}

	if _, err := s.store.DB().Exec("UPDATE messages SET deleted = 1 WHERE id = ?", msgID); err != nil {
		return fmt.Errorf("%s: ошибка удаления: %w", op, err)
	}
	s.network.emitEvent("vox:message-deleted", map[string]string{"id": msgID})

	// Уведомляем пира об удалении
	if peerID != "" && s.network.IsP2PConnected(peerID) {
		p2pMsg := P2PMessage{Type: "message_delete", ID: msgID}
		if data, err := json.Marshal(p2pMsg); err == nil {
			s.network.SendP2PData(peerID, data)
		}
	}

	s.log.Info("сообщение отменено (удалено у обоих)", slog.String("id", msgID))
	return nil
}

// DeliverPendingMessages отправляет все недоставленные сообщения пиру (вызывается при P2P reconnect)
func (s *MessagingService) DeliverPendingMessages(peerID string) {
	myID, err := s.sess.PeerID()
	if err != nil {
		return
	}

	rows, err := s.store.DB().Query(
		`SELECT id, content, type, audio_data, duration, waveform, created_at, reply_to, forwarded_from FROM messages
		 WHERE peer_id = ? AND sender_id = ? AND delivered = 0 AND deleted = 0
		 ORDER BY created_at ASC`,
		peerID, myID)
	if err != nil {
		return
	}
	defer rows.Close()

	if !s.network.IsP2PConnected(peerID) {
		return
	}

	// Собираем pending сообщения в срез (нельзя Exec внутри rows.Next)
	type pendingMsg struct {
		ID        string
		Content   string
		MsgType   string
		AudioData []byte
		Duration  int
		Waveform  string
		Time      int64
		ReplyTo   string
		FwdFrom   string
	}
	var pending []pendingMsg
	for rows.Next() {
		var m pendingMsg
		if err := rows.Scan(&m.ID, &m.Content, &m.MsgType, &m.AudioData, &m.Duration, &m.Waveform, &m.Time, &m.ReplyTo, &m.FwdFrom); err != nil {
			continue
		}
		pending = append(pending, m)
	}
	rows.Close()

	if len(pending) > 0 {
		s.log.Info("📬 ДОСТАВКА PENDING DM",
			slog.String("peer", safeShort(peerID)),
			slog.Int("pending_count", len(pending)))
	}

	delivered := 0
	for _, m := range pending {
		var ok bool
		if m.MsgType == "voice" && len(m.AudioData) > 0 {
			ok = s.deliverPendingVoice(peerID, m.ID, m.AudioData, m.Duration, m.Waveform, m.Time)
		} else if m.MsgType == "attachment" {
			// Сначала пытаемся через новый CAS-сервис (message_files-table).
			// Если у msg есть строки в message_files — это CAS-сообщение, его
			// надо доставлять через FilesService.RetryUploadDM (FILE_MANIFEST
			// поверх attachments DC). Старый sendAttachmentsToPeer в этом случае
			// вернёт "attach send: load failed err:null" и сообщение зависнет.
			//
			// Если message_files пустой — это legacy attachment (через
			// message_attachments + base64 chunks по messages DC), используем
			// старый путь.
			var hasCAS int
			_ = s.store.DB().QueryRow(
				`SELECT 1 FROM message_files WHERE message_id = ? LIMIT 1`,
				m.ID).Scan(&hasCAS)
			if hasCAS == 1 && s.filesRetryUploadDM != nil {
				go s.filesRetryUploadDM(peerID, m.ID)
				s.log.Debug("  → pending ATTACH (CAS) перезапущен",
					slog.String("msg_id", m.ID))
				continue
			}
			// Re-launch chunked sender в фоне; UPDATE delivered=1 произойдёт
			// внутри sendAttachmentsToPeer после complete. Здесь нет ok=true,
			// иначе мы пометим delivered=1 преждевременно.
			go s.sendAttachmentsToPeer(peerID, m.ID, m.Content)
			s.log.Debug("  → pending ATTACH (legacy) перезапущен",
				slog.String("msg_id", m.ID))
			continue
		} else {
			p2pMsg := P2PMessage{
				Type:          "message",
				ID:            m.ID,
				Content:       m.Content,
				Time:          m.Time,
				ReplyTo:       decodeReply(m.ReplyTo),
				ForwardedFrom: decodeForward(m.FwdFrom),
			}
			data := s.marshalP2P(p2pMsg)
			if data == nil {
				continue
			}
			if err := s.network.SendP2PData(peerID, data); err != nil {
				s.log.Warn("не удалось доставить pending",
					slog.String("peer", safeShort(peerID)),
					slog.String("id", m.ID),
					slog.String("error", err.Error()))
				continue
			}
			ok = true
		}

		if !ok {
			continue
		}

		if _, err := s.store.DB().Exec("UPDATE messages SET delivered = 1 WHERE id = ?", m.ID); err != nil {
			s.log.Error("не удалось обновить delivered pending", slog.String("msg_id", m.ID), slog.String("error", err.Error()))
		}
		delivered++
		s.log.Debug("  → pending DM доставлено",
			slog.String("msg_id", m.ID),
			slog.String("type", m.MsgType),
			logsafe.Len("len", m.Content))
	}

	if delivered > 0 {
		s.log.Info("📬 PENDING DM ДОСТАВЛЕНО",
			slog.String("peer", safeShort(peerID)),
			slog.Int("delivered", delivered),
			slog.Int("total_pending", len(pending)))

		// Уведомляем фронтенд что сообщения доставлены
		s.network.emitEvent("vox:messages-delivered", map[string]string{"peerId": peerID})
	}
}

// deliverPendingVoice отправляет офлайн voice сообщение через chunked протокол
func (s *MessagingService) deliverPendingVoice(peerID, msgID string, audioData []byte, duration int, waveform string, createdAt int64) bool {
	audioB64 := base64.StdEncoding.EncodeToString(audioData)

	// 1. metadata
	meta := P2PMessage{
		Type:     "voice_message",
		ID:       msgID,
		Duration: duration,
		Waveform: waveform,
		Time:     createdAt,
	}
	data := s.marshalP2P(meta)
	if data == nil {
		return false
	}
	if err := s.network.SendP2PData(peerID, data); err != nil {
		return false
	}

	// 2. chunks
	chunkSize := 64 * 1024
	totalChunks := (len(audioB64) + chunkSize - 1) / chunkSize
	for i := 0; i < totalChunks; i++ {
		start := i * chunkSize
		end := start + chunkSize
		if end > len(audioB64) {
			end = len(audioB64)
		}
		chunk := P2PMessage{
			Type:    "voice_chunk",
			ID:      msgID,
			Content: audioB64[start:end],
			Audio:   fmt.Sprintf("%d/%d", i, totalChunks),
		}
		chunkData := s.marshalP2P(chunk)
		if chunkData == nil {
			return false
		}
		if err := s.network.SendP2PDataThrottled(peerID, chunkData); err != nil {
			return false
		}
	}

	// 3. complete
	complete := P2PMessage{
		Type: "voice_complete",
		ID:   msgID,
	}
	completeData := s.marshalP2P(complete)
	if completeData != nil {
		s.network.SendP2PData(peerID, completeData)
	}

	s.log.Info("📬 pending voice DM доставлено",
		slog.String("peer", safeShort(peerID)),
		slog.String("msg_id", msgID),
		slog.Int("chunks", totalChunks))

	return true
}

func (s *MessagingService) AddContact(uri string) error {
	return nil
}

// SearchUser ищет пира по nick#tag через сигнальный сервер
func (s *MessagingService) SearchUser(tag string) (*SearchResult, error) {
	const op = "MessagingService.SearchUser"

	client := s.network.Client()
	if client == nil || !client.IsConnected() {
		return nil, fmt.Errorf("%s: не подключён к сигнальному серверу", op)
	}

	peers, err := client.SearchPeersSync(tag, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	s.log.Debug("результат поиска",
		slog.String("query", tag),
		slog.Int("count", len(peers)))

	// Извлекаем shortID из запроса (nick#shortID → shortID)
	queryID := tag
	if idx := strings.Index(tag, "#"); idx >= 0 {
		queryID = tag[idx+1:]
	}

	for _, p := range peers {
		peerShort := p.PublicKey
		if len(peerShort) > 16 {
			peerShort = peerShort[:16]
		}
		if strings.EqualFold(p.NickTag, tag) || strings.EqualFold(peerShort, queryID) {
			nick := p.NickTag
			if idx := strings.Index(nick, "#"); idx > 0 {
				nick = nick[:idx]
			}
			return &SearchResult{
				Found:    true,
				PeerID:   p.PublicKey,
				Nickname: nick,
				ShortID:  peerShort,
			}, nil
		}
	}

	return nil, fmt.Errorf("not_found")
}

// --- Обработка входящих P2P сообщений ---

func (s *MessagingService) handleIncomingP2P(fromPubKey string, data []byte) {
	s.dispatchP2P(fromPubKey, data, false)
}

// handleIncomingMeshP2P — сообщения, расшифрованные из LoRa-mesh.
func (s *MessagingService) handleIncomingMeshP2P(fromPubKey string, data []byte) {
	s.dispatchP2P(fromPubKey, data, true)
}

func (s *MessagingService) dispatchP2P(fromPubKey string, data []byte, viaMesh bool) {
	if len(data) > 2_000_000 {
		s.log.Warn("P2P сообщение слишком большое, отброшено", slog.Int("size", len(data)))
		return
	}
	var msg P2PMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		s.log.Warn("невалидное P2P сообщение", slog.String("error", err.Error()))
		return
	}
	// Заблокированный (blocklist.go): личное отбрасываем, состав групп и
	// голоса пропускаем.
	if dropFromBlocked(msg.Type) && s.network.IsBlocked(fromPubKey) {
		s.log.Debug("P2P от заблокированного отброшено", slog.String("type", msg.Type), slog.String("from", safeShort(fromPubKey)))
		return
	}

	switch msg.Type {
	case "message":
		s.handleIncomingMessage(fromPubKey, &msg)
	case "typing":
		s.handleTyping(fromPubKey, &msg)
	case "read":
		s.log.Info("📖 DM ПРОЧИТАНО собеседником",
			slog.String("peer", safeShort(fromPubKey)))
		s.network.emitEvent("vox:messages-read", map[string]string{"peerId": fromPubKey})
	case "message_edit":
		if _, err := s.store.DB().Exec("UPDATE messages SET content = ?, edited = 1 WHERE id = ?", msg.Content, msg.ID); err != nil {
			s.log.Error("ошибка редактирования DM", slog.String("msg_id", msg.ID), slog.String("error", err.Error()))
		}
		s.log.Info("✏️ DM ОТРЕДАКТИРОВАНО", slog.String("peer", safeShort(fromPubKey)), slog.String("msg_id", msg.ID))
		s.network.emitEvent("vox:message-edited", map[string]string{"id": msg.ID, "content": msg.Content})
	case "message_delete":
		if _, err := s.store.DB().Exec("UPDATE messages SET deleted = 1 WHERE id = ?", msg.ID); err != nil {
			s.log.Error("ошибка удаления DM", slog.String("msg_id", msg.ID), slog.String("error", err.Error()))
		}
		s.UnlinkAttachmentsForMessage(msg.ID, "dm")
		s.log.Info("🗑️ DM УДАЛЕНО", slog.String("peer", safeShort(fromPubKey)), slog.String("msg_id", msg.ID))
		s.network.emitEvent("vox:message-deleted", map[string]string{"id": msg.ID})
	case "caps":
		// Возможности клиента (звук демонстрации, фаза 5e); из LoRa не бывает.
		if !viaMesh {
			s.network.setPeerCaps(fromPubKey, msg.Content)
		}
	case "avatar_hash", "avatar_get", "avatar_data", "group_avatar", "group_avatar_get":
		// Фото ходят только по прямому P2P: через LoRa 55 КБ не пролезут,
		// и отвечать фото на запрос из эфира незачем.
		if !viaMesh && s.avatars != nil {
			s.avatars.handleP2P(fromPubKey, &msg)
		}
	case "nickname_update":
		if _, err := s.store.DB().Exec("UPDATE contacts SET nickname = ? WHERE peer_id = ?", msg.Content, fromPubKey); err != nil {
			s.log.Error("ошибка обновления ника", slog.String("peer", safeShort(fromPubKey)), slog.String("error", err.Error()))
		}
		// Также сохраняем в in-memory кеш — для не-друзей чьих записей нет в contacts
		s.network.CachePeerNickname(fromPubKey, msg.Content)
		s.log.Info("📛 НИК ОБНОВЛЁН",
			slog.String("peer", safeShort(fromPubKey)),
			slog.String("new_nick", msg.Content))
		s.network.emitEvent("vox:nickname-updated", map[string]string{
			"peerId":   fromPubKey,
			"nickname": msg.Content,
		})
	case "voice_message", "group_voice_message":
		// Metadata only (chunks follow separately)
		s.handleVoiceMetadata(fromPubKey, &msg)
	case "voice_chunk":
		s.handleVoiceChunk(fromPubKey, &msg)
	case "voice_complete":
		s.handleVoiceComplete(fromPubKey, &msg)
	case "voice_join", "voice_leave", "voice_status", "voice_members":
		s.log.Debug("📨 P2P voice signal",
			slog.String("type", msg.Type),
			slog.String("from", safeShort(fromPubKey)),
			slog.String("groupID", msg.GroupID),
			slog.Bool("voice_svc", s.voice != nil))
		if s.voice != nil {
			s.voice.HandleVoiceP2P(fromPubKey, &msg)
		}
	case "group_invite", "group_message", "group_leave", "group_kick", "group_delete", "group_rename",
		"group_message_edit", "group_message_delete", "group_rejoin", "member_added", "member_removed",
		"group_voice_toggle", "group_join_request", "group_join_result":
		if s.groups != nil {
			s.groups.HandleGroupP2P(fromPubKey, &msg)
		}
	case "attachment_offer":
		s.handleAttachmentOffer(fromPubKey, &msg)
	case "attachment_offer_ack":
		s.handleAttachmentOfferAck(fromPubKey, &msg)
	case "attachment_chunk":
		s.handleAttachmentChunk(fromPubKey, &msg)
	case "attachment_complete":
		s.handleAttachmentComplete(fromPubKey, &msg)
	case "attachment_cancel":
		s.handleAttachmentCancel(fromPubKey, &msg)
	case "attachment_decline":
		s.handleAttachmentDecline(fromPubKey, &msg)
	}
}

func (s *MessagingService) handleIncomingMessage(fromPubKey string, msg *P2PMessage) {
	opts := SendOptions{ReplyTo: msg.ReplyTo, ForwardedFrom: msg.ForwardedFrom}.normalize()
	// Сохраняем в БД
	res, err := s.store.DB().Exec(
		`INSERT OR IGNORE INTO messages (id, peer_id, sender_id, content, type, read, created_at, reply_to, forwarded_from)
		 VALUES (?, ?, ?, ?, 'text', 0, ?, ?, ?)`,
		msg.ID, fromPubKey, fromPubKey, msg.Content, msg.Time, encodeRef(opts.ReplyTo), encodeRef(opts.ForwardedFrom))
	if err != nil {
		s.log.Warn("не удалось сохранить сообщение", slog.String("error", err.Error()))
		return
	}
	// Дубликат — не эмитим событие повторно
	if rows, _ := res.RowsAffected(); rows == 0 {
		return
	}

	// Уведомляем фронтенд
	nickname := ""
	if err := s.store.DB().QueryRow("SELECT nickname FROM contacts WHERE peer_id = ?", fromPubKey).Scan(&nickname); err != nil {
		s.log.Debug("не удалось получить ник контакта", slog.String("peer", safeShort(fromPubKey)))
	}

	// Считаем непрочитанные от этого пира
	myID, _ := s.sess.PeerID()
	var unreadCount int
	if err := s.store.DB().QueryRow(
		"SELECT COUNT(*) FROM messages WHERE peer_id = ? AND sender_id != ? AND read = 0 AND deleted = 0",
		fromPubKey, myID).Scan(&unreadCount); err != nil {
		s.log.Warn("не удалось подсчитать непрочитанные", slog.String("peer", safeShort(fromPubKey)), slog.String("error", err.Error()))
	}

	s.log.Info("📥 DM ПОЛУЧЕНО",
		slog.String("peer", safeShort(fromPubKey)),
		slog.String("msg_id", msg.ID),
		logsafe.Len("len", msg.Content),
		slog.Int("unread_from_peer", unreadCount))

	s.network.emitEvent("vox:message", map[string]any{
		"id":            msg.ID,
		"peerId":        fromPubKey,
		"senderId":      fromPubKey,
		"content":       msg.Content,
		"timestamp":     msg.Time * 1000,
		"nickname":      nickname,
		"replyTo":       opts.ReplyTo,
		"forwardedFrom": opts.ForwardedFrom,
	})
}

// handleVoiceMetadata — первый пакет: metadata без audio (chunks придут отдельно)
func (s *MessagingService) handleVoiceMetadata(fromPubKey string, msg *P2PMessage) {
	s.voiceAssemblyMu.Lock()
	defer s.voiceAssemblyMu.Unlock()

	// Очистка зависших сборок старше 2 минут
	now := time.Now()
	for id, a := range s.voiceAssembly {
		if now.Sub(a.createdAt) > 2*time.Minute {
			delete(s.voiceAssembly, id)
		}
	}

	s.voiceAssembly[msg.ID] = &voiceAssembly{
		fromPubKey: fromPubKey,
		metadata:   *msg,
		chunks:     nil,
		total:      0,
		received:   0,
		createdAt:  now,
	}
	s.log.Debug("voice metadata received",
		slog.String("msg_id", msg.ID),
		slog.Int("duration", msg.Duration))
}

// handleVoiceChunk — фрагмент audio data
func (s *MessagingService) handleVoiceChunk(fromPubKey string, msg *P2PMessage) {
	s.voiceAssemblyMu.Lock()
	defer s.voiceAssemblyMu.Unlock()

	asm := s.voiceAssembly[msg.ID]
	if asm == nil {
		return // metadata не получен
	}

	// voiceMsgMaxBytes — потолок на суммарный base64 голосового сообщения.
	// 1000 chunks × 64 KB давали бы до 64 МБ RAM на одно in-flight сообщение;
	// голосовые короткие, 12 МБ base64 (~9 МБ WAV) с запасом.
	const voiceMsgMaxBytes = 12 * 1024 * 1024

	// Audio field = "index/total"
	var idx, total int
	if n, _ := fmt.Sscanf(msg.Audio, "%d/%d", &idx, &total); n != 2 {
		return // битый заголовок
	}

	if asm.total == 0 {
		if total <= 0 || total > 1000 {
			return // защита от некорректного total
		}
		asm.total = total
		asm.chunks = make([]string, total)
	}

	if idx >= 0 && idx < asm.total {
		// Только первая запись в слот: повторный idx не должен накручивать
		// received (иначе complete сработает не собрав все чанки) и переписывать
		// уже принятые данные.
		if asm.chunks[idx] != "" {
			return
		}
		if asm.assembledBytes+int64(len(msg.Content)) > voiceMsgMaxBytes {
			s.log.Warn("voice chunk: превышен лимит размера, дроп сообщения",
				slog.String("msg_id", msg.ID),
				slog.String("peer", safeShort(fromPubKey)))
			delete(s.voiceAssembly, msg.ID)
			return
		}
		asm.chunks[idx] = msg.Content
		asm.assembledBytes += int64(len(msg.Content))
		asm.received++
	}
}

// handleVoiceComplete — все chunks получены, собираем и сохраняем
func (s *MessagingService) handleVoiceComplete(fromPubKey string, msg *P2PMessage) {
	s.voiceAssemblyMu.Lock()
	asm := s.voiceAssembly[msg.ID]
	if asm == nil {
		s.voiceAssemblyMu.Unlock()
		return
	}
	delete(s.voiceAssembly, msg.ID)
	s.voiceAssemblyMu.Unlock()

	// Собираем base64
	var fullB64 string
	for _, chunk := range asm.chunks {
		fullB64 += chunk
	}

	wavData, err := base64.StdEncoding.DecodeString(fullB64)
	if err != nil {
		s.log.Warn("не удалось собрать voice message", slog.String("error", err.Error()))
		return
	}

	meta := asm.metadata
	isGroup := meta.GroupID != ""

	if isGroup {
		// Group voice message
		if s.groups != nil {
			s.groups.SaveAndEmitGroupVoice(fromPubKey, &meta, wavData)
		}
	} else {
		// DM voice message
		_, err = s.store.DB().Exec(
			`INSERT OR IGNORE INTO messages (id, peer_id, sender_id, content, type, audio_data, duration, waveform, read, created_at)
			 VALUES (?, ?, ?, ?, 'voice', ?, ?, ?, 0, ?)`,
			meta.ID, fromPubKey, fromPubKey, "🎤 Голосовое сообщение", wavData, meta.Duration, meta.Waveform, meta.Time)
		if err != nil {
			s.log.Warn("не удалось сохранить voice message", slog.String("error", err.Error()))
			return
		}

		nickname := ""
		if err := s.store.DB().QueryRow("SELECT nickname FROM contacts WHERE peer_id = ?", fromPubKey).Scan(&nickname); err != nil && err != sql.ErrNoRows {
			s.log.Warn("voice dm: nickname lookup failed", slog.String("error", err.Error()))
		}

		s.log.Info("📥 VOICE DM ПОЛУЧЕНО",
			slog.String("peer", safeShort(fromPubKey)),
			slog.String("msg_id", meta.ID),
			slog.Int("duration", meta.Duration),
			slog.Int("wav_size", len(wavData)),
			slog.Int("chunks", asm.received))

		s.network.emitEvent("vox:message", map[string]any{
			"id":        meta.ID,
			"peerId":    fromPubKey,
			"senderId":  fromPubKey,
			"content":   "🎤 Голосовое сообщение",
			"msgType":   "voice",
			"duration":  meta.Duration,
			"waveform":  meta.Waveform,
			"timestamp": meta.Time * 1000,
			"nickname":  nickname,
		})
	}
}

// --- Удаление / Редактирование сообщений ---

// DeleteMessage удаляет сообщение локально (только у себя).
// Soft-delete + освобождаем blob'ы attachments (в UI они уже не покажутся).
func (s *MessagingService) DeleteMessage(msgID string) error {
	if _, err := s.store.DB().Exec("UPDATE messages SET deleted = 1 WHERE id = ?", msgID); err != nil {
		s.log.Error("ошибка удаления DM", slog.String("msg_id", msgID), slog.String("error", err.Error()))
	}
	if _, err := s.store.DB().Exec("UPDATE group_messages SET deleted = 1 WHERE id = ?", msgID); err != nil {
		s.log.Error("ошибка удаления group msg", slog.String("msg_id", msgID), slog.String("error", err.Error()))
	}
	s.UnlinkAttachmentsForMessage(msgID, "dm")
	s.UnlinkAttachmentsForMessage(msgID, "group")
	s.network.emitEvent("vox:message-deleted", map[string]string{"id": msgID})
	return nil
}

// DeleteMessageForAll удаляет сообщение у себя и отправляет удаление пирам через P2P.
func (s *MessagingService) DeleteMessageForAll(msgID, peerID, groupID string) error {
	myID, _ := s.sess.PeerID()

	if groupID != "" {
		if _, err := s.store.DB().Exec("UPDATE group_messages SET deleted = 1 WHERE id = ?", msgID); err != nil {
			s.log.Error("ошибка удаления group msg", slog.String("msg_id", msgID), slog.String("error", err.Error()))
		}
		s.UnlinkAttachmentsForMessage(msgID, "group")

		p2pMsg := P2PMessage{Type: "group_message_delete", ID: msgID, GroupID: groupID, Time: time.Now().Unix()}
		data := s.marshalP2P(p2pMsg)
		if data != nil && s.groups != nil {
			for _, pid := range s.groups.GetGroupMemberIDs(groupID) {
				if pid == myID {
					continue
				}
				s.network.SendP2PData(pid, data)
			}
		}
	} else if peerID != "" {
		if _, err := s.store.DB().Exec("UPDATE messages SET deleted = 1 WHERE id = ?", msgID); err != nil {
			s.log.Error("ошибка удаления DM", slog.String("msg_id", msgID), slog.String("error", err.Error()))
		}
		s.UnlinkAttachmentsForMessage(msgID, "dm")

		p2pMsg := P2PMessage{Type: "message_delete", ID: msgID, Time: time.Now().Unix()}
		data := s.marshalP2P(p2pMsg)
		if data != nil {
			s.network.SendP2PData(peerID, data)
		}
	}

	s.network.emitEvent("vox:message-deleted", map[string]string{"id": msgID})
	return nil
}

// EditMessage редактирует сообщение локально и отправляет изменение пирам через P2P.
func (s *MessagingService) EditMessage(msgID, peerID, groupID, text string) error {
	if text == "" {
		return fmt.Errorf("пустое сообщение")
	}
	if len(text) > 1_000_000 {
		return fmt.Errorf("сообщение слишком большое (макс 1 МБ)")
	}
	myID, _ := s.sess.PeerID()

	if groupID != "" {
		s.store.DB().Exec("UPDATE group_messages SET content = ?, edited = 1 WHERE id = ? AND sender_id = ?",
			text, msgID, myID)

		p2pMsg := P2PMessage{Type: "group_message_edit", ID: msgID, GroupID: groupID, Content: text, Time: time.Now().Unix()}
		data := s.marshalP2P(p2pMsg)
		if data != nil && s.groups != nil {
			for _, pid := range s.groups.GetGroupMemberIDs(groupID) {
				if pid == myID {
					continue
				}
				s.network.SendP2PData(pid, data)
			}
		}
	} else if peerID != "" {
		s.store.DB().Exec("UPDATE messages SET content = ?, edited = 1 WHERE id = ? AND sender_id = ?",
			text, msgID, myID)

		p2pMsg := P2PMessage{Type: "message_edit", ID: msgID, Content: text, Time: time.Now().Unix()}
		data := s.marshalP2P(p2pMsg)
		if data != nil {
			s.network.SendP2PData(peerID, data)
		}
	}

	s.network.emitEvent("vox:message-edited", map[string]string{"id": msgID, "content": text})
	return nil
}

// marshalP2P сериализует P2P сообщение в JSON. Логирует ошибку и возвращает nil при сбое.
func (s *MessagingService) marshalP2P(msg P2PMessage) []byte {
	data, err := json.Marshal(msg)
	if err != nil {
		s.log.Warn("ошибка сериализации P2P сообщения", slog.String("error", err.Error()))
		return nil
	}
	return data
}

// peerNickname удалён — используется единый s.network.PeerNickname.

// truncate обрезает строку до maxLen символов и добавляет "..." если обрезана
func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}
