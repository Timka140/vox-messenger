package files

import (
	"time"

	"vox_app/internal/services/files/wire"
)

// emitFileMessage — фронт получает полноценное vox:message / vox:group-message
// сразу при получении manifest'а (ещё до реальной скачки chunks). UI рисует
// bubble с iconкой файла / placeholder-img, который подтянется позже через
// /vox-attach/<sha> по мере того как chunks докачаются.
//
// Структура attachments совместима с LoadAttachmentsForMessage (legacy):
//
//	{ id, kind, name, mime, size, sha256, width, height }
//
// id = sha (фронт использует sha для /vox-attach/), sha256 = то же sha.
func (s *FilesService) emitFileMessage(p *wire.FileManifestPayload) {
	if s.msg == nil {
		return
	}
	now := time.Now().UnixMilli()
	atts := make([]map[string]any, 0, len(p.Files))
	for _, f := range p.Files {
		// available — true если все chunks уже на диске (dedup). Для нового
		// файла false → фронт покажет Download-кнопку (либо auto-pull для
		// мелких photo — это решает onFileManifest, не здесь).
		available := s.isFullyAvailable(&f)
		atts = append(atts, map[string]any{
			"id":        f.Sha,
			"kind":      f.Kind,
			"name":      f.Name,
			"mime":      f.Mime,
			"size":      f.Size,
			"sha256":    f.Sha,
			"width":     f.Width,
			"height":    f.Height,
			"available": available,
			"fromPeer":  p.SenderID,
		})
	}
	// Также эмитим компактный file-message для offcanvas/media-gallery
	// которые могут не хотеть тащить полный payload.
	s.msg.EmitEvent("vox:file-message", map[string]any{
		"msgId":   p.MsgID,
		"groupId": p.GroupID,
		"caption": p.Caption,
		"files":   atts,
	})
	// Главный эмит: полный vox:message с attachments для appendMessage в UI.
	// Для DM peerID/senderID — это отправитель (fromPeer). Для группы доп. fields.
	senderID := p.SenderID
	if p.GroupID == "" {
		s.msg.EmitEvent("vox:message", map[string]any{
			"id":            p.MsgID,
			"peerId":        senderID,
			"senderId":      senderID,
			"content":       p.Caption,
			"msgType":       "attachment",
			"timestamp":     now,
			"attachments":   atts,
			"forwardedFrom": p.ForwardedFrom,
		})
		return
	}
	s.msg.EmitEvent("vox:group-message", map[string]any{
		"groupId": p.GroupID,
		"message": map[string]any{
			"id":            p.MsgID,
			"groupId":       p.GroupID,
			"senderId":      senderID,
			"content":       p.Caption,
			"msgType":       "attachment",
			"timestamp":     now,
			"attachments":   atts,
			"forwardedFrom": p.ForwardedFrom,
		},
	})
}

// emitFileProgress — на каждые ~50 chunks или при изменении состояния (debounced).
// Фронт обновляет progress-overlay в bubble.
func (s *FilesService) emitFileProgress(st *downloadState) {
	if s.msg == nil {
		return
	}
	st.mu.Lock()
	received := st.bitmap.Count()
	total := st.bitmap.Total()
	chunkSize := st.manifest.ChunkSize
	st.mu.Unlock()
	s.msg.EmitEvent("vox:file-progress", map[string]any{
		"fileSha":  st.fileSha,
		"peer":     st.sourcePeer,
		"received": received,
		"total":    total,
		"bytes":    int64(received) * int64(chunkSize), // приблизительно
	})
}

// emitFileReceived — bitmap полный, файл готов к отображению/скачиванию.
func (s *FilesService) emitFileReceived(fileSha, msgID string) {
	if s.msg == nil {
		return
	}
	s.msg.EmitEvent("vox:file-received", map[string]any{
		"fileSha": fileSha,
		"msgId":   msgID,
	})
}

// emitFileCancelled — пользователь отменил скачку. Фронт восстанавливает
// download-кнопку чтобы можно было запустить заново.
func (s *FilesService) emitFileCancelled(fileSha string) {
	if s.msg == nil {
		return
	}
	s.msg.EmitEvent("vox:file-cancelled", map[string]any{
		"fileSha": fileSha,
	})
}

// ──── Sender-side progress (для UI uploads.js) ────────────────────────────
//
// В CAS pipeline sender реально не «отправляет» файл — он лишь шлёт manifest
// и ждёт chunk_request. Поэтому семантика «отправлено» = «manifest доставлен +
// готов отвечать на запросы». Сразу после manifest_sent эмитим прогресс 100%
// для каждого peer'а — UI на bubble убирает спиннер. Если кто-то реально
// придёт за chunks потом — это fully-pull workflow на receiver'е, sender уже
// своё дело сделал.

// emitSenderManifestSent — сразу после успешной отправки manifest'а одному
// peer'у. UI uploads.js видит 100% по этому peer'у → отметить delivered.
func (s *FilesService) emitSenderManifestSent(msgID, peerID string, totalBytes int64) {
	if s.msg == nil {
		return
	}
	// Совместимый с uploads.js формат события vox:attach-progress.
	s.msg.EmitEvent("vox:attach-progress", map[string]any{
		"msgId":  msgID,
		"peerId": peerID,
		"sent":   totalBytes, // = total = 100%
		"total":  totalBytes,
	})
	s.msg.EmitEvent("vox:attach-complete", map[string]any{
		"msgId":  msgID,
		"peerId": peerID,
	})
}

// emitSenderManifestFailed — manifest не доставлен (peer offline или DC закрыт).
// UI покажет «не отправлено», обычный fail flow.
func (s *FilesService) emitSenderManifestFailed(msgID, peerID, reason string) {
	if s.msg == nil {
		return
	}
	s.msg.EmitEvent("vox:attach-failed", map[string]any{
		"msgId":  msgID,
		"peerId": peerID,
		"error":  reason,
	})
}
