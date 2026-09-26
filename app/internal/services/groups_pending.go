package services

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"time"
)

// bufferPendingGroupMessage сохраняет group_message в память, когда отправитель
// ещё не известен у нас (group_invite или member_added ещё не обработан).
// Буфер ограничен TTL и размером per-group.
func (s *GroupsService) bufferPendingGroupMessage(fromPubKey string, msg *P2PMessage) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()

	now := time.Now()
	bucket := s.pendingMsg[msg.GroupID]
	// Сбрасываем протухшие записи лениво.
	fresh := bucket[:0]
	for _, p := range bucket {
		if now.Sub(p.receivedAt) < pendingGroupMsgTTL {
			fresh = append(fresh, p)
		}
	}
	if len(fresh) >= pendingGroupMsgMaxPerGrp {
		s.log.Warn("pending group msg буфер переполнен, дропаем старейший",
			slog.String("group", safeShort8(msg.GroupID)))
		fresh = fresh[1:]
	}
	fresh = append(fresh, pendingGroupMsg{
		fromPubKey: fromPubKey,
		msg:        *msg,
		receivedAt: now,
	})
	s.pendingMsg[msg.GroupID] = fresh

	s.log.Debug("group msg буферизирован (отправитель ещё не известен)",
		slog.String("group", safeShort8(msg.GroupID)),
		slog.String("peer", safeShort(fromPubKey)),
		slog.String("msg_id", msg.ID),
		slog.Int("buffer_size", len(fresh)))
}

// flushPendingGroupMessages replay'ит буферизированные group_message для указанной
// группы. Если peerFilter != "" — replay'им только сообщения от этого отправителя
// (используется после member_added для конкретного пира). Если peerFilter == ""
// (после group_invite) — replay'им все.
func (s *GroupsService) flushPendingGroupMessages(groupID, peerFilter string) {
	s.pendingMu.Lock()
	bucket := s.pendingMsg[groupID]
	if len(bucket) == 0 {
		s.pendingMu.Unlock()
		return
	}
	var toReplay []pendingGroupMsg
	var keep []pendingGroupMsg
	now := time.Now()
	for _, p := range bucket {
		if now.Sub(p.receivedAt) >= pendingGroupMsgTTL {
			continue // протухло, дропаем
		}
		if peerFilter == "" || p.fromPubKey == peerFilter {
			toReplay = append(toReplay, p)
		} else {
			keep = append(keep, p)
		}
	}
	if len(keep) > 0 {
		s.pendingMsg[groupID] = keep
	} else {
		delete(s.pendingMsg, groupID)
	}
	s.pendingMu.Unlock()

	for i := range toReplay {
		p := toReplay[i]
		s.log.Info("replay буферизированного group msg",
			slog.String("group", safeShort8(groupID)),
			slog.String("peer", safeShort(p.fromPubKey)),
			slog.String("msg_id", p.msg.ID))
		msgCopy := p.msg
		s.handleGroupMessage(p.fromPubKey, &msgCopy)
	}
}

