package services

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"vox_app/internal/storage"

	"github.com/google/uuid"
)

// attachments_p2p.go — chunked P2P transfer для attachment-сообщений.
//
// Sender flow (sendAttachmentsToPeer):
//   1. INSERT в attachments+message_attachments уже сделан в SendAttachments.
//   2. Здесь: marshal offer (msgId+caption+per-att metadata) → отправить attachment_offer.
//   3. Ждать attachment_offer_ack от receiver: per-att offset (-1=dedup skip, 0=full, N=resume).
//   4. Для каждого нужного att: BlobStore.Get → split на 64KB raw чанки →
//      base64-encode → отправить attachment_chunk через SendDataThrottled.
//   5. После последнего чанка одного att → отправить attachment_complete с sha256.
//   6. После всех файлов → пометить delivered=1, emit vox:attach-complete.
//
// Receiver flow (handleAttachment*):
//   - attachment_offer: создать assembly, открыть .partial файлы для НЕ-dedup,
//     отправить attachment_offer_ack.
//   - attachment_chunk: append к нужному .partial, обновить running sha256.
//   - attachment_complete: проверить sha256 → atomic-rename в финальный путь
//     через BlobStore → когда ВСЕ atts собраны: INSERT message + message_attachments,
//     emit vox:message-received.
//   - attachment_cancel: удалить .partial, очистить assembly.

const (
	// attachChunkSize — размер raw данных одного чанка (legacy JSON-путь).
	// 64KB raw → ~85KB base64 → ~85KB JSON. Согласовано с cas.DefaultChunkSize=64KB.
	// Выбор обоснован в DefaultChunkSize: smooth interleaving на attachments DC.
	attachChunkSize = 64 * 1024

	// progressEmitMinDelta — минимум байт между двумя emit'ами vox:attach-progress.
	// Подбор: 1 MiB при chunk=128 KiB даёт эмит каждые ~8 чанков, что при P2P-direct
	// ~10 MB/s = ~10 событий/сек на бабл — UI успевает перерисовать без throttling.
	// На медленном TURN (0.5 MB/s) это ~2 события/сек — тоже ок.
	progressEmitMinDelta = 1024 * 1024

	// progressLogEveryChunks — backend логирует прогресс раз в N чанков.
	// Видим продвижение в vox-app.log.
	progressLogEveryChunks = 16

	// attachOfferAckTimeout — сколько ждать ack от receiver'а перед фейлом.
	attachOfferAckTimeout = 30 * time.Second

	// chunkSendMaxRetries — сколько раз ретраим один chunk при таймауте throttle.
	// 12 × 5s = 1 минута на чанк. Если TURN совсем умер — выходим.
	chunkSendMaxRetries = 12

	// attachAssemblyInactivityTTL — receiver удаляет assemblies, которые не
	// получили ни одного чанка за это время. Отсчитывается от lastChunkAt
	// (а не createdAt) — иначе 22-часовая загрузка 40 GB через TURN зачищается
	// посередине. 5 минут — достаточно чтобы пережить кратковременный обрыв
	// и переподключение P2P, не оставляя зависшие tmp-файлы навечно.
	attachAssemblyInactivityTTL = 5 * time.Minute

	// attachApproveThreshold — выше этого суммарного размера offer'а receiver
	// сначала спрашивает пользователя. Меньше — авто-приём (фото до ~10 MB,
	// мелкие файлы не тратят время пользователя на подтверждение).
	attachApproveThreshold = 10 * 1024 * 1024 // 10 MB
)

// --- Sender state ---

// attachSendState — per (peerID, msgID) состояние отправки.
type attachSendState struct {
	peerID    string
	msgID     string
	startedAt time.Time
	cancelCh  chan struct{}
	ackCh     chan AttachmentOfferAckPayload // буфер 1, ack ровно один
}

// initAttachState инициализируется лениво при первом обращении.
// Карты: msgID → state. Доступ только под mu.
func (s *MessagingService) ensureAttachMaps() {
	s.attachMu.Lock()
	defer s.attachMu.Unlock()
	if s.sendStates == nil {
		s.sendStates = make(map[string]*attachSendState)
	}
	if s.recvAssemblies == nil {
		s.recvAssemblies = make(map[string]*attachRecvAssembly)
	}
	if s.peerSendMu == nil {
		s.peerSendMu = make(map[string]*sync.Mutex)
	}
}

// peerSendMutex возвращает per-peer мьютекс для serialize'а sender'ов.
// Создаёт лениво при первом обращении.
func (s *MessagingService) peerSendMutex(peerID string) *sync.Mutex {
	s.attachMu.Lock()
	defer s.attachMu.Unlock()
	if s.peerSendMu == nil {
		s.peerSendMu = make(map[string]*sync.Mutex)
	}
	mu, ok := s.peerSendMu[peerID]
	if !ok {
		mu = &sync.Mutex{}
		s.peerSendMu[peerID] = mu
	}
	return mu
}

// --- Receiver state ---

// attachRecvAssembly: поток decrypt/write per att идёт напрямую через
// BlobStore.Writer — расшифрованные chunks стримятся на диск сразу в
// encrypted формате, sha считается writer'ом. Никаких .partial + ReadFile
// в RAM (это был OOM-риск для >100 MB файлов).
type attachRecvAssembly struct {
	mu          sync.Mutex
	peerID      string
	msgID       string
	groupID     string // непустой для group-attachment
	caption     string
	offer       AttachmentOfferPayload
	writers     []*storage.BlobWriter // один per att; nil если dedup
	written     []int64               // plaintext bytes записано per att (для диагностики)
	completed   []bool                // флаг "комплитнулся" per att
	createdAt   time.Time
	lastChunkAt time.Time // обновляется на каждом chunk; gc смотрит на эту метку
	totalBytes  int64     // сумма всех att.Size
	awaitingApproval bool // true пока пользователь не нажал Принять/Отклонить
}

// --- Public-ish entry point: вызывается из SendAttachments после INSERT ---

