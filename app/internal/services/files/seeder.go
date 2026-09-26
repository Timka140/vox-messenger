package files

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"vox_app/internal/logsafe"
	"vox_app/internal/services/files/wire"
	"vox_app/internal/storage/cas"
)

// registerUpload помещает upload в карту активных передач. Каждый peer
// получает свой peerUpload-стейт.
func (s *FilesService) registerUpload(msgID, groupID string, recipients []string, totalBytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := &uploadState{
		msgID:      msgID,
		groupID:    groupID,
		peers:      make(map[string]*peerUpload, len(recipients)),
		startedAt:  time.Now(),
		totalBytes: totalBytes,
	}
	for _, p := range recipients {
		st.peers[p] = &peerUpload{}
	}
	s.uploads[msgID] = st
}

// manifestPCReadyTimeout — сколько sendManifestsToPeer ждёт открытия
// attachments DC (см. комментарий в теле).
const manifestPCReadyTimeout = 30 * time.Second

// sendManifestsToPeer отправляет одному peer'у FILE_MANIFEST со всеми
// файлами сообщения. Если peer offline — выходит, manifest будет переотправлен
// при reconnect через DeliverPendingGroupMessages → restartFileDelivery.
func (s *FilesService) sendManifestsToPeer(msgID, groupID, peerID, caption string,
	items []FileItem, manifests []wire.FileManifestEntry) {

	// Один manifest на (msgID, peer) за раз: повторный вызов, пока первый
	// ещё ждёт attach DC, просто выходит — первый доставит.
	inflightKey := msgID + "|" + peerID
	s.mu.Lock()
	if s.manifestInFlight[inflightKey] {
		s.mu.Unlock()
		s.log.Debug("file manifest: уже отправляется, пропускаем повтор",
			slog.String("peer", safeShort(peerID)),
			slog.String("msg_id", msgID))
		return
	}
	s.manifestInFlight[inflightKey] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.manifestInFlight, inflightKey)
		s.mu.Unlock()
	}()

	if !s.network.IsP2PConnected(peerID) {
		s.log.Debug("file manifest: peer offline",
			slog.String("peer", safeShort(peerID)),
			slog.String("msg_id", msgID))
		s.emitSenderManifestFailed(msgID, peerID, "peer offline")
		return
	}
	// Лениво поднимаем files PC и ждём открытия DC. Handshake через TURN-relay
	// занимает 3-5 с, при glare на files PC (обе стороны подняли его
	// одновременно) answer приходит через ~8-12 с — 10 с не хватало
	// (2026-09-08: фото не доходили после реконнекта). Даём 30 с.
	ctx, cancel := context.WithTimeout(context.Background(), manifestPCReadyTimeout)
	if err := s.network.EnsureFilesPCReady(ctx, peerID); err != nil {
		cancel()
		// НЕ emitSenderManifestFailed: сообщение остаётся pending в БД и
		// уйдёт по TopicAttachReady / следующему реконнекту. «Не отправлено»
		// в UI показываем только при реальном отказе (peer offline, send fail).
		s.log.Info("file manifest: files PC not ready, ждём attach DC",
			slog.String("peer", safeShort(peerID)),
			slog.String("msg_id", msgID),
			logsafe.Err("err", err))
		return
	}
	cancel()

	payload := &wire.FileManifestPayload{
		MsgID:   msgID,
		GroupID: groupID,
		Caption: caption,
		Files:   manifests,
	}
	payload.ForwardedFrom = s.lookupForwardedFrom(msgID, groupID)
	frame, err := wire.EncodeFileManifest(payload)
	if err != nil {
		s.log.Error("file manifest: encode",
			slog.String("msg_id", msgID),
			logsafe.Err("err", err))
		return
	}
	if err := s.network.SendAttachChunk(peerID, frame); err != nil {
		s.log.Warn("file manifest: send",
			slog.String("peer", safeShort(peerID)),
			slog.String("msg_id", msgID),
			logsafe.Err("err", err))
		s.emitSenderManifestFailed(msgID, peerID, "send failed: "+err.Error())
		return
	}

	totalBytes := int64(0)
	for _, m := range manifests {
		totalBytes += m.Size
	}
	s.log.Info("📤 FILE MANIFEST sent",
		slog.String("peer", safeShort(peerID)),
		slog.String("msg_id", msgID),
		slog.Int("files", len(manifests)),
		slog.Int64("bytes", totalBytes))

	// Sender свою часть выполнил: manifest доставлен, готов отвечать на
	// chunk_request'ы. Эмитим 100% для UI чтобы убрать spinner с outbound bubble.
	// Реальная скачка теперь в зоне receiver'а.
	s.emitSenderManifestSent(msgID, peerID, totalBytes)

	// Манифест доставлен — delivered=1 (DM: messages, группа: строка участника
	// в group_message_delivery).
	if groupID != "" {
		s.markDeliveredGroup(msgID, peerID)
	} else {
		s.markDeliveredDM(msgID, peerID)
	}
}