// DeliverPendingGroupMessages отправляет все недоставленные групповые сообщения пиру.
// Вызывается при DataChannel.OnOpen (reconnect).
func (s *GroupsService) DeliverPendingGroupMessages(peerID string) {
	myID, err := s.sess.PeerID()
	if err != nil {
		return
	}

	if !s.network.IsP2PConnected(peerID) {
		return
	}

	// Собираем undelivered сообщения для этого пира
	rows, err := s.store.DB().Query(
		`SELECT gmd.msg_id, gm.group_id, gm.content, gm.created_at, gm.type, gm.reply_to, gm.forwarded_from
		 FROM group_message_delivery gmd
		 JOIN group_messages gm ON gm.id = gmd.msg_id
		 WHERE gmd.peer_id = ? AND gmd.delivered = 0 AND gm.sender_id = ? AND gm.deleted = 0
		 ORDER BY gm.created_at ASC`,
		peerID, myID)
	if err != nil {
		return
	}
	defer rows.Close()

	type pendingGMsg struct {
		MsgID   string
		GroupID string
		Content string
		Time    int64
		MsgType string
		ReplyTo string
		FwdFrom string
	}
	var pending []pendingGMsg
	for rows.Next() {
		var m pendingGMsg
		if err := rows.Scan(&m.MsgID, &m.GroupID, &m.Content, &m.Time, &m.MsgType, &m.ReplyTo, &m.FwdFrom); err != nil {
			continue
		}
		pending = append(pending, m)
	}

	if len(pending) > 0 {
		s.log.Info("📬 ДОСТАВКА PENDING GROUP",
			slog.String("peer", safeShort(peerID)),
			slog.Int("pending_count", len(pending)))
	}

	delivered := 0
	deliveredGroups := make(map[string]struct{})
	for _, m := range pending {
		var ok bool
		if m.MsgType == "voice" {
			ok = s.deliverPendingGroupVoice(peerID, m.MsgID, m.GroupID, m.Time)
		} else if m.MsgType == "attachment" {
			// Вложения нового формата (message_files) — повторный FILE_MANIFEST
			// через FilesService; delivered=1 проставит sendManifestsToPeer.
			var hasCAS int
			_ = s.store.DB().QueryRow(
				`SELECT 1 FROM message_files WHERE group_msg_id = ? LIMIT 1`,
				m.MsgID).Scan(&hasCAS)
			if hasCAS == 1 && s.filesRetryUploadGroup != nil {
				go s.filesRetryUploadGroup(peerID, m.MsgID)
				s.log.Debug("  → pending GROUP ATTACH (CAS) перезапущен",
					slog.String("msg_id", m.MsgID),
					slog.String("group", safeShort8(m.GroupID)))
				continue
			}
			// Перезапускаем chunked attachment sender. delivered=1 для этого peer'а
			// проставится внутри sendAttachmentsToPeer после complete — не помечаем
			// ok=true иначе UPDATE снизу подвинет статус преждевременно.
			if s.restartAttachmentDelivery != nil {
				s.restartAttachmentDelivery(peerID, m.MsgID, m.Content)
				s.log.Debug("  → pending GROUP ATTACH перезапущен",
					slog.String("msg_id", m.MsgID),
					slog.String("group", safeShort8(m.GroupID)))
			}
			continue
		} else {
			p2pMsg := P2PMessage{
				Type:          "group_message",
				ID:            m.MsgID,
				GroupID:       m.GroupID,
				Content:       m.Content,
				Time:          m.Time,
				ReplyTo:       decodeReply(m.ReplyTo),
				ForwardedFrom: decodeForward(m.FwdFrom),
			}
			data := s.marshalP2P(p2pMsg)
			if data != nil {
				if err := s.network.SendP2PData(peerID, data); err != nil {
					s.log.Warn("не удалось доставить pending group msg",
						slog.String("peer", safeShort(peerID)),
						slog.String("id", m.MsgID),
						slog.String("error", err.Error()))
					continue
				}
				ok = true
			}
		}

		if !ok {
			continue
		}

		if _, err := s.store.DB().Exec(
			"UPDATE group_message_delivery SET delivered = 1 WHERE msg_id = ? AND peer_id = ?",
			m.MsgID, peerID); err != nil {
			s.log.Warn("не удалось пометить group msg как доставленное (P2P отправка прошла, но БД UPDATE упал)",
				slog.String("msg_id", m.MsgID),
				slog.String("peer", safeShort(peerID)),
				slog.String("error", err.Error()))
		}
		delivered++
		deliveredGroups[m.GroupID] = struct{}{}

		s.log.Debug("  → pending group msg доставлено",
			slog.String("msg_id", m.MsgID),
			slog.String("group_id", safeShort8(m.GroupID)),
			slog.String("type", m.MsgType))
	}

	if delivered > 0 {
		s.log.Info("📬 PENDING GROUP ДОСТАВЛЕНО",
			slog.String("peer", safeShort(peerID)),
			slog.Int("delivered", delivered),
			slog.Int("total_pending", len(pending)))

		// Уведомляем фронтенд чтобы обновить часики → галочки.
		// groupIds — множество групп, чьи сообщения реально доставлены, чтобы
		// фронт не помечал галочками сообщения посторонней открытой группы.
		gids := make([]string, 0, len(deliveredGroups))
		for gid := range deliveredGroups {
			gids = append(gids, gid)
		}
		s.network.emitEvent("vox:group-messages-delivered", map[string]any{
			"peerId":   peerID,
			"count":    delivered,
			"groupIds": gids,
		})
	}
}