// sendAttachmentsToPeer запускается в горутине. Один экземпляр на (peerID, msgID).
// Если peer не подключён — early-return; повторная попытка случится в DeliverPendingMessages.
//
// Serialize per-peer: несколько sender'ов в одного пира выполняются последовательно
// (через peerSendMutex). Без этого они делят bandwidth TURN и каждый идёт в N раз
// медленнее. С serialize: первый файл уходит быстро, второй сразу за ним.
func (s *MessagingService) sendAttachmentsToPeer(peerID, msgID, caption string) {
	s.ensureAttachMaps()

	if !s.network.IsP2PConnected(peerID) {
		s.log.Debug("attach send skipped: peer offline",
			slog.String("peer", safeShort(peerID)),
			slog.String("msg_id", msgID))
		return
	}

	// Per-peer mutex: ждём пока другие attachment-sender'ы в этого пира закончат.
	// Запоминаем acquire-time для диагностики.
	peerMu := s.peerSendMutex(peerID)
	queueStart := time.Now()
	peerMu.Lock()
	defer peerMu.Unlock()
	if waited := time.Since(queueStart); waited > 100*time.Millisecond {
		s.log.Info("attach: waited in queue",
			slog.String("peer", safeShort(peerID)),
			slog.String("msg_id", msgID),
			slog.Duration("waited", waited))
	}

	// Re-check connection after acquiring lock — peer мог отвалиться пока ждали.
	if !s.network.IsP2PConnected(peerID) {
		s.log.Debug("attach send skipped after queue: peer offline",
			slog.String("peer", safeShort(peerID)),
			slog.String("msg_id", msgID))
		return
	}

	// Загрузить attachments-meta из БД (порядок по ord). Это и offer, и source-of-truth.
	atts, names, kinds, err := s.loadAttachmentsForSend(msgID)
	if err != nil || len(atts) == 0 {
		s.log.Warn("attach send: load failed",
			slog.String("msg_id", msgID),
			slog.Any("err", err))
		return
	}

	totalBytes := int64(0)
	for _, a := range atts {
		totalBytes += a.Size
	}

	// Регистрируем send state. Если уже есть — значит уже летит, ничего не делаем.
	state := &attachSendState{
		peerID:    peerID,
		msgID:     msgID,
		startedAt: time.Now(),
		cancelCh:  make(chan struct{}),
		ackCh:     make(chan AttachmentOfferAckPayload, 1),
	}
	stateKey := peerMsgKey(peerID, msgID)
	s.attachMu.Lock()
	if _, exists := s.sendStates[stateKey]; exists {
		s.attachMu.Unlock()
		s.log.Debug("attach send: already in flight", slog.String("msg_id", msgID), slog.String("peer", safeShort(peerID)))
		return
	}
	s.sendStates[stateKey] = state
	s.attachMu.Unlock()

	defer func() {
		s.attachMu.Lock()
		delete(s.sendStates, stateKey)
		s.attachMu.Unlock()
	}()

	myID, _ := s.sess.PeerID()
	_ = myID // зарезервировано для будущих логов

	// 1. Send offer.
	offerPayload := AttachmentOfferPayload{
		MsgID:   msgID,
		Caption: caption,
		Atts:    atts,
		Names:   names,
		Kinds:   kinds,
	}
	offerJSON, err := json.Marshal(offerPayload)
	if err != nil {
		s.log.Error("attach offer marshal", slog.String("err", err.Error()))
		return
	}
	offerMsg := P2PMessage{
		Type:    "attachment_offer",
		ID:      msgID,
		GroupID: s.lookupGroupIDForMsg(msgID),
		Content: string(offerJSON),
		Time:    time.Now().Unix(),
	}
	if data := s.marshalP2P(offerMsg); data != nil {
		if err := s.network.SendP2PData(peerID, data); err != nil {
			s.log.Warn("attach offer send", slog.String("peer", safeShort(peerID)), slog.String("err", err.Error()))
			s.emitAttachFailed(msgID, peerID, "offer send failed")
			return
		}
	}

	// 2. Wait for ack.
	var ack AttachmentOfferAckPayload
	select {
	case ack = <-state.ackCh:
	case <-state.cancelCh:
		s.log.Info("attach send cancelled before ack", slog.String("msg_id", msgID))
		return
	case <-time.After(attachOfferAckTimeout):
		s.log.Warn("attach offer ack timeout", slog.String("msg_id", msgID), slog.String("peer", safeShort(peerID)))
		s.emitAttachFailed(msgID, peerID, "offer ack timeout")
		return
	}

	if len(ack.Offsets) != len(atts) {
		s.log.Warn("attach ack: offsets mismatch", slog.String("msg_id", msgID), slog.Int("expected", len(atts)), slog.Int("got", len(ack.Offsets)))
		s.emitAttachFailed(msgID, peerID, "offsets mismatch")
		return
	}

	// 3. Stream chunks per att.
	// Выбор транспорта: если у обеих сторон открыт бинарный "attachments" DC,
	// гонят чанки бинарём (без base64-overhead, без JSON-marshal, отдельный
	// SCTP канал — не блокирует "messages"). Иначе — legacy base64 через
	// "messages" DC (для старых клиентов в переходный период).
	useBinaryAttachDC := s.network.IsAttachDCOpen(peerID)
	if useBinaryAttachDC {
		s.log.Info("attach: using binary DC",
			slog.String("msg_id", msgID),
			slog.String("peer", safeShort(peerID)))
	} else {
		s.log.Info("attach: using legacy base64 (messages DC)",
			slog.String("msg_id", msgID),
			slog.String("peer", safeShort(peerID)))
	}
	sentBytes := int64(0)
	lastEmitBytes := int64(0)
	emitThreshold := int64(progressEmitMinDelta)

	for attIdx, meta := range atts {
		off := ack.Offsets[attIdx]
		if off < 0 {
			// Dedup: receiver уже имеет blob. Шлём complete тем же транспортом
			// что выбран для chunks — для бинарного DC порядок относительно
			// предыдущих chunk-фреймов гарантирован SCTP.
			s.log.Info("attach dedup", slog.String("msg_id", msgID), slog.Int("att", attIdx), slog.String("sha", safeShort8(meta.Sha256)))
			s.sendAttachComplete(peerID, msgID, attIdx, meta.Sha256, useBinaryAttachDC)
			sentBytes += meta.Size
			continue
		}

		// Streaming read: держим в RAM только один chunk (+ его base64). OpenReader
		// расшифровывает on-the-fly — большие файлы больше не грузят память.
		rdr, err := s.blobs.OpenReader(meta.Sha256)
		if err != nil {
			s.log.Error("attach: open blob failed", slog.String("sha", safeShort8(meta.Sha256)), slog.String("err", err.Error()))
			s.emitAttachFailed(msgID, peerID, "blob open failed")
			return
		}

		// Resume: пропускаем первые off байт plaintext. Seek по chunked-формату
		// невозможен (каждый chunk независимо зашифрован), поэтому пропуск —
		// через io.CopyN в io.Discard. Медленно, но кейс редкий.
		if off > 0 {
			if _, err := io.CopyN(io.Discard, rdr, off); err != nil {
				_ = rdr.Close()
				s.log.Error("attach: resume skip failed", slog.String("err", err.Error()))
				s.emitAttachFailed(msgID, peerID, "blob resume failed")
				return
			}
		}

		totalChunks := int((meta.Size + int64(attachChunkSize) - 1) / int64(attachChunkSize))
		startChunk := int(off / int64(attachChunkSize))
		chunkBuf := make([]byte, attachChunkSize)
		sendErrorHit := false
		for chunkIdx := startChunk; ; chunkIdx++ {
			select {
			case <-state.cancelCh:
				s.log.Info("attach send cancelled mid-stream", slog.String("msg_id", msgID), slog.Int("att", attIdx), slog.Int("chunk", chunkIdx))
				_ = rdr.Close()
				return
			default:
			}

			n, rerr := io.ReadFull(rdr, chunkBuf)
			if rerr == io.EOF {
				break
			}
			if rerr != nil && rerr != io.ErrUnexpectedEOF {
				_ = rdr.Close()
				s.log.Error("attach: blob read chunk", slog.String("err", rerr.Error()))
				s.emitAttachFailed(msgID, peerID, "blob read failed")
				return
			}

			// Подготовка фрейма: бинарь напрямую vs JSON+base64. Sender одинаков
			// для обоих — отличается только encoding и канал отправки.
			var (
				data    []byte
				sendErr error
				marshalErr error
			)
			if useBinaryAttachDC {
				data, marshalErr = encodeAttachChunk(msgID, attIdx, chunkIdx, totalChunks, chunkBuf[:n])
				if marshalErr != nil {
					s.log.Error("attach binary frame encode",
						slog.String("msg_id", msgID),
						slog.String("err", marshalErr.Error()))
					_ = rdr.Close()
					s.emitAttachFailed(msgID, peerID, "frame encode failed")
					return
				}
			} else {
				chunkData := base64.StdEncoding.EncodeToString(chunkBuf[:n])
				chunkMsg := P2PMessage{
					Type:    "attachment_chunk",
					ID:      msgID,
					Att:     attIdx,
					Idx:     chunkIdx,
					Total:   totalChunks,
					Content: chunkData,
				}
				data = s.marshalP2P(chunkMsg)
				if data == nil {
					if rerr == io.ErrUnexpectedEOF {
						break
					}
					continue
				}
			}
			// Retry имеет смысл ТОЛЬКО на throttle-timeout (SCTP буфер переполнен):
			// TURN буферизует долго, один 5-секундный timeout не повод бросать transfer.
			// Любые другие ошибки (DC closed, peer disappeared, SCTP send) — fatal,
			// ретрай их не вылечит, только задержит пользовательский feedback на минуту.
			for retry := 0; retry < chunkSendMaxRetries; retry++ {
				select {
				case <-state.cancelCh:
					_ = rdr.Close()
					return
				default:
				}
				if useBinaryAttachDC {
					sendErr = s.network.SendAttachChunk(peerID, data)
				} else {
					sendErr = s.network.SendP2PDataThrottled(peerID, data)
				}
				if sendErr == nil {
					break
				}
				if !isThrottleTimeout(sendErr) {
					break // fatal — не ретраим
				}
				s.log.Debug("attach chunk throttle-wait, retrying",
					slog.String("msg_id", msgID),
					slog.Int("att", attIdx),
					slog.Int("chunk", chunkIdx),
					slog.Int("retry", retry+1))
			}
			if sendErr != nil {
				s.log.Warn("attach chunk send FAILED",
					slog.String("msg_id", msgID),
					slog.Int("att", attIdx),
					slog.Int("chunk", chunkIdx),
					slog.Bool("throttle", isThrottleTimeout(sendErr)),
					slog.String("err", sendErr.Error()))
				s.emitAttachFailed(msgID, peerID, "chunk send failed: "+sendErr.Error())
				sendErrorHit = true
				break
			}

			sentBytes += int64(n)
			if sentBytes-lastEmitBytes >= emitThreshold {
				s.emitAttachProgress(msgID, peerID, sentBytes, totalBytes)
				lastEmitBytes = sentBytes
			}
			if (chunkIdx+1)%progressLogEveryChunks == 0 || rerr == io.ErrUnexpectedEOF {
				s.log.Info("attach progress",
					slog.String("msg_id", msgID),
					slog.String("peer", safeShort(peerID)),
					slog.Int("att", attIdx),
					slog.Int("chunk", chunkIdx+1),
					slog.Int("of", totalChunks),
					slog.Int64("sent", sentBytes),
					slog.Int64("total", totalBytes))
			}
			if rerr == io.ErrUnexpectedEOF {
				break // это был последний неполный chunk
			}
		}
		_ = rdr.Close()
		if sendErrorHit {
			return
		}

		// Per-att complete с sha256.
		s.sendAttachComplete(peerID, msgID, attIdx, meta.Sha256, useBinaryAttachDC)
		s.log.Debug("attach part sent",
			slog.String("msg_id", msgID),
			slog.Int("att", attIdx),
			slog.Int64("bytes", meta.Size))
	}

	// Финальный progress event (точно 100%).
	s.emitAttachProgress(msgID, peerID, totalBytes, totalBytes)

	// 4. Mark delivered.
	if isGroupMsg, _ := s.isGroupMessage(msgID); isGroupMsg {
		if _, err := s.store.DB().Exec(
			"UPDATE group_message_delivery SET delivered = 1 WHERE msg_id = ? AND peer_id = ?",
			msgID, peerID); err != nil {
			s.log.Warn("update group delivery", slog.String("err", err.Error()))
		}
	} else {
		if _, err := s.store.DB().Exec(
			"UPDATE messages SET delivered = 1 WHERE id = ?", msgID); err != nil {
			s.log.Warn("update delivered", slog.String("err", err.Error()))
		}
		s.network.emitEvent("vox:messages-delivered", map[string]string{"peerId": peerID})
	}

	s.emitAttachComplete(msgID, peerID)
	s.log.Info("📎 ATTACH SENT",
		slog.String("peer", safeShort(peerID)),
		slog.String("msg_id", msgID),
		slog.Int("count", len(atts)),
		slog.Int64("bytes", totalBytes),
		slog.Duration("dur", time.Since(state.startedAt)))
}