// markDeliveredDM — UPDATE messages SET delivered=1 для DM-сообщений после
// успешной доставки manifest'а. Не критично если упадёт (idempotent).
func (s *FilesService) markDeliveredDM(msgID, peerID string) {
	if _, err := s.db.Exec(
		`UPDATE messages SET delivered = 1 WHERE id = ? AND peer_id = ? AND delivered = 0`,
		msgID, peerID); err != nil {
		s.log.Debug("mark delivered: skip", logsafe.Err("err", err))
	}
}

// onChunkRequest обрабатывает входящий 0x11 фрейм — receiver просит N chunks
// начиная с idx. Читаем из CAS, отправляем CHUNK_DATA в ответ. Для каждого
// missing chunk — CHUNK_NAK.
func (s *FilesService) onChunkRequest(fromPeer string, frame []byte) {
	fileSha, startIdx, count, err := wire.DecodeChunkRequest(frame)
	if err != nil {
		s.log.Warn("chunk request: decode",
			slog.String("peer", safeShort(fromPeer)),
			logsafe.Err("err", err))
		return
	}
	s.ensureStatsLoop()

	manifest, err := cas.LoadManifest(s.db, fileSha)
	if err != nil {
		// Файла у нас нет — возможно был удалён. Шлём NAK для всех запрошенных.
		s.log.Debug("chunk request: file not found",
			slog.String("peer", safeShort(fromPeer)),
			slog.String("file", safeShort8(fileSha)))
		for i := 0; i < count; i++ {
			nak, _ := wire.EncodeChunkNak(fileSha, startIdx+i, wire.NakRemoved)
			_ = s.network.SendAttachChunk(fromPeer, nak)
		}
		return
	}

	// Single-file модель: оригинал/assembled-файл на диске, чанки читаем по
	// offset. Streaming-режим (source=streaming) даёт invariant guard
	// (size+mtime); CAS-режим (source=cas, sender = receiver когда-то) —
	// верифицируем по sha-чанка из manifest.Chunks[idx].
	streaming := manifest.Source == "streaming"
	originPath := cas.GetFileLocalPath(s.db, fileSha)
	if originPath == "" || !FileExists(originPath) {
		s.log.Debug("chunk request: local_path missing",
			slog.String("file", safeShort8(fileSha)),
			slog.String("peer", safeShort(fromPeer)))
		for i := 0; i < count; i++ {
			nak, _ := wire.EncodeChunkNak(fileSha, startIdx+i, wire.NakRemoved)
			_ = s.network.SendAttachChunk(fromPeer, nak)
		}
		return
	}

	var missCount int
	var firstMissIdx int = -1
	var firstMissErr string
	for i := 0; i < count; i++ {
		idx := startIdx + i
		if idx >= manifest.ChunkCount {
			break // запрос вышел за границу — игнорируем хвост
		}
		chunkSha := manifest.Chunks[idx]

		var plaintext []byte
		if streaming {
			// Strict invariant: size+mtime должны совпадать с manifest'ом —
			// иначе файл изменили, отдаём NAK.
			plaintext, err = cas.ReadChunkFromFile(originPath, idx, manifest.ChunkSize, manifest.OriginSize, manifest.OriginMtime)
		} else {
			// CAS-режим: 0/0 для size/mtime, проверяем по sha. Если контент
			// файла не соответствует manifest.Chunks[idx] (юзер изменил
			// файл после receive) — sha не совпадёт, NAK.
			plaintext, err = cas.ReadChunkFromFile(originPath, idx, manifest.ChunkSize, 0, 0)
			if err == nil && cas.HashOf(plaintext) != chunkSha {
				err = fmt.Errorf("chunk sha mismatch: file changed since manifest")
			}
		}
		if err != nil {
			reason := wire.NakNotFound
			if errors.Is(err, cas.ErrChunkNotFound) {
				reason = wire.NakRemoved
			}
			if streaming {
				// Любая ошибка чтения оригинала (deleted/moved/changed) —
				// сообщаем receiver что файл утерян, передача отменяется.
				reason = wire.NakRemoved
			}
			if firstMissIdx < 0 {
				firstMissIdx = idx
				firstMissErr = err.Error()
			}
			missCount++
			nak, _ := wire.EncodeChunkNak(fileSha, idx, reason)
			_ = s.network.SendAttachChunk(fromPeer, nak)
			continue
		}
		dataFrame, err := wire.EncodeChunkData(fileSha, idx, plaintext)
		if err != nil {
			s.log.Error("chunk request: encode data", logsafe.Err("err", err))
			continue
		}
		if err := s.sendChunkFrame(fromPeer, idx, dataFrame); err != nil {
			s.log.Warn("chunk request: send data",
				slog.String("peer", safeShort(fromPeer)),
				slog.Int("idx", idx),
				logsafe.Err("err", err))
			return // peer отвалился — прекращаем batch, receiver перезапросит по stall
		}
		s.stats.bytesOut.Add(int64(len(dataFrame)))
		s.stats.chunksOut.Add(1)
	}
	if missCount > 0 {
		s.log.Debug("chunk request: missing chunks",
			slog.String("file", safeShort8(fileSha)),
			slog.Int("count", missCount),
			slog.Int("first_idx", firstMissIdx),
			slog.String("err", firstMissErr))
	}
}

