package services

import "database/sql"

// Превью последнего сообщения в списке чатов (фаза 6c редизайна, макеты
// Main / MessagesText): «🎤 Голосовое сообщение · 0:42», «📷 3 фото»,
// «📎 файл.pdf», «Ник: текст» в группах. Сообщение без текста (фото без
// подписи, голосовое) — тоже превью: раньше отбор по content != "" его
// выкидывал, и строка чата оставалась пустой.

// lastDMMessage — последнее неудалённое сообщение переписки; onlyIncoming —
// только входящее (для чата с непрочитанными). nil — сообщений нет.
func lastDMMessage(db *sql.DB, peerID, myID string, onlyIncoming bool) *LastMessage {
	q := `SELECT id, content, sender_id, created_at, type, duration FROM messages
	      WHERE peer_id = ? AND deleted = 0`
	args := []any{peerID}
	if onlyIncoming {
		q += " AND sender_id != ?"
		args = append(args, myID)
	}
	q += " ORDER BY created_at DESC LIMIT 1"
	var id, content, senderID, msgType string
	var created int64
	var dur int
	if err := db.QueryRow(q, args...).Scan(&id, &content, &senderID, &created, &msgType, &dur); err != nil {
		return nil
	}
	lm := &LastMessage{
		Content:   content,
		Timestamp: created * 1000,
		Outbound:  senderID == myID,
		MsgType:   msgType,
		Duration:  dur,
	}
	fillAttachmentPreview(db, lm, "message_id", id)
	return lm
}

// lastGroupMessage — последнее неудалённое сообщение группы с ником автора
// (contacts → peer_profiles, как в GetGroupMessages). nil — сообщений нет.
func lastGroupMessage(db *sql.DB, groupID, myID string) *LastMessage {
	var id, content, senderID, msgType string
	var created int64
	var dur int
	if err := db.QueryRow(`SELECT id, content, sender_id, created_at, type, duration FROM group_messages
	                       WHERE group_id = ? AND deleted = 0
	                       ORDER BY created_at DESC LIMIT 1`, groupID).
		Scan(&id, &content, &senderID, &created, &msgType, &dur); err != nil {
		return nil
	}
	lm := &LastMessage{
		Content:   content,
		Timestamp: created * 1000,
		Outbound:  senderID == myID,
		MsgType:   msgType,
		Duration:  dur,
	}
	if !lm.Outbound {
		_ = db.QueryRow(`SELECT COALESCE(
		                   (SELECT NULLIF(nickname, '') FROM contacts WHERE peer_id = ?),
		                   (SELECT NULLIF(nickname, '') FROM peer_profiles WHERE peer_id = ?),
		                   '')`, senderID, senderID).Scan(&lm.SenderName)
	}
	fillAttachmentPreview(db, lm, "group_msg_id", id)
	return lm
}

// fillAttachmentPreview — число вложений сообщения и первое из них (вид,
// имя). col — "message_id" | "group_msg_id" (только из этого файла).
func fillAttachmentPreview(db *sql.DB, lm *LastMessage, col, msgID string) {
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM message_attachments WHERE "+col+" = ?", msgID).Scan(&n); err != nil || n == 0 {
		return
	}
	lm.Attachments = n
	_ = db.QueryRow("SELECT kind, name FROM message_attachments WHERE "+col+" = ? ORDER BY ord LIMIT 1", msgID).
		Scan(&lm.AttachmentKind, &lm.AttachmentName)
}

// callRecordRead — флаг read для системной записи о звонке: пропущенный
// входящий — непрочитанный (счётчик беседы, бейдж «Чатов»; снимается
// MarkMessagesRead при открытии чата), остальные исходы — прочитанные.
func callRecordRead(outcome string, outbound bool) int {
	if outcome == "missed" && !outbound {
		return 0
	}
	return 1
}