// --- Receiver handlers ---

// handleAttachmentOffer: receiver получает анонс. Создаём assembly, проверяем
// dedup для каждого sha256, открываем .partial для не-dedup, отвечаем offer_ack.
func (s *MessagingService) handleAttachmentOffer(fromPubKey string, msg *P2PMessage) {
	s.ensureAttachMaps()
	if s.blobs == nil {
		return
	}

	var payload AttachmentOfferPayload
	if err := json.Unmarshal([]byte(msg.Content), &payload); err != nil {
		s.log.Warn("attach offer unmarshal", slog.String("err", err.Error()))
		return
	}
	if payload.MsgID == "" || len(payload.Atts) == 0 {
		return
	}
	// Защита от битого/злого отправителя: все три slice-поля должны быть равной
	// длины, иначе sendOfferAck/finalize получат index-out-of-range.
	if len(payload.Names) != len(payload.Atts) || len(payload.Kinds) != len(payload.Atts) {
		s.log.Warn("attach offer: array length mismatch — rejecting",
			slog.String("peer", safeShort(fromPubKey)),
			slog.String("msg_id", payload.MsgID),
			slog.Int("atts", len(payload.Atts)),
			slog.Int("names", len(payload.Names)),
			slog.Int("kinds", len(payload.Kinds)))
		return
	}
	// Sanity: каждый att должен иметь непустой sha256 (hex) и положительный size.
	for i, a := range payload.Atts {
		if a.Sha256 == "" || a.Size <= 0 {
			s.log.Warn("attach offer: invalid att entry",
				slog.String("peer", safeShort(fromPubKey)),
				slog.String("msg_id", payload.MsgID),
				slog.Int("att", i))
			return
		}
	}

	// Защита от повторного offer'а на тот же msgID — приветствуем переподключения.
	asmKey := peerMsgKey(fromPubKey, payload.MsgID)
	s.attachMu.Lock()
	if existing, ok := s.recvAssemblies[asmKey]; ok {
		s.attachMu.Unlock()
		s.log.Debug("attach offer: duplicate, re-sending ack",
			slog.String("peer", safeShort(fromPubKey)),
			slog.String("msg_id", payload.MsgID))
		// Re-sending ack (may have been lost).
		s.sendOfferAck(fromPubKey, existing)
		return
	}
	s.attachMu.Unlock()

	// Подготавливаем assembly. Для не-dedup слотов открываем BlobWriter —
	// расшифрованные чанки пойдут сразу в encrypted blob-формат, sha считает writer.
	asm := &attachRecvAssembly{
		peerID:    fromPubKey,
		msgID:     payload.MsgID,
		groupID:   msg.GroupID,
		caption:   payload.Caption,
		offer:     payload,
		writers:   make([]*storage.BlobWriter, len(payload.Atts)),
		written:   make([]int64, len(payload.Atts)),
		completed: make([]bool, len(payload.Atts)),
		createdAt: time.Now(),
	}

	for i, att := range payload.Atts {
		asm.totalBytes += att.Size
		// Dedup: принимаем только если И row в attachments есть, И blob физически
		// на диске. Иначе (удалили файл вручную, crash посреди Delete) sender
		// на этом файле остановится и мы никогда не получим его содержимое.
		var existing string
		err := s.store.DB().QueryRow(
			"SELECT id FROM attachments WHERE sha256 = ?", att.Sha256).Scan(&existing)
		if err == nil && s.blobs.Exists(att.Sha256) {
			asm.writers[i] = nil // dedup
			continue
		}
		w, werr := s.blobs.NewWriter()
		if werr != nil {
			s.log.Error("attach: blob writer", slog.String("err", werr.Error()))
			s.cleanupAssembly(asm)
			return
		}
		asm.writers[i] = w
	}

	// Approval gate: нужно ли спрашивать пользователя?
	// Считаем только non-dedup байты — если всё уже на диске, спрашивать не о чем.
	pendingBytes := int64(0)
	for i, att := range payload.Atts {
		if asm.writers[i] != nil {
			pendingBytes += att.Size
		}
	}
	needsApproval := pendingBytes > attachApproveThreshold

	s.attachMu.Lock()
	asm.awaitingApproval = needsApproval
	s.recvAssemblies[asmKey] = asm
	s.attachMu.Unlock()

	if needsApproval {
		s.log.Info("📥 ATTACH OFFER (awaiting approval)",
			slog.String("peer", safeShort(fromPubKey)),
			slog.String("msg_id", payload.MsgID),
			slog.Int("atts", len(payload.Atts)),
			slog.Int64("bytes", asm.totalBytes),
			slog.Int64("pending", pendingBytes))
		s.emitAttachNeedsApproval(asm)
		return
	}

	s.sendOfferAck(fromPubKey, asm)
	s.log.Info("📥 ATTACH OFFER",
		slog.String("peer", safeShort(fromPubKey)),
		slog.String("msg_id", payload.MsgID),
		slog.Int("atts", len(payload.Atts)),
		slog.Int64("bytes", asm.totalBytes))
}

