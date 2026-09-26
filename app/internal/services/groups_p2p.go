package services

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"vox_app/internal/logsafe"
)

// HandleGroupP2P — диспетчер всех входящих group_* P2P-сообщений.
// Вызывается из MessagingService после парсинга P2PMessage.
func (s *GroupsService) HandleGroupP2P(fromPubKey string, msg *P2PMessage) {
	switch msg.Type {
	case "group_invite":
		s.handleGroupInvite(fromPubKey, msg)
	case "group_message":
		s.handleGroupMessage(fromPubKey, msg)
	// group_voice_message теперь обрабатывается через chunked protocol в messaging.go
	case "group_leave":
		s.handleGroupLeave(fromPubKey, msg)
	case "group_kick":
		s.handleGroupKick(fromPubKey, msg)
	case "group_delete":
		s.handleGroupDelete(fromPubKey, msg)
	case "group_rename":
		s.handleGroupRename(fromPubKey, msg)
	case "group_join_request":
		s.handleJoinRequest(fromPubKey, msg)
	case "group_join_result":
		s.handleJoinResult(fromPubKey, msg)
	case "group_voice_toggle":
		if !s.senderIsGroupAdmin(msg.GroupID, fromPubKey) {
			s.log.Warn("group_voice_toggle от не-админа, игнорируем",
				slog.String("group", msg.GroupID), slog.String("peer", safeShort(fromPubKey)))
			return
		}
		enabled := msg.Content == "1"
		val := 0
		if enabled {
			val = 1
		}
		s.store.DB().Exec("UPDATE groups SET voice_enabled = ? WHERE group_id = ?", val, msg.GroupID)
		s.log.Info("голосовой канал переключён (P2P)",
			slog.String("group", msg.GroupID),
			slog.Bool("enabled", enabled))
		s.network.emitEvent("vox:group-voice-toggle", map[string]any{
			"groupId": msg.GroupID,
			"enabled": enabled,
		})
	case "group_rejoin":
		// Участник вернулся — обновляем статус (если запись существует)
		s.store.DB().Exec(
			"UPDATE group_members SET status = 'active' WHERE group_id = ? AND peer_id = ?",
			msg.GroupID, fromPubKey)
		s.log.Info("участник вернулся в группу",
			slog.String("group", msg.GroupID),
			slog.String("peer", safeShort(fromPubKey)))
		s.network.EmitEvent("vox:group-member-joined", map[string]string{
			"groupId": msg.GroupID,
			"peerId":  fromPubKey,
		})
	case "member_added":
		// Новый участник добавлен в группу — вставляем запись (Content = peerID нового участника).
		// Только админ группы может добавлять участников.
		if !s.senderIsGroupAdmin(msg.GroupID, fromPubKey) {
			s.log.Warn("member_added от не-админа, игнорируем",
				slog.String("group", msg.GroupID), slog.String("peer", safeShort(fromPubKey)))
			return
		}
		newPeerID := msg.Content
		if newPeerID != "" {
			s.store.DB().Exec(
				`INSERT OR IGNORE INTO group_members (group_id, peer_id, role, joined_at, last_read_at)
				 VALUES (?, ?, 'member', ?, ?)`,
				msg.GroupID, newPeerID, msg.Time, msg.Time)
			s.log.Info("новый участник в группе",
				slog.String("group", msg.GroupID),
				slog.String("peer", safeShort(newPeerID)))
			s.network.emitEvent("vox:group-member-added", map[string]string{
				"groupId": msg.GroupID,
				"peerId":  newPeerID,
			})
			// Replay буферизированных group_message ИМЕННО от этого нового участника,
			// если они пришли раньше чем member_added.
			s.flushPendingGroupMessages(msg.GroupID, newPeerID)
			// Новичок уже в сети: peer_online о нём мы проигнорировали, пока
			// общей группы не было, — соединяемся сами.
			if myID, _ := s.sess.PeerID(); newPeerID != myID {
				go s.network.ConnectToGroupPeer(newPeerID)
			}
		}
	case "group_message_edit":
		s.store.DB().Exec("UPDATE group_messages SET content = ?, edited = 1 WHERE id = ?", msg.Content, msg.ID)
		s.log.Info("✏️ GROUP ОТРЕДАКТИРОВАНО",
			slog.String("group_id", msg.GroupID),
			slog.String("msg_id", msg.ID))
		s.network.EmitEvent("vox:message-edited", map[string]string{"id": msg.ID, "content": msg.Content})
	case "group_message_delete":
		s.store.DB().Exec("UPDATE group_messages SET deleted = 1 WHERE id = ?", msg.ID)
		if s.onUnlinkAttachments != nil {
			s.onUnlinkAttachments(msg.ID, "group")
		}
		s.log.Info("🗑️ GROUP УДАЛЕНО",
			slog.String("group_id", msg.GroupID),
			slog.String("msg_id", msg.ID))
		s.network.EmitEvent("vox:message-deleted", map[string]string{"id": msg.ID})
	}
}