// deliverPendingGroupVoice отправляет офлайн voice сообщение группы через chunked протокол
func (s *GroupsService) deliverPendingGroupVoice(peerID, msgID, groupID string, createdAt int64) bool {
	var audioData []byte
	var duration int
	var waveform string
	err := s.store.DB().QueryRow(
		"SELECT audio_data, duration, waveform FROM group_messages WHERE id = ? AND type = 'voice'",
		msgID).Scan(&audioData, &duration, &waveform)
	if err != nil || len(audioData) == 0 {
		return false
	}

	audioB64 := base64.StdEncoding.EncodeToString(audioData)

	// 1. metadata
	meta := P2PMessage{
		Type:     "group_voice_message",
		ID:       msgID,
		GroupID:  groupID,
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
			GroupID: groupID,
			Content: audioB64[start:end],
			Audio:   fmt.Sprintf("%d/%d", i, totalChunks),
		}
		chunkData := s.marshalP2P(chunk)
		if chunkData == nil {
			return false
		}
		if err := s.network.SendP2PData(peerID, chunkData); err != nil {
			return false
		}
	}

	// 3. complete
	complete := P2PMessage{
		Type:    "voice_complete",
		ID:      msgID,
		GroupID: groupID,
	}
	completeData := s.marshalP2P(complete)
	if completeData != nil {
		s.network.SendP2PData(peerID, completeData)
	}

	s.log.Info("📬 pending group voice доставлено",
		slog.String("peer", safeShort(peerID)),
		slog.String("msg_id", msgID),
		slog.Int("chunks", totalChunks))

	return true
}

// persistGroupAction сохраняет pending action для офлайн пира
func (s *GroupsService) persistGroupAction(id, groupID, peerID, actionType, payload string, createdAt int64) {
	_, err := s.store.DB().Exec(
		`INSERT OR IGNORE INTO pending_group_actions (id, group_id, peer_id, type, payload, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		id, groupID, peerID, actionType, payload, createdAt)
	if err != nil {
		s.log.Warn("не удалось сохранить pending action",
			slog.String("type", actionType),
			slog.String("peer", safeShort(peerID)),
			slog.String("error", err.Error()))
	}
}

// DeliverPendingGroupActions — доставляет pending group actions при подключении пира
func (s *GroupsService) DeliverPendingGroupActions(peerID string) {
	rows, err := s.store.DB().Query(
		`SELECT id, group_id, type, payload, created_at
		 FROM pending_group_actions
		 WHERE peer_id = ?
		 ORDER BY created_at ASC`, peerID)
	if err != nil {
		return
	}
	defer rows.Close()

	type pendingAction struct {
		id         string
		groupID    string
		actionType string
		payload    string
		createdAt  int64
	}

	var actions []pendingAction
	for rows.Next() {
		var a pendingAction
		if err := rows.Scan(&a.id, &a.groupID, &a.actionType, &a.payload, &a.createdAt); err != nil {
			continue
		}
		actions = append(actions, a)
	}

	if len(actions) == 0 {
		return
	}

	s.log.Info("📬 ДОСТАВКА PENDING GROUP ACTIONS",
		slog.String("peer", safeShort(peerID)),
		slog.Int("count", len(actions)))

	delivered := 0
	for _, a := range actions {
		if !s.network.IsP2PConnected(peerID) {
			break
		}

		p2pMsg := P2PMessage{
			Type:    a.actionType,
			ID:      a.id,
			GroupID: a.groupID,
			Content: a.payload,
			Time:    a.createdAt,
		}
		data := s.marshalP2P(p2pMsg)
		if data == nil {
			continue
		}
		if err := s.network.SendP2PData(peerID, data); err != nil {
			s.log.Warn("не удалось доставить pending action",
				slog.String("type", a.actionType),
				slog.String("peer", safeShort(peerID)),
				slog.String("error", err.Error()))
			break
		}

		// Успешно доставлено — удаляем из очереди
		s.store.DB().Exec("DELETE FROM pending_group_actions WHERE id = ? AND peer_id = ?", a.id, peerID)
		delivered++
	}

	if delivered > 0 {
		s.log.Info("📬 PENDING GROUP ACTIONS ДОСТАВЛЕНО",
			slog.String("peer", safeShort(peerID)),
			slog.Int("delivered", delivered),
			slog.Int("total", len(actions)))
	}
}