// emitAttachNeedsApproval — эмит event'а для frontend-модалки.
// Frontend показывает Accept/Decline, потом зовёт ApproveAttachment / DeclineAttachment.
func (s *MessagingService) emitAttachNeedsApproval(asm *attachRecvAssembly) {
	files := make([]map[string]any, 0, len(asm.offer.Atts))
	for i, att := range asm.offer.Atts {
		dedup := asm.writers[i] == nil
		name := ""
		kind := ""
		if i < len(asm.offer.Names) {
			name = asm.offer.Names[i]
		}
		if i < len(asm.offer.Kinds) {
			kind = asm.offer.Kinds[i]
		}
		files = append(files, map[string]any{
			"name":  name,
			"kind":  kind,
			"mime":  att.Mime,
			"size":  att.Size,
			"dedup": dedup,
		})
	}
	s.network.emitEvent("vox:attach-needs-approval", map[string]any{
		"msgId":      asm.msgID,
		"peerId":     asm.peerID,
		"groupId":    asm.groupID,
		"totalBytes": asm.totalBytes,
		"caption":    asm.caption,
		"files":      files,
	})
}

// ApproveAttachment — Wails-метод, вызывается фронтом по Accept в модалке.
// Снимает флаг awaitingApproval и шлёт ack — sender начинает грузить чанки.
func (s *MessagingService) ApproveAttachment(peerID, msgID string) error {
	asmKey := peerMsgKey(peerID, msgID)
	s.attachMu.Lock()
	asm, ok := s.recvAssemblies[asmKey]
	if ok {
		asm.awaitingApproval = false
	}
	s.attachMu.Unlock()
	if !ok {
		return fmt.Errorf("ApproveAttachment: assembly not found for %s", safeShort8(msgID))
	}
	s.sendOfferAck(peerID, asm)
	s.log.Info("attach: user APPROVED",
		slog.String("peer", safeShort(peerID)),
		slog.String("msg_id", msgID),
		slog.Int64("bytes", asm.totalBytes))
	return nil
}

// DeclineAttachment — Wails-метод, вызывается фронтом по Decline в модалке.
// Cleanup'ает assembly локально + шлёт sender'у attachment_decline (тот закроет
// своё send state и эмитнет attach-failed на своей стороне).
func (s *MessagingService) DeclineAttachment(peerID, msgID string) error {
	asmKey := peerMsgKey(peerID, msgID)
	s.attachMu.Lock()
	asm, ok := s.recvAssemblies[asmKey]
	if ok {
		delete(s.recvAssemblies, asmKey)
	}
	s.attachMu.Unlock()
	if !ok {
		return fmt.Errorf("DeclineAttachment: assembly not found for %s", safeShort8(msgID))
	}
	s.cleanupAssembly(asm)

	declineMsg := P2PMessage{
		Type: "attachment_decline",
		ID:   msgID,
	}
	if data := s.marshalP2P(declineMsg); data != nil {
		_ = s.network.SendP2PData(peerID, data)
	}
	s.log.Info("attach: user DECLINED",
		slog.String("peer", safeShort(peerID)),
		slog.String("msg_id", msgID),
		slog.Int64("bytes", asm.totalBytes))
	return nil
}

