package services

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"vox_app/internal/logsafe"
)

// SendGroupMessage отправляет сообщение в группу.
// Доставка считается успешной если сообщение ушло хотя бы одному пиру (Delivered=true).
// Это осознанное решение: в группе на 10 человек галочка появится когда 1 из 9 получил.
// Часики означают что НИКТО не получил (все офлайн).
func (s *GroupsService) SendGroupMessage(groupID, text string) (*SendResult, error) {
	return s.SendGroupMessageWithOptions(groupID, text, SendOptions{})
}

// SendGroupMessageWithOptions — SendGroupMessage с цитатой и/или «переслано от».
func (s *GroupsService) SendGroupMessageWithOptions(groupID, text string, opts SendOptions) (*SendResult, error) {
	const op = "GroupsService.SendGroupMessage"
	opts = opts.normalize()

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

	// Проверяем что мы активный участник группы
	var memberStatus string
	err = s.store.DB().QueryRow(
		"SELECT status FROM group_members WHERE group_id = ? AND peer_id = ?",
		groupID, myID).Scan(&memberStatus)
	if err != nil || memberStatus != "active" {
		return nil, fmt.Errorf("%s: вы не являетесь участником группы", op)
	}

	// Если группа помечена на server relay (или сработал force/threshold) —
	// отправляем через signal сервер. P2P-путь (ниже) — fallback.
	if s.useServerRelay(groupID) {
		return s.sendViaRelay(groupID, text, opts)
	}

	msgID := uuid.New().String()
	now := time.Now().Unix()

	// Получаем участников один раз, до транзакции.
	members := s.GetGroupMemberIDs(groupID)

	// Сохраняем сообщение и записи доставки атомарно: либо видно и сообщение,
	// и pending-delivery для всех участников, либо ни того ни другого.
	// P2P-отправка делается уже ПОСЛЕ Commit, чтобы сетевые вызовы не держали
	// блокировку БД.
	tx, err := s.store.DB().Begin()
	if err != nil {
		return nil, fmt.Errorf("%s: begin tx: %w", op, err)
	}
	defer tx.Rollback() // no-op после успешного Commit

	if _, err := tx.Exec(
		`INSERT INTO group_messages (id, group_id, sender_id, content, type, created_at, reply_to, forwarded_from)
		 VALUES (?, ?, ?, ?, 'text', ?, ?, ?)`,
		msgID, groupID, myID, text, now, encodeRef(opts.ReplyTo), encodeRef(opts.ForwardedFrom)); err != nil {
		return nil, fmt.Errorf("%s: insert msg: %w", op, err)
	}

	for _, peerID := range members {
		if peerID == myID {
			continue
		}
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO group_message_delivery (msg_id, peer_id, delivered) VALUES (?, ?, 0)`,
			msgID, peerID); err != nil {
			return nil, fmt.Errorf("%s: insert delivery: %w", op, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("%s: commit: %w", op, err)
	}

	// Вне транзакции: пробуем P2P-доставку и помечаем delivered=1 по мере успеха.
	p2pMsg := P2PMessage{
		Type:          "group_message",
		ID:            msgID,
		GroupID:       groupID,
		Content:       text,
		Time:          now,
		ReplyTo:       opts.ReplyTo,
		ForwardedFrom: opts.ForwardedFrom,
	}
	data := s.marshalP2P(p2pMsg)

	sent := 0
	for _, peerID := range members {
		if peerID == myID || data == nil {
			continue
		}
		if err := s.network.SendP2PData(peerID, data); err != nil {
			s.log.Warn("не удалось отправить group msg",
				slog.String("peer", safeShort(peerID)),
				slog.String("error", err.Error()))
			// Попытка установить P2P для будущей доставки (pending delivery)
			go s.network.ConnectToGroupPeer(peerID)
			continue
		}
		sent++
		if _, err := s.store.DB().Exec(
			"UPDATE group_message_delivery SET delivered = 1 WHERE msg_id = ? AND peer_id = ?",
			msgID, peerID); err != nil {
			s.log.Warn("не удалось обновить статус доставки",
				slog.String("msg_id", msgID),
				slog.String("error", err.Error()))
		}
	}

	// Получаем имя группы для логов
	otherMembers := len(members) - 1
	delivered := sent > 0 || otherMembers == 0 // ✓ если доставлено хотя бы одному ИЛИ я один в группе
	statusIcon := "⏳"
	if delivered {
		statusIcon = "✓"
	}
	s.log.Info("📤 GROUP ОТПРАВЛЕНО",
		slog.String("group_id", safeShort8(groupID)),
		slog.String("msg_id", msgID),
		logsafe.Len("len", text),
		slog.String("status", statusIcon),
		slog.Int("delivered_to", sent),
		slog.Int("total_members", otherMembers))

	return &SendResult{ID: msgID, Delivered: delivered}, nil
}

// GetGroupMessages возвращает историю сообщений группы (последние 50)
func (s *GroupsService) GetGroupMessages(groupID string) ([]GroupChatMessage, error) {
	return s.getGroupMessagesPage(groupID, 50, 0)
}

// GetGroupMessagesPage — история группы постранично (подгрузка старых при
// прокрутке вверх и прыжок к найденному сообщению).
func (s *GroupsService) GetGroupMessagesPage(groupID string, limit, offset int) ([]GroupChatMessage, error) {
	return s.getGroupMessagesPage(groupID, limit, offset)
}

// MarkGroupRead отмечает все сообщения группы как прочитанные
func (s *GroupsService) MarkGroupRead(groupID string) error {
	myID, err := s.sess.PeerID()
	if err != nil {
		return err
	}

	// Считаем непрочитанные до обновления
	var unreadBefore int
	var lastReadAt int64
	s.store.DB().QueryRow(
		"SELECT last_read_at FROM group_members WHERE group_id = ? AND peer_id = ?",
		groupID, myID).Scan(&lastReadAt)
	s.store.DB().QueryRow(
		"SELECT COUNT(*) FROM group_messages WHERE group_id = ? AND deleted = 0 AND sender_id != ? AND created_at > ?",
		groupID, myID, lastReadAt).Scan(&unreadBefore)

	now := time.Now().Unix()
	_, err = s.store.DB().Exec(
		"UPDATE group_members SET last_read_at = ? WHERE group_id = ? AND peer_id = ?",
		now, groupID, myID)

	if unreadBefore > 0 {
		s.log.Info("✅ GROUP ОТМЕЧЕНО ПРОЧИТАННЫМ",
			slog.String("group_id", safeShort8(groupID)),
			slog.Int("marked_read", unreadBefore))
	}

	return err
}

func (s *GroupsService) getGroupMessagesPage(groupID string, limit, offset int) ([]GroupChatMessage, error) {
	const op = "GroupsService.GetGroupMessages"

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
		`SELECT id, group_id, sender_id, content, type, COALESCE(edited, 0), created_at, duration, waveform, seq, reply_to, forwarded_from, send_failed
		 FROM group_messages
		 WHERE group_id = ? AND deleted = 0
		 ORDER BY created_at DESC LIMIT ? OFFSET ?`,
		groupID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer rows.Close()

	// Предзагружаем myNick и контакты один раз (вместо N+1 запросов внутри цикла)
	myNick := s.network.MyNickname()
	if myNick == "" {
		myNick = safeShort(myID)
	}

	// Кеш никнеймов контактов
	nickCache := make(map[string]string)
	contactRows, err := s.store.DB().Query("SELECT peer_id, nickname FROM contacts")
	if err != nil {
		s.log.Warn("не удалось загрузить ники контактов", slog.String("error", err.Error()))
	} else {
		defer contactRows.Close()
		for contactRows.Next() {
			var pid, nick string
			if err := contactRows.Scan(&pid, &nick); err != nil {
				continue
			}
			nickCache[pid] = nick
		}
	}

	var messages []GroupChatMessage
	// Параллельный массив seq для маркера server-relay доставки (не экспортируется).
	var seqs []int64
	for rows.Next() {
		var m GroupChatMessage
		var createdAt, seq int64
		var replyTo, fwdFrom string
		if err := rows.Scan(&m.ID, &m.GroupID, &m.SenderID, &m.Content, &m.Type, &m.Edited, &createdAt, &m.Duration, &m.Waveform, &seq, &replyTo, &fwdFrom, &m.SendFailed); err != nil {
			continue
		}
		m.ReplyTo = decodeReply(replyTo)
		m.ForwardedFrom = decodeForward(fwdFrom)
		m.Timestamp = createdAt * 1000
		m.Outbound = m.SenderID == myID
		m.MsgType = m.Type

		if m.Outbound {
			m.SenderName = myNick
			// Server relay: если есть серверный seq — сервер принял и будет фан-аутить,
			// считаем такие сообщения доставленными (аналог P2P "доставлено ≥ 1 пиру").
			if seq > 0 {
				m.Delivered = true
			}
		} else {
			m.Delivered = true
			if nick, ok := nickCache[m.SenderID]; ok && nick != "" {
				m.SenderName = nick
			} else {
				// Не друг: ник из peer_profiles; кешируем на время цикла.
				m.SenderName = s.network.PeerNickname(m.SenderID)
				nickCache[m.SenderID] = m.SenderName
			}
		}

		messages = append(messages, m)
		seqs = append(seqs, seq)
	}
	_ = seqs // зарезервировано на будущее (метки типа "server", "p2p")

	// Пакетная проверка delivery для исходящих сообщений
	if len(messages) > 0 {
		deliveredSet := make(map[string]bool)
		delRows, err := s.store.DB().Query(
			`SELECT DISTINCT msg_id FROM group_message_delivery
			 WHERE msg_id IN (SELECT id FROM group_messages WHERE group_id = ? AND sender_id = ?)
			 AND delivered = 1`, groupID, myID)
		if err != nil {
			s.log.Warn("не удалось загрузить статусы доставки", slog.String("error", err.Error()))
		} else {
			defer delRows.Close()
			for delRows.Next() {
				var msgID string
				if err := delRows.Scan(&msgID); err != nil {
					continue
				}
				deliveredSet[msgID] = true
			}
		}
		for i := range messages {
			if messages[i].Outbound {
				// Сохраняем уже выставленный flag (seq>0 = server relay delivered);
				// иначе берём из delivery таблицы (P2P путь).
				if !messages[i].Delivered {
					messages[i].Delivered = deliveredSet[messages[i].ID]
				}
			}
		}
	}

	// Разворачиваем — фронтенд ожидает от старых к новым
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}

	// Подгружаем attachments через колбэк (избегаем circular dep на MessagingService)
	if s.onLoadAttachments != nil {
		for i := range messages {
			if messages[i].Type == "attachment" {
				messages[i].Attachments = s.onLoadAttachments(messages[i].ID, "group")
			}
		}
	}

	if messages == nil {
		messages = []GroupChatMessage{}
	}
	return messages, rows.Err()
}