// chunkSendThrottleRetries — сколько раз повторяем SendAttachChunk, если
// SCTP-буфер не дренится (SendAttachChunk ждёт 5 с и возвращает «send
// timeout»). Раньше первый же таймаут обрывал пачку: чанки терялись, а
// receiver ждал stall-таймаут (2026-09-08 14:55: 16 чанков, 30 с простоя).
// 6 × 5 с = 30 с реального затыка канала — только тогда сдаёмся.
const chunkSendThrottleRetries = 6

// sendChunkFrame — SendAttachChunk с повтором на throttle-таймауте. Прочие
// ошибки (DC закрыт, peer ушёл) возвращаются сразу.
func (s *FilesService) sendChunkFrame(peer string, idx int, frame []byte) error {
	var err error
	for attempt := 0; attempt <= chunkSendThrottleRetries; attempt++ {
		err = s.network.SendAttachChunk(peer, frame)
		if err == nil {
			return nil
		}
		if !strings.Contains(err.Error(), "send timeout") {
			return err
		}
		s.log.Debug("chunk send: throttle-wait, retry",
			slog.String("peer", safeShort(peer)),
			slog.Int("idx", idx),
			slog.Int("attempt", attempt+1))
	}
	return err
}

// onFileDecline — peer отказался принять файл. Чистим upload state.
func (s *FilesService) onFileDecline(fromPeer string, frame []byte) {
	msgID, err := wire.DecodeFileDecline(frame)
	if err != nil {
		return
	}
	s.mu.Lock()
	if st, ok := s.uploads[msgID]; ok {
		st.mu.Lock()
		if pu, ok := st.peers[fromPeer]; ok {
			pu.declined = true
		}
		st.mu.Unlock()
	}
	s.mu.Unlock()
	s.log.Info("file decline",
		slog.String("peer", safeShort(fromPeer)),
		slog.String("msg_id", msgID))
}