// sendOfferAck отвечает sender'у со списком offset'ов (-1 = dedup).
// Phase 3: всегда 0 для нужных, -1 для dedup. Resume offsets — Phase 6.
func (s *MessagingService) sendOfferAck(peerID string, asm *attachRecvAssembly) {
	offsets := make([]int64, len(asm.offer.Atts))
	for i := range asm.offer.Atts {
		if asm.writers[i] == nil {
			offsets[i] = -1
		} else {
			offsets[i] = 0
		}
	}
	ackPayload := AttachmentOfferAckPayload{MsgID: asm.msgID, Offsets: offsets}
	body, _ := json.Marshal(ackPayload)
	ackMsg := P2PMessage{
		Type:    "attachment_offer_ack",
		ID:      asm.msgID,
		Content: string(body),
	}
	if data := s.marshalP2P(ackMsg); data != nil {
		_ = s.network.SendP2PData(peerID, data)
	}
}

// handleAttachmentOfferAck: sender получает offsets, разблокирует свою goroutine.
func (s *MessagingService) handleAttachmentOfferAck(fromPubKey string, msg *P2PMessage) {
	s.ensureAttachMaps()
	var ack AttachmentOfferAckPayload
	if err := json.Unmarshal([]byte(msg.Content), &ack); err != nil {
		return
	}
	stateKey := peerMsgKey(fromPubKey, ack.MsgID)
	s.attachMu.Lock()
	state, ok := s.sendStates[stateKey]
	s.attachMu.Unlock()
	if !ok {
		s.log.Debug("attach ack: no send state",
			slog.String("peer", safeShort(fromPubKey)),
			slog.String("msg_id", ack.MsgID))
		return
	}
	select {
	case state.ackCh <- ack:
	default:
		// Каждый ackCh ловит только первый ack — повторные игнорируем.
	}
}

// handleAttachmentChunk: legacy-обёртка над handleAttachmentChunkBytes для
// JSON-фреймов через "messages" DC (base64-payload в msg.Content). Используется
// при общении со старыми клиентами, у которых ещё нет бинарного DC.
func (s *MessagingService) handleAttachmentChunk(fromPubKey string, msg *P2PMessage) {
	chunk, err := base64.StdEncoding.DecodeString(msg.Content)
	if err != nil {
		s.log.Warn("attach chunk: bad base64", slog.String("msg_id", msg.ID), slog.String("err", err.Error()))
		return
	}
	s.handleAttachmentChunkBytes(fromPubKey, msg.ID, msg.Att, chunk)
}

// handleAttachmentChunkBytes — общая логика приёма chunk'а независимо от
// транспорта. Стримит payload прямо в BlobStore.Writer; writer сам считает
// sha256 и пишет encrypted chunks в tmp-файл.
func (s *MessagingService) handleAttachmentChunkBytes(fromPubKey, msgID string, attIdx int, chunk []byte) {
	asmKey := peerMsgKey(fromPubKey, msgID)
	s.attachMu.Lock()
	asm, ok := s.recvAssemblies[asmKey]
	s.attachMu.Unlock()
	if !ok {
		return
	}
	if attIdx < 0 || attIdx >= len(asm.writers) {
		return
	}

	// nil-check ВНУТРИ asm.mu: handleAttachmentComplete может обнулить writer
	// между нашей проверкой и Write'ом — особенно если complete пришёл по другому
	// каналу ("messages" DC), а chunk по бинарному ("attachments" DC). Без этого
	// замка получали nil pointer dereference в BlobWriter.Write.
	asm.mu.Lock()
	if asm.writers[attIdx] == nil {
		// Dedup-slot или уже финализированный att — чанки игнорируем.
		asm.mu.Unlock()
		return
	}
	asm.lastChunkAt = time.Now()
	// Cap: не принимать больше объявленного att.Size. Sha проверяется только на
	// Finalize — без этой границы sender мог слить сколько угодно байт на диск
	// до провала проверки. Обрываем при первом превышении.
	if attIdx < len(asm.offer.Atts) {
		declared := asm.offer.Atts[attIdx].Size
		if asm.written[attIdx]+int64(len(chunk)) > declared {
			asm.mu.Unlock()
			s.log.Warn("attach chunk: превышен объявленный размер, обрыв",
				slog.String("msg_id", msgID),
				slog.Int("att", attIdx),
				slog.Int64("declared", declared),
				slog.String("peer", safeShort(fromPubKey)))
			s.attachMu.Lock()
			delete(s.recvAssemblies, peerMsgKey(fromPubKey, msgID))
			s.attachMu.Unlock()
			s.cleanupAssembly(asm)
			s.emitAttachFailed(msgID, fromPubKey, "receiver: size cap exceeded")
			return
		}
	}
	if _, err := asm.writers[attIdx].Write(chunk); err != nil {
		// Write fail обычно = disk full / permission. Иначе sender досылал
		// complete и считал что доставил, а receiver терял пакет тихо. Здесь явно
		// выбиваем assembly + emit failed → следующие chunks игнорируются (asm уже нет).
		asm.mu.Unlock()
		s.log.Error("attach chunk write — aborting transfer",
			slog.String("msg_id", msgID),
			slog.Int("att", attIdx),
			slog.String("peer", safeShort(fromPubKey)),
			slog.String("err", err.Error()))
		s.attachMu.Lock()
		delete(s.recvAssemblies, peerMsgKey(fromPubKey, msgID))
		s.attachMu.Unlock()
		s.cleanupAssembly(asm)
		s.emitAttachFailed(msgID, fromPubKey, "receiver write error: "+err.Error())
		return
	}
	asm.written[attIdx] += int64(len(chunk))
	asm.mu.Unlock()
}