func (s *GroupsService) handleGroupInvite(fromPubKey string, msg *P2PMessage) {
	var invite GroupInvitePayload
	if err := json.Unmarshal([]byte(msg.Content), &invite); err != nil {
		s.log.Warn("невалидное приглашение в группу", slog.String("error", err.Error()))
		return
	}

	now := time.Now().Unix()
	myID, _ := s.sess.PeerID()

	// Вступление по ссылке: пока ждём ответа, приглашение в эту группу
	// принимаем только от владельца из ссылки — иначе любой подсунул бы свою
	// группу (состав, владельца) под ID из чужой ссылки.
	byLink := false
	if j := s.pendingJoinFor(invite.GroupID); j != nil {
		if fromPubKey != j.OwnerID || invite.CreatorID != j.OwnerID {
			s.log.Warn("приглашение по ссылке не от владельца, игнорируем",
				slog.String("group", safeShort8(invite.GroupID)), slog.String("from", safeShort(fromPubKey)))
			return
		}
		byLink = true
	}

	// Создаём группу локально (игнорируем если уже есть)
	s.store.DB().Exec(
		`INSERT OR IGNORE INTO groups (group_id, name, owner_id, type, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		invite.GroupID, invite.Name, invite.CreatorID, invite.Type, now)

	// Добавляем всех участников
	for _, peerID := range invite.Members {
		role := "member"
		if peerID == invite.CreatorID {
			role = "admin"
		}
		s.store.DB().Exec(
			`INSERT OR IGNORE INTO group_members (group_id, peer_id, role, joined_at, last_read_at)
			 VALUES (?, ?, ?, ?, ?)`,
			invite.GroupID, peerID, role, now, now)
	}
	if byLink {
		// Вернулись после выхода: своя строка была 'left', название могло смениться.
		s.store.DB().Exec("UPDATE group_members SET status = 'active' WHERE group_id = ? AND peer_id = ?", invite.GroupID, myID)
		s.store.DB().Exec("UPDATE groups SET name = ? WHERE group_id = ?", invite.Name, invite.GroupID)
	}

	s.log.Info("приглашение в группу принято",
		slog.String("group_id", safeShort8(invite.GroupID)),
		slog.String("from", safeShort(fromPubKey)))

	s.network.emitEvent("vox:group-invite", map[string]string{
		"groupId": invite.GroupID,
		"name":    invite.Name,
	})

	// Replay буферизированных group_message, которые пришли раньше invite.
	s.flushPendingGroupMessages(invite.GroupID, "")

	// Новичок сам соединяется с участниками: peer_online о тех, кто уже в
	// сети, ему не придёт, а они его peer_online проигнорировали. Для офлайн
	// сигналка быстро ответит «не в сети».
	for _, pid := range invite.Members {
		if pid != myID {
			go s.network.ConnectToGroupPeer(pid)
		}
	}
	if byLink {
		s.completeJoin(invite.GroupID)
	}

	// Фото группы не кладём в очередь вместе с invite (порядок в pending не
	// гарантирован) — новичок сам просит его у пригласившего.
	if invite.AvatarHash != "" {
		var local string
		s.store.DB().QueryRow("SELECT avatar_hash FROM groups WHERE group_id = ?", invite.GroupID).Scan(&local)
		if local != invite.AvatarHash {
			if data := s.marshalP2P(P2PMessage{Type: "group_avatar_get", GroupID: invite.GroupID, Time: now}); data != nil {
				_ = s.network.SendP2PData(fromPubKey, data)
			}
		}
	}
}

func (s *GroupsService) handleGroupMessage(fromPubKey string, msg *P2PMessage) {
	// Проверяем что отправитель — активный участник группы.
	// Различаем три случая:
	//   1. Отправитель вообще неизвестен (ErrNoRows) — возможен ordering:
	//      group_message пришёл до group_invite/member_added. Буферизируем
	//      и replay'им когда членство станет известным.
	//   2. Статус найден, но != "active" (left/kicked) — dropаем навсегда.
	//   3. Статус "active" — обрабатываем нормально.
	var senderStatus string
	err := s.store.DB().QueryRow(
		"SELECT status FROM group_members WHERE group_id = ? AND peer_id = ?",
		msg.GroupID, fromPubKey).Scan(&senderStatus)
	if errors.Is(err, sql.ErrNoRows) {
		s.bufferPendingGroupMessage(fromPubKey, msg)
		return
	}
	if err != nil {
		s.log.Warn("ошибка проверки членства отправителя group msg",
			slog.String("group", msg.GroupID),
			slog.String("peer", safeShort(fromPubKey)),
			slog.String("error", err.Error()))
		return
	}
	if senderStatus != "active" {
		s.log.Warn("group message от не-активного участника, игнорируем",
			slog.String("group", msg.GroupID),
			slog.String("peer", safeShort(fromPubKey)),
			slog.String("status", senderStatus))
		return
	}

	opts := SendOptions{ReplyTo: msg.ReplyTo, ForwardedFrom: msg.ForwardedFrom}.normalize()
	// Сохраняем в БД (INSERT OR IGNORE для дедупликации)
	_, err = s.store.DB().Exec(
		`INSERT OR IGNORE INTO group_messages (id, group_id, sender_id, content, type, created_at, reply_to, forwarded_from)
		 VALUES (?, ?, ?, ?, 'text', ?, ?, ?)`,
		msg.ID, msg.GroupID, fromPubKey, msg.Content, msg.Time, encodeRef(opts.ReplyTo), encodeRef(opts.ForwardedFrom))
	if err != nil {
		s.log.Warn("не удалось сохранить group message", slog.String("error", err.Error()))
		return
	}

	// Ник отправителя: contacts, затем профиль не-друга (peer_profiles).
	nickname := s.network.PeerNickname(fromPubKey)

	// Считаем непрочитанные в группе
	myID, _ := s.sess.PeerID()
	var unreadCount int
	var lastReadAt int64
	s.store.DB().QueryRow(
		"SELECT last_read_at FROM group_members WHERE group_id = ? AND peer_id = ?",
		msg.GroupID, myID).Scan(&lastReadAt)
	s.store.DB().QueryRow(
		"SELECT COUNT(*) FROM group_messages WHERE group_id = ? AND deleted = 0 AND sender_id != ? AND created_at > ?",
		msg.GroupID, myID, lastReadAt).Scan(&unreadCount)

	var groupName string
	s.store.DB().QueryRow("SELECT name FROM groups WHERE group_id = ?", msg.GroupID).Scan(&groupName)

	s.log.Info("📥 GROUP ПОЛУЧЕНО",
		slog.String("group_id", safeShort8(msg.GroupID)),
		slog.String("peer", safeShort(fromPubKey)),
		slog.String("msg_id", msg.ID),
		logsafe.Len("len", msg.Content),
		slog.Int("unread_in_group", unreadCount))

	s.network.emitEvent("vox:group-message", map[string]any{
		"groupId":   msg.GroupID,
		"groupName": groupName,
		"message": map[string]any{
			"id":            msg.ID,
			"groupId":       msg.GroupID,
			"senderId":      fromPubKey,
			"senderName":    nickname,
			"content":       msg.Content,
			"replyTo":       opts.ReplyTo,
			"forwardedFrom": opts.ForwardedFrom,
			"timestamp":     msg.Time * 1000,
			"outbound":      false,
			"delivered":     true,
		},
	})
}

// SaveAndEmitGroupVoice — сохраняет собранное голосовое сообщение группы и уведомляет frontend
func (s *GroupsService) SaveAndEmitGroupVoice(fromPubKey string, meta *P2PMessage, wavData []byte) {
	// Проверяем membership
	var senderStatus string
	if err := s.store.DB().QueryRow(
		"SELECT status FROM group_members WHERE group_id = ? AND peer_id = ?",
		meta.GroupID, fromPubKey).Scan(&senderStatus); err != nil || senderStatus != "active" {
		return
	}

	_, err := s.store.DB().Exec(
		`INSERT OR IGNORE INTO group_messages (id, group_id, sender_id, content, type, audio_data, duration, waveform, created_at)
		 VALUES (?, ?, ?, ?, 'voice', ?, ?, ?, ?)`,
		meta.ID, meta.GroupID, fromPubKey, "🎤 Голосовое сообщение", wavData, meta.Duration, meta.Waveform, meta.Time)
	if err != nil {
		return
	}

	nickname := s.network.PeerNickname(fromPubKey)

	var groupName string
	s.store.DB().QueryRow("SELECT name FROM groups WHERE group_id = ?", meta.GroupID).Scan(&groupName)

	s.log.Info("📥 VOICE GROUP ПОЛУЧЕНО",
		slog.String("group_id", safeShort8(meta.GroupID)),
		slog.String("peer", safeShort(fromPubKey)),
		slog.String("msg_id", meta.ID),
		slog.Int("duration", meta.Duration),
		slog.Int("wav_size", len(wavData)))

	s.network.emitEvent("vox:group-message", map[string]any{
		"groupId":   meta.GroupID,
		"groupName": groupName,
		"message": map[string]any{
			"id":         meta.ID,
			"groupId":    meta.GroupID,
			"senderId":   fromPubKey,
			"senderName": nickname,
			"content":    "🎤 Голосовое сообщение",
			"msgType":    "voice",
			"duration":   meta.Duration,
			"waveform":   meta.Waveform,
			"timestamp":  meta.Time * 1000,
			"outbound":   false,
			"delivered":  true,
		},
	})
}

func (s *GroupsService) handleGroupLeave(fromPubKey string, msg *P2PMessage) {
	s.store.DB().Exec(
		"UPDATE group_members SET status = 'left' WHERE group_id = ? AND peer_id = ?",
		msg.GroupID, fromPubKey)

	s.log.Info("участник покинул группу",
		slog.String("group", msg.GroupID),
		slog.String("peer", safeShort(fromPubKey)))

	s.network.emitEvent("vox:group-member-left", map[string]string{
		"groupId": msg.GroupID,
		"peerId":  fromPubKey,
	})
}

// senderIsGroupAdmin — true если peerID является админом или владельцем группы
// по локальному состоянию. Входящие управляющие group_* команды приходят от
// недоверенного пира: без этой проверки любой, кто знает groupID, мог удалить
// или переименовать группу либо кикнуть тебя локально.
//
// Fail-closed: если о группе или об отправителе локально ничего не известно
// (ErrNoRows/ошибка) — считаем НЕ админом.
func (s *GroupsService) senderIsGroupAdmin(groupID, peerID string) bool {
	if groupID == "" || peerID == "" {
		return false
	}
	var ownerID string
	if err := s.store.DB().QueryRow(
		"SELECT owner_id FROM groups WHERE group_id = ?", groupID).Scan(&ownerID); err == nil {
		if ownerID == peerID {
			return true
		}
	}
	var role string
	if err := s.store.DB().QueryRow(
		"SELECT role FROM group_members WHERE group_id = ? AND peer_id = ?",
		groupID, peerID).Scan(&role); err == nil {
		return role == "admin"
	}
	return false
}

// senderIsGroupOwner — строже: только владелец. Для необратимого group_delete.
func (s *GroupsService) senderIsGroupOwner(groupID, peerID string) bool {
	if groupID == "" || peerID == "" {
		return false
	}
	var ownerID string
	if err := s.store.DB().QueryRow(
		"SELECT owner_id FROM groups WHERE group_id = ?", groupID).Scan(&ownerID); err != nil {
		return false
	}
	return ownerID == peerID
}

func (s *GroupsService) handleGroupKick(fromPubKey string, msg *P2PMessage) {
	if !s.senderIsGroupAdmin(msg.GroupID, fromPubKey) {
		s.log.Warn("group_kick от не-админа, игнорируем",
			slog.String("group", msg.GroupID), slog.String("peer", safeShort(fromPubKey)))
		return
	}
	myID, _ := s.sess.PeerID()

	// Полное удаление группы — кикнутый не может вернуться
	if tx, err := s.store.DB().Begin(); err == nil {
		if _, err := tx.Exec("DELETE FROM group_members WHERE group_id = ? AND peer_id = ?", msg.GroupID, myID); err != nil {
			tx.Rollback()
			s.log.Error("ошибка удаления при кике", slog.String("group", msg.GroupID), slog.String("error", err.Error()))
		} else if _, err := tx.Exec("DELETE FROM group_messages WHERE group_id = ?", msg.GroupID); err != nil {
			tx.Rollback()
			s.log.Error("ошибка удаления при кике", slog.String("group", msg.GroupID), slog.String("error", err.Error()))
		} else if _, err := tx.Exec("DELETE FROM groups WHERE group_id = ?", msg.GroupID); err != nil {
			tx.Rollback()
			s.log.Error("ошибка удаления при кике", slog.String("group", msg.GroupID), slog.String("error", err.Error()))
		} else {
			tx.Commit()
		}
	}

	s.log.Info("кикнут из группы (удалена)", slog.String("group", msg.GroupID))

	s.network.emitEvent("vox:group-kicked", map[string]string{
		"groupId": msg.GroupID,
	})
}

func (s *GroupsService) handleGroupDelete(fromPubKey string, msg *P2PMessage) {
	if !s.senderIsGroupOwner(msg.GroupID, fromPubKey) {
		s.log.Warn("group_delete от не-владельца, игнорируем",
			slog.String("group", msg.GroupID), slog.String("peer", safeShort(fromPubKey)))
		return
	}
	if tx, err := s.store.DB().Begin(); err == nil {
		if _, err := tx.Exec("DELETE FROM group_members WHERE group_id = ?", msg.GroupID); err != nil {
			tx.Rollback()
			s.log.Error("ошибка удаления группы", slog.String("group", msg.GroupID), slog.String("error", err.Error()))
		} else if _, err := tx.Exec("DELETE FROM group_messages WHERE group_id = ?", msg.GroupID); err != nil {
			tx.Rollback()
			s.log.Error("ошибка удаления группы", slog.String("group", msg.GroupID), slog.String("error", err.Error()))
		} else if _, err := tx.Exec("DELETE FROM groups WHERE group_id = ?", msg.GroupID); err != nil {
			tx.Rollback()
			s.log.Error("ошибка удаления группы", slog.String("group", msg.GroupID), slog.String("error", err.Error()))
		} else {
			tx.Commit()
		}
	}

	s.log.Info("группа удалена владельцем", slog.String("group", msg.GroupID))

	s.network.emitEvent("vox:group-deleted", map[string]string{
		"groupId": msg.GroupID,
	})
}

func (s *GroupsService) handleGroupRename(fromPubKey string, msg *P2PMessage) {
	if !s.senderIsGroupAdmin(msg.GroupID, fromPubKey) {
		s.log.Warn("group_rename от не-админа, игнорируем",
			slog.String("group", msg.GroupID), slog.String("peer", safeShort(fromPubKey)))
		return
	}
	s.store.DB().Exec(
		"UPDATE groups SET name = ? WHERE group_id = ?",
		msg.Content, msg.GroupID)

	s.log.Info("группа переименована",
		slog.String("group_id", safeShort8(msg.GroupID)))

	s.network.emitEvent("vox:group-renamed", map[string]string{
		"groupId": msg.GroupID,
		"name":    msg.Content,
	})
}