// handleAttachmentComplete: проверить sha, atomic-rename в BlobStore.
// Когда ВСЕ atts complete → INSERT message + message_attachments + emit message-received.
func (s *MessagingService) handleAttachmentComplete(fromPubKey string, msg *P2PMessage) {
	asmKey := peerMsgKey(fromPubKey, msg.ID)
	s.attachMu.Lock()
	asm, ok := s.recvAssemblies[asmKey]
	s.attachMu.Unlock()
	if !ok {
		return
	}
	if msg.Att < 0 || msg.Att >= len(asm.writers) {
		return
	}

	asm.mu.Lock()
	defer asm.mu.Unlock()

	if asm.completed[msg.Att] {
		return // повтор
	}

	// Dedup-slot: writer=nil, нечего финализировать — отмечаем complete.
	if asm.writers[msg.Att] == nil {
		if asm.offer.Atts[msg.Att].Sha256 != msg.Sha {
			s.log.Warn("attach complete: dedup sha mismatch", slog.String("msg_id", msg.ID))
		}
		asm.completed[msg.Att] = true
	} else {
		w := asm.writers[msg.Att]
		asm.writers[msg.Att] = nil // чтобы повторный complete не финализировал дважды

		sha, err := w.Finalize()
		if err != nil {
			s.log.Error("attach complete: finalize",
				slog.String("msg_id", msg.ID),
				slog.Int("att", msg.Att),
				slog.String("err", err.Error()))
			s.emitAttachFailed(msg.ID, fromPubKey, "blob finalize failed")
			return
		}
		if sha != msg.Sha {
			s.log.Warn("attach complete: SHA MISMATCH",
				slog.String("msg_id", msg.ID),
				slog.Int("att", msg.Att),
				slog.String("expect", safeShort8(msg.Sha)),
				slog.String("got", safeShort8(sha)))
			// Finalize уже сделал rename — удаляем канонический файл, чтобы не
			// остался "битый" blob под чужим sha (наш sha не совпадает с обещанным).
			_ = s.blobs.Delete(sha)
			s.emitAttachFailed(msg.ID, fromPubKey, "sha256 mismatch")
			return
		}
		asm.completed[msg.Att] = true
	}

	// Все ли собраны?
	allDone := true
	for _, c := range asm.completed {
		if !c {
			allDone = false
			break
		}
	}
	if !allDone {
		return
	}

	// Финализация: вставить attachments-rows, message + message_attachments.
	if err := s.finalizeReceivedAttachments(asm); err != nil {
		s.log.Error("attach finalize", slog.String("err", err.Error()))
		s.emitAttachFailed(msg.ID, fromPubKey, "finalize failed")
		return
	}

	// Cleanup из карты.
	s.attachMu.Lock()
	delete(s.recvAssemblies, asmKey)
	s.attachMu.Unlock()

	s.emitAttachComplete(msg.ID, fromPubKey)
	s.log.Info("📥 ATTACH RECEIVED",
		slog.String("peer", safeShort(fromPubKey)),
		slog.String("msg_id", msg.ID),
		slog.Int("atts", len(asm.offer.Atts)),
		slog.Int64("bytes", asm.totalBytes))
}

// handleAttachmentDecline — sender-side обработка отказа receiver'а
// (тот нажал «Отклонить» в approval-модалке). Закрываем send state, эмитим
// failed для UI чтобы убрать spinner и показать toast.
func (s *MessagingService) handleAttachmentDecline(fromPubKey string, msg *P2PMessage) {
	stateKey := peerMsgKey(fromPubKey, msg.ID)
	s.attachMu.Lock()
	state, ok := s.sendStates[stateKey]
	if ok {
		delete(s.sendStates, stateKey)
	}
	s.attachMu.Unlock()
	if !ok {
		return
	}
	// Cancel чтобы текущая горутина sender'а вышла из ожидания ack/throttle.
	close(state.cancelCh)
	s.emitAttachFailed(msg.ID, fromPubKey, "declined by recipient")
	s.log.Info("attach: peer DECLINED our send",
		slog.String("peer", safeShort(fromPubKey)),
		slog.String("msg_id", msg.ID))
}

// handleAttachmentCancel: receiver-side cleanup .partial и assembly.
func (s *MessagingService) handleAttachmentCancel(fromPubKey string, msg *P2PMessage) {
	asmKey := peerMsgKey(fromPubKey, msg.ID)
	s.attachMu.Lock()
	asm, ok := s.recvAssemblies[asmKey]
	if ok {
		delete(s.recvAssemblies, asmKey)
	}
	s.attachMu.Unlock()
	if !ok {
		return
	}
	s.cleanupAssembly(asm)
	s.emitAttachFailed(msg.ID, fromPubKey, "cancelled by sender")
	s.log.Info("attach cancelled by sender", slog.String("msg_id", msg.ID), slog.String("peer", safeShort(fromPubKey)))
}

// --- Internal helpers ---

func (s *MessagingService) loadAttachmentsForSend(msgID string) ([]AttachmentMeta, []string, []string, error) {
	rows, err := s.store.DB().Query(
		`SELECT a.id, ma.kind, ma.name, a.mime, a.size, a.sha256, a.width, a.height
		 FROM message_attachments ma JOIN attachments a ON a.id = ma.attachment_id
		 WHERE ma.message_id = ? OR ma.group_msg_id = ?
		 ORDER BY ma.ord ASC`, msgID, msgID)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()
	var atts []AttachmentMeta
	var names, kinds []string
	for rows.Next() {
		var m AttachmentMeta
		var kind, name string
		if err := rows.Scan(&m.ID, &kind, &name, &m.Mime, &m.Size, &m.Sha256, &m.Width, &m.Height); err != nil {
			return nil, nil, nil, err
		}
		m.Name = name
		m.Kind = kind
		atts = append(atts, m)
		names = append(names, name)
		kinds = append(kinds, kind)
	}
	return atts, names, kinds, rows.Err()
}

func (s *MessagingService) lookupGroupIDForMsg(msgID string) string {
	var gid string
	_ = s.store.DB().QueryRow("SELECT group_id FROM group_messages WHERE id = ?", msgID).Scan(&gid)
	return gid
}

func (s *MessagingService) isGroupMessage(msgID string) (bool, string) {
	var gid string
	err := s.store.DB().QueryRow("SELECT group_id FROM group_messages WHERE id = ?", msgID).Scan(&gid)
	if err == nil {
		return true, gid
	}
	return false, ""
}

// cleanupAssembly завершает активные BlobWriter'ы (удаляет их tmp-файлы).
// Используется при отмене, GC stale assemblies, ошибках открытия.
func (s *MessagingService) cleanupAssembly(asm *attachRecvAssembly) {
	for i, w := range asm.writers {
		if w == nil {
			continue
		}
		if err := w.Abort(); err != nil {
			s.log.Debug("attach: writer abort",
				slog.Int("att", i),
				slog.String("err", err.Error()))
		}
		asm.writers[i] = nil
	}
}

// finalizeReceivedAttachments: вставить attachments-rows (с дедупом по sha),
// message-row (DM или group), и message_attachments-связи. Под транзакцией.
func (s *MessagingService) finalizeReceivedAttachments(asm *attachRecvAssembly) error {
	const op = "MessagingService.finalizeReceivedAttachments"
	now := time.Now().Unix()

	tx, err := s.store.DB().Begin()
	if err != nil {
		return fmt.Errorf("%s: begin: %w", op, err)
	}
	defer tx.Rollback()

	// 1. INSERT message (DM или group) с type='attachment'. INSERT OR IGNORE
	//    на случай если sender уже доставил message_id раньше через другой путь
	//    (re-offer после reconnect, дубликат delivery). Если RowsAffected==0
	//    значит сообщение уже было полностью обработано — message_attachments
	//    уже привязаны, refcount уже инкрементирован. Повторный link дал бы
	//    дублирующиеся row'ы (UNIQUE constraint отсутствует) и refcount-leak.
	var res sql.Result
	if asm.groupID != "" {
		res, err = tx.Exec(
			`INSERT OR IGNORE INTO group_messages (id, group_id, sender_id, content, type, created_at)
			 VALUES (?, ?, ?, ?, 'attachment', ?)`,
			asm.msgID, asm.groupID, asm.peerID, asm.caption, now)
		if err != nil {
			return fmt.Errorf("%s: insert group_message: %w", op, err)
		}
	} else {
		res, err = tx.Exec(
			`INSERT OR IGNORE INTO messages (id, peer_id, sender_id, content, type, read, delivered, created_at)
			 VALUES (?, ?, ?, ?, 'attachment', 0, 1, ?)`,
			asm.msgID, asm.peerID, asm.peerID, asm.caption, now)
		if err != nil {
			return fmt.Errorf("%s: insert message: %w", op, err)
		}
	}
	rowsAffected, _ := res.RowsAffected()
	if rowsAffected == 0 {
		// Message already exists — duplicate offer. Транзакцию можно отпустить
		// (defer Rollback), эмиссию для UI делаем — на случай если первый
		// vox:message был потерян (UI дедуплицирует по msgId).
		_ = tx.Rollback()
		s.log.Debug("attach finalize: message already exists (duplicate offer)",
			slog.String("msg_id", asm.msgID),
			slog.String("peer", safeShort(asm.peerID)))
		s.emitMessageReadyEventIfNeeded(asm, now)
		return nil
	}

	// 2. Per-att: ensure attachments-row (dedup на sha256), затем link.
	// При первой записи row сразу создаётся с refcount=1 — ссылка от
	// message_attachments, который мы вставим ниже. Для существующей row — UPDATE +1.
	for i, meta := range asm.offer.Atts {
		var attID string
		err := tx.QueryRow("SELECT id FROM attachments WHERE sha256 = ?", meta.Sha256).Scan(&attID)
		if errors.Is(err, sql.ErrNoRows) {
			attID = uuid.New().String()
			if _, err := tx.Exec(
				`INSERT INTO attachments (id, sha256, mime, size, width, height, path, refcount, created_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?)`,
				attID, meta.Sha256, meta.Mime, meta.Size, meta.Width, meta.Height,
				s.blobs.PathRel(meta.Sha256), now); err != nil {
				return fmt.Errorf("%s: insert attachment: %w", op, err)
			}
		} else if err != nil {
			return fmt.Errorf("%s: lookup sha: %w", op, err)
		} else {
			if _, err := tx.Exec(
				"UPDATE attachments SET refcount = refcount + 1 WHERE id = ?", attID); err != nil {
				return fmt.Errorf("%s: refcount++: %w", op, err)
			}
		}

		if asm.groupID != "" {
			if _, err := tx.Exec(
				`INSERT INTO message_attachments (message_id, group_msg_id, attachment_id, ord, name, kind)
				 VALUES (NULL, ?, ?, ?, ?, ?)`,
				asm.msgID, attID, i, asm.offer.Names[i], asm.offer.Kinds[i]); err != nil {
				return fmt.Errorf("%s: link group: %w", op, err)
			}
		} else {
			if _, err := tx.Exec(
				`INSERT INTO message_attachments (message_id, group_msg_id, attachment_id, ord, name, kind)
				 VALUES (?, NULL, ?, ?, ?, ?)`,
				asm.msgID, attID, i, asm.offer.Names[i], asm.offer.Kinds[i]); err != nil {
				return fmt.Errorf("%s: link dm: %w", op, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: commit: %w", op, err)
	}

	s.emitMessageReadyEventIfNeeded(asm, now)
	return nil
}

// emitMessageReadyEventIfNeeded шлёт vox:message / vox:group-message с
// загруженными attachments. Извлечён в helper чтобы вызываться и в happy-path
// (после commit), и в duplicate-offer (когда message уже есть в БД).
//
// КРИТИЧНО: attachments всегда non-nil slice — иначе JSON-сериализация
// даёт null и фронт получает пустой массив → баббл рендерится без
// фотографий/файлов и кажется пустым.
func (s *MessagingService) emitMessageReadyEventIfNeeded(asm *attachRecvAssembly, now int64) {
	loadedAtts := s.LoadAttachmentsForMessage(asm.msgID, ifGroupKind(asm.groupID))
	if loadedAtts == nil {
		loadedAtts = []AttachmentMeta{}
	}
	s.log.Info("📥 ATTACH READY FOR UI",
		slog.String("msg_id", asm.msgID),
		slog.Int("count", len(loadedAtts)),
		slog.Bool("is_group", asm.groupID != ""))
	if asm.groupID == "" {
		s.network.emitEvent("vox:message", map[string]any{
			"id":          asm.msgID,
			"peerId":      asm.peerID,
			"senderId":    asm.peerID,
			"content":     asm.caption,
			"msgType":     "attachment",
			"timestamp":   now * 1000,
			"attachments": loadedAtts,
		})
		return
	}
	var groupName string
	_ = s.store.DB().QueryRow("SELECT name FROM groups WHERE group_id = ?", asm.groupID).Scan(&groupName)
	senderName := s.network.PeerNickname(asm.peerID)
	s.network.emitEvent("vox:group-message", map[string]any{
		"groupId":   asm.groupID,
		"groupName": groupName,
		"message": map[string]any{
			"id":          asm.msgID,
			"groupId":     asm.groupID,
			"senderId":    asm.peerID,
			"senderName":  senderName,
			"content":     asm.caption,
			"msgType":     "attachment",
			"timestamp":   now * 1000,
			"attachments": loadedAtts,
		},
	})
}

func ifGroupKind(groupID string) string {
	if groupID != "" {
		return "group"
	}
	return "dm"
}

// --- Cancel API (Wails-exposed) ---

// CancelAttachmentTransfer прерывает отправку (sender side) и шлёт
// attachment_cancel пиру для очистки на той стороне.
// Если transfer не активен — no-op.
func (s *MessagingService) CancelAttachmentTransfer(msgID string) error {
	cancelled := s.cancelActiveSendersForMsg(msgID)
	for _, peerID := range cancelled {
		// Сообщить пиру что мы прекратили отправку.
		cancelMsg := P2PMessage{Type: "attachment_cancel", ID: msgID}
		if data := s.marshalP2P(cancelMsg); data != nil {
			_ = s.network.SendP2PData(peerID, data)
		}
	}
	if len(cancelled) > 0 {
		s.log.Info("attach cancelled by user", slog.String("msg_id", msgID), slog.Int("peers", len(cancelled)))
	}
	return nil
}

// cancelActiveSendersForMsg закрывает cancelCh всех активных sendStates
// для указанного msgID. Возвращает список peerID кому шла отправка
// (для последующего notify через attachment_cancel).
//
// Используется и при user-initiated cancel, и перед UnlinkAttachmentsForMessage
// — иначе goroutine продолжает читать blob'ы которые вот-вот удалят.
func (s *MessagingService) cancelActiveSendersForMsg(msgID string) []string {
	s.ensureAttachMaps()
	s.attachMu.Lock()
	var toCancel []*attachSendState
	for k, st := range s.sendStates {
		if st.msgID == msgID {
			toCancel = append(toCancel, st)
			delete(s.sendStates, k)
		}
	}
	s.attachMu.Unlock()

	peerIDs := make([]string, 0, len(toCancel))
	for _, st := range toCancel {
		// safe close — cancelCh ровно один раз закрывается, sender видит и выходит
		select {
		case <-st.cancelCh:
			// уже закрыт
		default:
			close(st.cancelCh)
		}
		peerIDs = append(peerIDs, st.peerID)
	}
	return peerIDs
}

// --- Emit helpers ---

func (s *MessagingService) emitAttachProgress(msgID, peerID string, sent, total int64) {
	s.network.emitEvent("vox:attach-progress", map[string]any{
		"msgId":  msgID,
		"peerId": peerID,
		"sent":   sent,
		"total":  total,
	})
}

func (s *MessagingService) emitAttachComplete(msgID, peerID string) {
	// Подкладываем реальные attachments (id/sha/mime/size). UI на outbound bubble
	// при старте имел только локальные ObjectURL-превью (localUrl из attach.js);
	// по завершении доставки UI переключит <img> на attachmentBlobURL(realAtt)
	// и только потом revoke'нет localUrl. Без realAtts был break: revoke убивал
	// blob-URL, но в bubble он всё ещё висел как src → broken image.
	atts := s.loadAttachmentsForEmit(msgID)
	s.network.emitEvent("vox:attach-complete", map[string]any{
		"msgId":       msgID,
		"peerId":      peerID,
		"attachments": atts,
	})
}

// loadAttachmentsForEmit — минимальная meta-информация для UI swap: id, kind,
// name, mime, size, width, height. Порядок по ord — совпадает с previews.
// Non-nil slice даже если ничего не нашли (JSON-сериализация даёт [], не null).
func (s *MessagingService) loadAttachmentsForEmit(msgID string) []AttachmentMeta {
	out := s.LoadAttachmentsForMessage(msgID, "dm")
	if len(out) == 0 {
		out = s.LoadAttachmentsForMessage(msgID, "group")
	}
	if out == nil {
		out = []AttachmentMeta{}
	}
	return out
}

// sendAttachComplete отправляет attachment_complete тем же транспортом, что
// выбран для chunks этого transfer'а. Для бинарного DC это критично: complete
// должен пройти ПОСЛЕ всех chunks, иначе receiver finalize'нет writer пустым
// и sha не сойдётся (видели sha256("")=e3b0c442... в логах). SCTP ordered
// гарантирует это только в пределах одного DC.
func (s *MessagingService) sendAttachComplete(peerID, msgID string, attIdx int, sha256Hex string, useBinaryAttachDC bool) {
	if useBinaryAttachDC {
		frame, err := encodeAttachComplete(msgID, attIdx, sha256Hex)
		if err != nil {
			s.log.Error("attach complete: binary encode",
				slog.String("msg_id", msgID),
				slog.Int("att", attIdx),
				slog.String("err", err.Error()))
			return
		}
		// SendAttachChunk имеет внутренний backpressure: ждёт пока буфер
		// "attachments" DC не освободится. Это нам и нужно — complete пойдёт
		// в очередь после всех уже-stuffed chunks.
		if err := s.network.SendAttachChunk(peerID, frame); err != nil {
			s.log.Warn("attach complete: binary send",
				slog.String("msg_id", msgID),
				slog.Int("att", attIdx),
				slog.String("err", err.Error()))
		}
		return
	}

	// Legacy: JSON через "messages" DC. Используется когда binary DC не открыт.
	completeMsg := P2PMessage{
		Type: "attachment_complete",
		ID:   msgID,
		Att:  attIdx,
		Sha:  sha256Hex,
	}
	if data := s.marshalP2P(completeMsg); data != nil {
		if err := s.network.SendP2PData(peerID, data); err != nil {
			s.log.Warn("attach complete: legacy send",
				slog.String("msg_id", msgID),
				slog.Int("att", attIdx),
				slog.String("err", err.Error()))
		}
	}
}

func (s *MessagingService) emitAttachFailed(msgID, peerID, reason string) {
	s.network.emitEvent("vox:attach-failed", map[string]any{
		"msgId":  msgID,
		"peerId": peerID,
		"error":  reason,
	})
}

// --- Periodic GC of stale assemblies (called once at startup) ---

// StartAttachmentsGC запускает фоновую очистку зависших receiver-assemblies.
// Вызывается из main.go после старта.
func (s *MessagingService) StartAttachmentsGC() {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			s.gcStaleAssemblies()
		}
	}()
}

func (s *MessagingService) gcStaleAssemblies() {
	s.ensureAttachMaps()
	cutoff := time.Now().Add(-attachAssemblyInactivityTTL)
	s.attachMu.Lock()
	var stale []*attachRecvAssembly
	for k, asm := range s.recvAssemblies {
		// last = lastChunkAt если был хоть один чанк, иначе createdAt.
		// Иначе assembly без чанков (ack отправили, sender пропал) висел бы вечно.
		asm.mu.Lock()
		last := asm.lastChunkAt
		if last.IsZero() {
			last = asm.createdAt
		}
		asm.mu.Unlock()
		if last.Before(cutoff) {
			stale = append(stale, asm)
			delete(s.recvAssemblies, k)
		}
	}
	s.attachMu.Unlock()
	for _, asm := range stale {
		s.log.Warn("attach: GC stale assembly (no chunks for >TTL)",
			slog.String("msg_id", asm.msgID),
			slog.String("peer", safeShort(asm.peerID)))
		s.cleanupAssembly(asm)
	}
}

// --- Misc helpers ---

// isThrottleTimeout различает "буфер SCTP полный, подожди" (retry worthy)
// от прочих ошибок SendP2PDataThrottled (DC closed, peer gone, Send failed —
// все fatal на уровне этой передачи).
//
// PeerManager.SendDataThrottled возвращает error со строкой "send timeout"
// только при backpressure-ожидании, см. peermgr.go:SendDataThrottled.
func isThrottleTimeout(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	// "send timeout: буфер DataChannel переполнен"
	return strings.Contains(s, "send timeout")
}

// peerMsgKey — ключ "peer|msgID" для send/recv карт.
// Send и receive живут в разных map'ах (sendStates / recvAssemblies), поэтому
// коллизий между ними не бывает — один helper на оба направления.
func peerMsgKey(peerID, msgID string) string {
	return peerID + "|" + msgID
}

