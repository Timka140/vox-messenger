package files

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"vox_app/internal/logsafe"
	"vox_app/internal/services/files/wire"
	"vox_app/internal/storage/cas"
)

const (
	// pullWindowBytes — сколько байт держим «в полёте» (запрошено, ещё не
	// получено). Окно непрерывное: как только ответы освобождают место на
	// pullBatchSize чанков, уходит следующий запрос. Прежний вариант
	// (pullPipelineDepth=4, сравнивался с числом ЧАНКОВ в полёте) после
	// первого бёрста дозапрашивал только когда в полёте оставалось ≤3 чанка —
	// труба пустела на каждом цикле, посекундный профиль 86–627 КБ/с при
	// канале 4+ Мбит/с (замер 2026-09-08). 1.5 MiB ≈ 24 чанка по 64 KiB:
	// при RTT 100 мс через TURN хватает на ~15 МБ/с, sender'ский
	// attachSendBufferThreshold (256 KiB) сглаживает бёрст на его стороне.
	pullWindowBytes = 1536 * 1024

	// pullBatchSize — сколько chunks просим в одном CHUNK_REQUEST. Sender
	// в ответ шлёт N CHUNK_DATA подряд (chunk_size 64 KiB → 512 KiB на запрос).
	pullBatchSize = 8

	// pullIdleTick — страховочный тик pullLoop, если wake не пришёл
	// (stall-проверка, peer offline, истёкшие deferred).
	pullIdleTick = time.Second

	// nakRetryDelay — пауза перед повторным запросом NAK'нутого чанка
	// (NakNotFound: у sender'а временно нет данных).
	nakRetryDelay = 3 * time.Second

	// nakGiveUpFactor — если NAK'ов больше, чем chunks × factor, sender
	// систематически не отдаёт файл: останавливаем download как lost.
	nakGiveUpFactor = 3

	// bitmapPersistInterval — как часто сбрасываем bitmap в БД.
	bitmapPersistInterval = 2 * time.Second

	// pullStallTimeout — если за это время не пришло ни одного chunk —
	// перезапрашиваем (sender могло перезапустить, потеряли запросы).
	// 10 с: при RTT ~100 мс и непрерывном окне тишина дольше нескольких
	// секунд — уже потеря; 30 с стоили полминуты на каждый сбой sender'а
	// (замер 2026-09-08 14:55).
	pullStallTimeout = 10 * time.Second

	// staleDownloadThreshold — если downloadState не обновлял lastActive
	// дольше этого времени, считаем его мёртвым (предыдущая попытка завершилась
	// тем, что Files PC рухнул до первого chunk, а cancel-канал не отработал).
	// При следующем StartFileDownload такой state кикается и создаётся новый.
	staleDownloadThreshold = 60 * time.Second

	// autoPullPhotoMaxSize — фото меньше этого размера качаются автоматически
	// (превью сразу видно в чате, как в Telegram). Всё остальное — manual:
	// пользователь жмёт кнопку «Скачать» в bubble → StartFileDownload.
	autoPullPhotoMaxSize = 1 * 1024 * 1024 // 1 MB
)

// onFileManifest — receiver-side обработка входящего FILE_MANIFEST.
// 1. Verify sha манифестов; 2. SaveManifest в files+file_chunks;
// 3. INSERT messages/group_messages + message_files; 4. Создать file_downloads
// row с empty bitmap; 5. Запустить pull-loop для каждого файла.
//
// Approval-логику пока не делаем — auto-accept всегда. Можно добавить позже.
func (s *FilesService) onFileManifest(fromPeer string, frame []byte) {
	payload, err := wire.DecodeFileManifest(frame)
	if err != nil {
		s.log.Warn("file manifest: decode",
			slog.String("peer", safeShort(fromPeer)),
			logsafe.Err("err", err))
		return
	}
	// SenderID нет в wire (он всегда fromPeer). Заполняем здесь перед emit.
	payload.SenderID = fromPeer

	// Верификация каждого manifest'а.
	for i, f := range payload.Files {
		m := &cas.FileManifest{
			Sha: f.Sha, Size: f.Size, ChunkSize: f.ChunkSize,
			ChunkCount: f.ChunkCount, Chunks: f.Chunks, Mime: f.Mime,
		}
		if err := m.Verify(); err != nil {
			s.log.Warn("file manifest: bad entry",
				slog.String("peer", safeShort(fromPeer)),
				slog.String("msg_id", payload.MsgID),
				slog.Int("idx", i),
				logsafe.Err("err", err))
			return
		}
	}

	if err := s.persistIncomingMessage(fromPeer, payload); err != nil {
		s.log.Error("file manifest: persist",
			slog.String("peer", safeShort(fromPeer)),
			slog.String("msg_id", payload.MsgID),
			logsafe.Err("err", err))
		return
	}

	s.log.Info("📥 FILE MANIFEST received",
		slog.String("peer", safeShort(fromPeer)),
		slog.String("msg_id", payload.MsgID),
		slog.Int("files", len(payload.Files)))

	// Manual-download UX:
	//  - Уже скачанные (dedup) файлы — мгновенно emitFileReceived (UI покажет готовый bubble).
	//  - Маленькие фото (<1 MB) — auto-pull, превью видно сразу как в Telegram.
	//  - Всё остальное (большие фото, документы, видео, архивы) — НЕ качаем.
	//    Bubble показывает иконку файла + кнопку «Скачать». Юзер жмёт — фронт зовёт
	//    StartFileDownload(peerID, fileSha) → запускается pull.
	for _, f := range payload.Files {
		ff := f
		if s.isFullyAvailable(&ff) {
			s.emitFileReceived(ff.Sha, payload.MsgID)
			continue
		}
		// Параметры для решения «auto или manual»:
		isPhoto := ff.Kind == "photo"
		small := ff.Size <= autoPullPhotoMaxSize
		if isPhoto && small {
			s.startDownload(fromPeer, payload.MsgID, payload.GroupID, &ff)
			continue
		}
		// Manual mode: запоминаем где взять файл (peer), но pull не стартуем.
		s.parkManualDownload(ff.Sha, fromPeer, payload.MsgID, payload.GroupID, &ff)
	}
}

// parkManualDownload — сохраняем info о доступном файле, чтобы юзер мог
// нажать «Скачать» и StartFileDownload нашёл откуда качать.
func (s *FilesService) parkManualDownload(fileSha, peerID, msgID, groupID string, entry *wire.FileManifestEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.manualSources == nil {
		s.manualSources = make(map[string]*manualSource)
	}
	s.manualSources[fileSha] = &manualSource{
		peerID:  peerID,
		msgID:   msgID,
		groupID: groupID,
		entry:   cloneEntry(entry),
	}
}

// StartFileDownload — Wails-метод: пользователь нажал «Скачать» на bubble.
// Берём info из manualSources и запускаем pull. Если уже качается — no-op.
func (s *FilesService) StartFileDownload(peerID, fileSha string) error {
	s.mu.Lock()
	src, ok := s.manualSources[fileSha]
	if !ok {
		// Файл может быть на нескольких peers. Если переданный peerID отличается —
		// используем его, manifest подгрузим из БД.
		s.mu.Unlock()
		return s.startManualWithoutSource(peerID, fileSha)
	}
	delete(s.manualSources, fileSha)
	s.mu.Unlock()

	// Если в запросе явно другой peer — используем тот что просил юзер
	// (обычно совпадает с msg.senderId, но swarm в будущем может предложить).
	usePeer := src.peerID
	if peerID != "" && peerID != src.peerID && s.network.IsP2PConnected(peerID) {
		usePeer = peerID
	}
	s.log.Info("file download: STARTED by user",
		slog.String("peer", safeShort(usePeer)),
		slog.String("file", safeShort8(fileSha)),
		slog.Int64("size", src.entry.Size))
	s.startDownload(usePeer, src.msgID, src.groupID, src.entry)
	return nil
}

// startManualWithoutSource — fallback: manualSources уже очищен (например после
// перезапуска приложения), но manifest есть в БД. Подгружаем из CAS и стартуем
// pull. msgID/groupID находим через message_files.
func (s *FilesService) startManualWithoutSource(peerID, fileSha string) error {
	m, err := cas.LoadManifest(s.db, fileSha)
	if err != nil {
		return fmt.Errorf("StartFileDownload: %w", err)
	}
	var msgID, groupID string
	_ = s.db.QueryRow(
		`SELECT COALESCE(message_id, ''), COALESCE(group_msg_id, '')
		 FROM message_files WHERE file_sha = ? LIMIT 1`,
		fileSha).Scan(&msgID, &groupID)
	if msgID == "" && groupID == "" {
		return fmt.Errorf("StartFileDownload: no message_files row for %s", safeShort8(fileSha))
	}
	anyMsgID := msgID
	if anyMsgID == "" {
		anyMsgID = groupID
	}
	// Пустой peerID (кнопка «Продолжить» после cancel, когда manualSources
	// уже пуст после перезапуска) — берём отправителя сообщения; иначе
	// download стартовал с sourcePeer="" и сразу вставал на паузу «peer
	// offline», а кнопка выглядела нерабочей.
	if peerID == "" {
		if msgID != "" {
			_ = s.db.QueryRow(`SELECT sender_id FROM messages WHERE id = ?`, msgID).Scan(&peerID)
		} else {
			_ = s.db.QueryRow(`SELECT sender_id FROM group_messages WHERE id = ?`, groupID).Scan(&peerID)
		}
	}
	if peerID == "" {
		s.emitFileCancelled(fileSha)
		return fmt.Errorf("StartFileDownload: no source peer for %s", safeShort8(fileSha))
	}
	entry := wire.FileManifestEntry{
		Sha: m.Sha, Size: m.Size, ChunkSize: m.ChunkSize,
		ChunkCount: m.ChunkCount, Chunks: m.Chunks, Mime: m.Mime,
		Width: m.Width, Height: m.Height,
	}
	s.log.Info("file download: STARTED (no source cached)",
		slog.String("peer", safeShort(peerID)),
		slog.String("file", safeShort8(fileSha)))
	s.startDownload(peerID, anyMsgID, groupID, &entry)
	return nil
}

// CancelFileDownload — пользователь нажал «×» на bubble во время скачки.
// Закрываем cancel-channel pull-loop'а, удаляем download state. Сами chunks
// что уже легли на диск — оставляем (могут пригодиться при retry; GC уберёт
// если файл удалят полностью).
//
// Защитно отменяем ВСЕ downloadState для этого fileSha — на случай если
// дубль каким-то образом проскочил мимо dedup'а в startDownload (например
// race пока добавлялись разные ключи sha|peerA / sha|""). Frontend гарантированно
// получит vox:file-cancelled и вернёт «Скачать»-кнопку.
func (s *FilesService) CancelFileDownload(fileSha string) error {
	s.mu.Lock()
	var states []*downloadState
	for k, d := range s.downloads {
		if d.fileSha == fileSha {
			states = append(states, d)
			delete(s.downloads, k)
		}
	}
	s.mu.Unlock()
	if len(states) == 0 {
		// Уже не качается — emit'им cancelled на всякий случай, чтобы UI
		// гарантированно вернулся в Download-состояние (idempotent для frontend).
		s.emitFileCancelled(fileSha)
		return nil
	}
	for _, st := range states {
		// 'paused' — явная отмена пользователем: OnPeerOnline /
		// ResumePendingDownloads такие строки не трогают (только 'pending' и
		// 'active'), иначе отменённая скачка сама возобновлялась при следующем
		// reconnect пира. Bitmap и partial сохраняются — ручной «Скачать»
		// продолжит с места остановки.
		s.persistBitmap(st, "paused")
		// Закрываем .vox-partial handle, файл остаётся на диске (resume
		// при ручном retry: pullLoop reopen'ит partial_path из БД).
		st.mu.Lock()
		pf := st.partialFile
		st.partialFile = nil
		st.mu.Unlock()
		if pf != nil {
			_ = pf.Sync()
			_ = pf.Close()
		}
		st.closeCancel()
	}
	// Возвращаем file в manualSources только один раз (любой st подойдёт).
	first := states[0]
	s.parkManualDownload(fileSha, first.sourcePeer, "", "", first.manifest)
	s.log.Info("file download: CANCELLED by user",
		slog.String("file", safeShort8(fileSha)),
		slog.Int("states_cancelled", len(states)))
	s.emitFileCancelled(fileSha)
	return nil
}

type manualSource struct {
	peerID  string
	msgID   string
	groupID string
	entry   *wire.FileManifestEntry
}

// fileNameForSha — пытается найти оригинальное имя файла из любой
// message_files-row. Если не нашли — fallback на sha-based имя.
func (s *FilesService) fileNameForSha(fileSha string) string {
	var name string
	_ = s.db.QueryRow(
		`SELECT name FROM message_files WHERE file_sha = ? LIMIT 1`, fileSha,
	).Scan(&name)
	if name == "" {
		return fileSha[:8] + ".bin"
	}
	return name
}

// isFullyAvailable — single-file модель: source of truth = local_path в
// Vox Downloads. Если local_path есть и файл физически на диске — файл готов.
// Иначе нужно скачать (или ре-скачать, если bitmap утверждает complete но
// файл удалили — обрабатывается в startDownload через stale-bitmap reset).
func (s *FilesService) isFullyAvailable(f *wire.FileManifestEntry) bool {
	local := cas.GetFileLocalPath(s.db, f.Sha)
	return local != "" && FileExists(local)
}

// parkPendingApproval сохраняет manifest для последующего ApproveFileDownload.
// Хранится в памяти — после перезапуска приложения approval-state теряется
// (юзер увидит manifest снова при reconnect peer'а).
func (s *FilesService) parkPendingApproval(msgID, peerID, groupID string, files []wire.FileManifestEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingApprovals == nil {
		s.pendingApprovals = make(map[string]*pendingApproval)
	}
	s.pendingApprovals[msgID] = &pendingApproval{
		peerID:  peerID,
		groupID: groupID,
		files:   files,
	}
}

// emitNeedsApproval — событие для UI-модалки. Совместимый формат с legacy
// vox:attach-needs-approval (см. attach_approve.js).
func (s *FilesService) emitNeedsApproval(p *wire.FileManifestPayload, fromPeer string, pending []wire.FileManifestEntry, pendingBytes int64) {
	if s.msg == nil {
		return
	}
	totalBytes := int64(0)
	files := make([]map[string]any, 0, len(p.Files))
	for _, f := range p.Files {
		dedup := true
		for _, pf := range pending {
			if pf.Sha == f.Sha {
				dedup = false
				break
			}
		}
		totalBytes += f.Size
		files = append(files, map[string]any{
			"name":  f.Name,
			"kind":  f.Kind,
			"mime":  f.Mime,
			"size":  f.Size,
			"dedup": dedup,
		})
	}
	s.msg.EmitEvent("vox:attach-needs-approval", map[string]any{
		"msgId":      p.MsgID,
		"peerId":     fromPeer,
		"groupId":    p.GroupID,
		"totalBytes": totalBytes,
		"caption":    p.Caption,
		"files":      files,
	})
}

// ApproveFileDownload — Wails-метод, вызывается из attach_approve.js по «Принять».
func (s *FilesService) ApproveFileDownload(peerID, msgID string) error {
	s.mu.Lock()
	pa, ok := s.pendingApprovals[msgID]
	if ok {
		delete(s.pendingApprovals, msgID)
	}
	s.mu.Unlock()
	if !ok {
		return nil // уже обработано или не было
	}
	s.log.Info("file approval: ACCEPTED",
		slog.String("peer", safeShort(peerID)),
		slog.String("msg_id", msgID),
		slog.Int("files", len(pa.files)))
	for _, f := range pa.files {
		ff := f
		s.startDownload(pa.peerID, msgID, pa.groupID, &ff)
	}
	return nil
}

// DeclineFileDownload — отказ. Шлём sender'у FILE_DECLINE и забываем manifest.
func (s *FilesService) DeclineFileDownload(peerID, msgID string) error {
	s.mu.Lock()
	pa, ok := s.pendingApprovals[msgID]
	if ok {
		delete(s.pendingApprovals, msgID)
	}
	s.mu.Unlock()
	if !ok {
		return nil
	}
	if frame, err := wire.EncodeFileDecline(msgID); err == nil {
		_ = s.network.SendAttachChunk(pa.peerID, frame)
	}
	s.log.Info("file approval: DECLINED",
		slog.String("peer", safeShort(peerID)),
		slog.String("msg_id", msgID))
	return nil
}

type pendingApproval struct {
	peerID  string
	groupID string
	files   []wire.FileManifestEntry
}

// persistIncomingMessage:
//  1. SaveManifest для каждого файла
//  2. INSERT message + message_files (idempotent через INSERT OR IGNORE на messages)
//  3. IncFileRefcount для каждого file
//
// Если message уже существовал (duplicate manifest) — message_files INSERT
// пропускается, refcount не инкрементируется (чтобы не утечь).
func (s *FilesService) persistIncomingMessage(fromPeer string, p *wire.FileManifestPayload) error {
	// Шаг 1 — manifests (вне tx; SaveManifest сам tx-aware и идемпотентен).
	for _, f := range p.Files {
		m := &cas.FileManifest{
			Sha: f.Sha, Size: f.Size, ChunkSize: f.ChunkSize,
			ChunkCount: f.ChunkCount, Chunks: f.Chunks, Mime: f.Mime,
			Width: f.Width, Height: f.Height,
		}
		if err := cas.SaveManifest(s.db, m); err != nil {
			return fmt.Errorf("save manifest %s: %w", safeShort8(f.Sha), err)
		}
	}

	// Шаг 2 — message + links в одной транзакции.
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	now := time.Now().Unix()
	fwd := ""
	if p.ForwardedFrom != nil && p.ForwardedFrom.Name != "" {
		r := []rune(p.ForwardedFrom.Name)
		if len(r) > 64 {
			r = r[:64]
		}
		p.ForwardedFrom.Name = string(r)
		if b, err := json.Marshal(p.ForwardedFrom); err == nil {
			fwd = string(b)
		}
	}
	var res sql.Result
	if p.GroupID != "" {
		res, err = tx.Exec(
			`INSERT OR IGNORE INTO group_messages (id, group_id, sender_id, content, type, created_at, forwarded_from)
			 VALUES (?, ?, ?, ?, 'attachment', ?, ?)`,
			p.MsgID, p.GroupID, fromPeer, p.Caption, now, fwd)
	} else {
		res, err = tx.Exec(
			`INSERT OR IGNORE INTO messages (id, peer_id, sender_id, content, type, read, delivered, created_at, forwarded_from)
			 VALUES (?, ?, ?, ?, 'attachment', 0, 1, ?, ?)`,
			p.MsgID, fromPeer, fromPeer, p.Caption, now, fwd)
	}
	if err != nil {
		return fmt.Errorf("insert message: %w", err)
	}
	rowsAffected, _ := res.RowsAffected()
	if rowsAffected == 0 {
		// Дубликат manifest'а — message уже есть. Пропускаем link'и + refcount.
		_ = tx.Rollback()
		s.log.Debug("file manifest: duplicate message (idempotent)",
			slog.String("msg_id", p.MsgID))
		return nil
	}

	for i, f := range p.Files {
		if p.GroupID != "" {
			if _, err := tx.Exec(
				`INSERT INTO message_files (group_msg_id, file_sha, ord, name, kind)
				 VALUES (?, ?, ?, ?, ?)`,
				p.MsgID, f.Sha, i, f.Name, f.Kind); err != nil {
				return fmt.Errorf("insert link: %w", err)
			}
		} else {
			if _, err := tx.Exec(
				`INSERT INTO message_files (message_id, file_sha, ord, name, kind)
				 VALUES (?, ?, ?, ?, ?)`,
				p.MsgID, f.Sha, i, f.Name, f.Kind); err != nil {
				return fmt.Errorf("insert link: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true

	for _, f := range p.Files {
		if err := cas.IncFileRefcount(s.db, f.Sha); err != nil {
			s.log.Warn("inc refcount on receive",
				slog.String("file_sha", safeShort8(f.Sha)),
				logsafe.Err("err", err))
		}
	}

	// Эмит UI о новом сообщении (как старый vox:message-received). MessagingOps
	// уже знает как.
	s.emitFileMessage(p)
	return nil
}

// startDownload — запускает pull-loop для одного файла. Идемпотентно: если
// этот файл уже качается (от любого peer'а) — no-op. Раньше дедуп был по
// (fileSha, peer), но юзер мог двойным кликом «⬇» создать второй download
// со sourcePeer="" (через startManualWithoutSource при пустом manualSources):
// CancelFileDownload видел только одну запись, вторая продолжала слать chunks
// → progress event → frontend снова рисовал X.
func (s *FilesService) startDownload(sourcePeer, msgID, groupID string, f *wire.FileManifestEntry) {
	// Если файл уже собран в Vox Downloads — качать нечего, эмитим complete и
	// выходим. Защита от случая, когда manifest приходит повторно (пересланное
	// сообщение, новый msg_id для уже-имеющегося файла), и юзер жмёт Download
	// несмотря на то, что bubble должен показывать Open.
	if local := cas.GetFileLocalPath(s.db, f.Sha); local != "" && FileExists(local) {
		s.log.Debug("startDownload skipped: file already assembled on disk",
			slog.String("file", safeShort8(f.Sha)))
		s.emitFileReceived(f.Sha, msgID)
		return
	}

	key := f.Sha + "|" + sourcePeer
	s.mu.Lock()
	if existing, exists := s.downloads[key]; exists {
		// Проверяем не "залип" ли старый state. Если pull-loop давно не
		// обновлял lastActive — Files PC, вероятно, упал до первого chunk
		// (см. логи: dtls timeout через 6 сек после manifest), а cancel
		// никто не дёрнул. Кикаем мёртвый state и стартуем новый.
		existing.mu.Lock()
		idle := time.Since(existing.lastActive)
		existing.mu.Unlock()
		if idle > staleDownloadThreshold {
			s.log.Info("startDownload: replacing stale state",
				slog.String("file", safeShort8(f.Sha)),
				slog.String("peer", safeShort(sourcePeer)),
				slog.Duration("idle", idle))
			delete(s.downloads, key)
			existing.closeCancel()
		} else {
			s.mu.Unlock()
			s.log.Debug("download already active",
				slog.String("file", safeShort8(f.Sha)),
				slog.Duration("idle", idle))
			// UI мог потерять state (перерисовка чата) — синхронизируем.
			s.emitFileProgress(existing)
			return
		}
	}
	// Cross-peer dedup: если уже качается от другого peer'а — no-op
	// (с учётом stale-чистки выше).
	for k, d := range s.downloads {
		if d.fileSha == f.Sha {
			d.mu.Lock()
			idle := time.Since(d.lastActive)
			d.mu.Unlock()
			if idle > staleDownloadThreshold {
				s.log.Info("startDownload: replacing stale cross-peer state",
					slog.String("file", safeShort8(f.Sha)),
					slog.String("active_peer", safeShort(d.sourcePeer)),
					slog.Duration("idle", idle))
				delete(s.downloads, k)
				d.closeCancel()
				continue
			}
			s.mu.Unlock()
			s.log.Debug("download already active (other peer)",
				slog.String("file", safeShort8(f.Sha)),
				slog.String("active_peer", safeShort(d.sourcePeer)))
			s.emitFileProgress(d)
			return
		}
	}

	bitmap, partialPath, err := s.loadOrCreateBitmap(f.Sha, sourcePeer, f.ChunkCount)
	if err != nil {
		s.mu.Unlock()
		s.log.Error("load bitmap", logsafe.Err("err", err))
		s.emitFileCancelled(f.Sha)
		return
	}
	if bitmap.Complete() {
		// Sanity-check: bitmap может быть stale. Source of truth — local_path
		// в Vox Downloads. Если юзер удалил тот файл, данных нет нигде:
		// bitmap остался "complete" но файла нет. В этом случае нужно
		// перекачать.
		if s.isFullyAvailable(f) {
			s.mu.Unlock()
			s.log.Debug("file already complete", slog.String("file", safeShort8(f.Sha)))
			s.emitFileReceived(f.Sha, msgID)
			return
		}
		s.log.Warn("stale complete bitmap — данных нет на диске, re-download",
			slog.String("file", safeShort8(f.Sha)),
			slog.String("peer", safeShort(sourcePeer)))
		// Перед DELETE собираем partial-пути из БД, чтобы потом удалить
		// файлы с диска (могут быть осколки от прошлых попыток).
		var stalePartials []string
		rows, qerr := s.db.Query(
			`SELECT partial_path FROM file_downloads WHERE file_sha = ? AND partial_path != ''`, f.Sha)
		if qerr == nil {
			for rows.Next() {
				var pp string
				if rows.Scan(&pp) == nil {
					stalePartials = append(stalePartials, pp)
				}
			}
			rows.Close()
		}
		// Сбрасываем bitmap row для всех source_peer (могут быть резидуальные
		// записи от прошлых попыток с другими peer'ами) — иначе следующий
		// loadOrCreateBitmap заберёт другой stale row.
		if _, err := s.db.Exec(
			`DELETE FROM file_downloads WHERE file_sha = ?`, f.Sha); err != nil {
			s.mu.Unlock()
			s.log.Error("delete stale bitmap row", logsafe.Err("err", err))
			s.emitFileCancelled(f.Sha)
			return
		}
		for _, pp := range stalePartials {
			_ = os.Remove(pp)
		}
		// local_path тоже "утерял связь" — сбрасываем чтобы UI не показывал
		// файл как доступный для Open пока re-download не завершится.
		if err := cas.SetFileLocalPath(s.db, f.Sha, ""); err != nil {
			s.log.Warn("reset local_path", logsafe.Err("err", err))
		}
		bitmap, partialPath, err = s.loadOrCreateBitmap(f.Sha, sourcePeer, f.ChunkCount)
		if err != nil {
			s.mu.Unlock()
			s.log.Error("recreate bitmap after reset", logsafe.Err("err", err))
			s.emitFileCancelled(f.Sha)
			return
		}
	}

	// Открываем .vox-partial файл для прямой записи чанков по offset.
	// При resume re-open того же файла (existing partialPath из БД).
	partialFile, partialPath, finalPath, err := s.openOrResumePartialFile(f.Sha, sourcePeer, partialPath, f.Size)
	if err != nil {
		s.mu.Unlock()
		s.log.Error("open partial file",
			slog.String("file", safeShort8(f.Sha)),
			logsafe.Err("err", err))
		s.emitFileCancelled(f.Sha)
		return
	}

	st := &downloadState{
		fileSha:     f.Sha,
		sourcePeer:  sourcePeer,
		manifest:    cloneEntry(f),
		bitmap:      bitmap,
		inFlight:    make(map[int]bool),
		wake:        make(chan struct{}, 1),
		deferred:    make(map[int]time.Time),
		pendingMsgs: map[string]bool{msgID: true},
		lastActive:  time.Now(),
		cancel:      make(chan struct{}),
		partialFile: partialFile,
		partialPath: partialPath,
		finalPath:   finalPath,
	}
	s.downloads[key] = st
	s.mu.Unlock()

	// Первый progress — сразу, до поднятия files PC (через TURN это 2–5 с):
	// UI переключает «Скачать» на «×» + шкалу мгновенно, а не с первым чанком.
	// При resume received > 0 — шкала сразу показывает, сколько уже есть.
	s.emitFileProgress(st)
	s.ensureStatsLoop()
	go s.pullLoop(st, msgID)
}

// pullMaxInFlight — сколько чанков помещается в окно pullWindowBytes для
// данного chunk_size (не меньше одного batch'а).
func pullMaxInFlight(chunkSize int) int {
	if chunkSize <= 0 {
		return pullBatchSize
	}
	n := pullWindowBytes / chunkSize
	if n < pullBatchSize {
		n = pullBatchSize
	}
	return n
}

// missingNotInFlight — диапазоны недостающих чанков, которые ещё НЕ запрошены
// и не отложены (deferred до now), не длиннее batch каждый, суммарно не
// больше maxChunks. Сканирует bitmap с начала, перешагивая in-flight (они
// тоже ещё не в bitmap). Вызывать под st.mu. Bitmap.MissingRanges для этого
// не годится: он отдаёт первые N диапазонов, а начало missing-области всегда
// занято in-flight чанками — окно не продвигалось бы дальше них.
func missingNotInFlight(bitmap *Bitmap, inFlight map[int]bool, deferred map[int]time.Time, now time.Time, maxChunks, batch int) []ChunkRange {
	if maxChunks <= 0 {
		return nil
	}
	if batch <= 0 {
		batch = 1
	}
	skip := func(i int) bool {
		if bitmap.Get(i) || inFlight[i] {
			return true
		}
		if until, ok := deferred[i]; ok {
			if now.Before(until) {
				return true
			}
			delete(deferred, i)
		}
		return false
	}
	var out []ChunkRange
	total := bitmap.Total()
	got := 0
	i := 0
	for i < total && got < maxChunks {
		if skip(i) {
			i++
			continue
		}
		start := i
		for i < total && !skip(i) && i-start < batch && got < maxChunks {
			i++
			got++
		}
		out = append(out, ChunkRange{Start: start, Count: i - start})
	}
	return out
}

// pullLoop — для одного (fileSha, peer): держит окно pullWindowBytes
// запрошенных чанков непрерывно заполненным, ждёт пока bitmap не станет
// complete или receiver/sender не упадёт. Событийный: просыпается по
// st.wake (onChunkData/onChunkNak) или страховочному тику pullIdleTick.
func (s *FilesService) pullLoop(st *downloadState, msgID string) {
	maxInFlight := pullMaxInFlight(st.manifest.ChunkSize)
	wait := func() bool {
		select {
		case <-st.cancel:
			return false
		case <-st.wake:
		case <-time.After(pullIdleTick):
		}
		return true
	}

	for {
		select {
		case <-st.cancel:
			return
		default:
		}

		// Peer offline → пауза (перезапустим при reconnect). Проверка стоит
		// ДО расчёта окна, чтобы при заполненном окне цикл всё равно доходил
		// и до неё, и до stall-reset.
		if !s.network.IsP2PConnected(st.sourcePeer) {
			s.log.Debug("pull: peer offline, pause",
				slog.String("file", safeShort8(st.fileSha)),
				slog.String("peer", safeShort(st.sourcePeer)))
			s.markStateInactive(st)
			return
		}

		st.mu.Lock()
		if st.bitmap.Complete() {
			st.mu.Unlock()
			s.finishDownload(st, msgID)
			return
		}
		// Stall: ответов нет дольше pullStallTimeout — сбрасываем inFlight,
		// чтобы пере-запросить. lastActive читаем под st.mu (пишет onChunkData).
		if len(st.inFlight) > 0 && time.Now().After(st.lastActive.Add(pullStallTimeout)) {
			s.log.Warn("pull: stall, retrying inflight",
				slog.String("file", safeShort8(st.fileSha)),
				slog.String("peer", safeShort(st.sourcePeer)),
				slog.Int("inflight", len(st.inFlight)))
			st.inFlight = make(map[int]bool)
			st.lastActive = time.Now()
			s.stats.stalls.Add(1)
		}
		// Дозапрашиваем только целыми batch'ами: room < batch → ждём.
		room := maxInFlight - len(st.inFlight)
		if room < pullBatchSize {
			st.mu.Unlock()
			if !wait() {
				return
			}
			continue
		}
		toRequest := missingNotInFlight(st.bitmap, st.inFlight, st.deferred, time.Now(), room, pullBatchSize)
		if len(toRequest) > 0 && len(st.inFlight) == 0 {
			// Первый запрос после старта/stall — отсчёт stall с этого момента.
			st.lastActive = time.Now()
		}
		for _, r := range toRequest {
			for i := r.Start; i < r.Start+r.Count; i++ {
				st.inFlight[i] = true
			}
		}
		st.mu.Unlock()

		if len(toRequest) == 0 {
			// Все missing уже запрошены — ждём ответов.
			if !wait() {
				return
			}
			continue
		}

		// Лениво поднимаем files PC, если attach DC ещё не открыт (после
		// обрыва files PC; idempotent). На горячем пути не вызываем — раньше
		// это был вызов с 10-с контекстом на каждой итерации.
		if !s.network.IsAttachDCOpen(st.sourcePeer) {
			ensureCtx, ensureCancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := s.network.EnsureFilesPCReady(ensureCtx, st.sourcePeer)
			ensureCancel()
			if err != nil {
				s.log.Debug("pull: files PC not ready, pause",
					slog.String("file", safeShort8(st.fileSha)),
					slog.String("peer", safeShort(st.sourcePeer)),
					logsafe.Err("err", err))
				s.markStateInactive(st)
				return
			}
		}
		for _, r := range toRequest {
			req, err := wire.EncodeChunkRequest(st.fileSha, r.Start, r.Count)
			if err != nil {
				s.log.Error("pull: encode request", logsafe.Err("err", err))
				continue
			}
			if err := s.network.SendAttachChunk(st.sourcePeer, req); err != nil {
				s.log.Warn("pull: send request",
					slog.String("peer", safeShort(st.sourcePeer)),
					logsafe.Err("err", err))
				s.markStateInactive(st)
				return
			}
		}
		// Без sleep: следующая итерация пересчитает room; если окно полно —
		// wait() до ближайшего chunk_data/NAK.
	}
}

// onChunkData — пришёл chunk от sender'а. Verify, persist, отметить bit.
func (s *FilesService) onChunkData(fromPeer string, frame []byte) {
	fileSha, idx, payload, err := wire.DecodeChunkData(frame)
	if err != nil {
		s.log.Warn("chunk data: decode", logsafe.Err("err", err))
		return
	}

	key := fileSha + "|" + fromPeer
	s.mu.Lock()
	st, ok := s.downloads[key]
	s.mu.Unlock()
	if !ok {
		// Поздний chunk — например после Cancel'а или после reconnect когда
		// мы снова запросили те же chunks. Не критично.
		s.log.Debug("chunk data: no active download",
			slog.String("file", safeShort8(fileSha)))
		return
	}

	// Verify per-chunk.
	if idx < 0 || idx >= len(st.manifest.Chunks) {
		s.log.Warn("chunk data: idx out of range")
		return
	}
	// Длина payload не должна выходить за пределы объявленной геометрии.
	// SHA привязывает СОДЕРЖИМОЕ чанка, но не его длину к ChunkSize — без этой
	// проверки sender мог объявить chunk[0]=хеш огромного блоба и записать его
	// на offset 0, раздув файл далеко за манифестный Size (disk-fill).
	chunkSize := int64(st.manifest.ChunkSize)
	offset := int64(idx) * chunkSize
	if int64(len(payload)) > chunkSize || offset+int64(len(payload)) > st.manifest.Size {
		s.log.Warn("chunk data: payload за пределами манифеста",
			slog.String("file", safeShort8(fileSha)),
			slog.Int("idx", idx),
			slog.Int("len", len(payload)),
			slog.Int64("size", st.manifest.Size))
		return
	}
	wantSha := st.manifest.Chunks[idx]
	gotShaBytes := sha256.Sum256(payload)
	gotSha := hex.EncodeToString(gotShaBytes[:])
	if gotSha != wantSha {
		s.log.Warn("chunk data: SHA MISMATCH (will re-request)",
			slog.String("file", safeShort8(fileSha)),
			slog.Int("idx", idx))
		s.stats.mismatches.Add(1)
		st.mu.Lock()
		delete(st.inFlight, idx)
		st.mu.Unlock()
		st.wakePull()
		return
	}

	// Persist напрямую в .vox-partial файл по offset.
	st.mu.Lock()
	pf := st.partialFile
	st.mu.Unlock()
	if pf == nil {
		s.log.Error("chunk data: partial file not open",
			slog.String("file", safeShort8(fileSha)),
			slog.Int("idx", idx))
		return
	}
	if _, err := pf.WriteAt(payload, offset); err != nil {
		s.log.Error("chunk data: WriteAt",
			slog.String("file", safeShort8(fileSha)),
			slog.Int("idx", idx),
			slog.Int64("offset", offset),
			logsafe.Err("err", err))
		return
	}

	st.mu.Lock()
	if !st.bitmap.Get(idx) {
		st.bitmap.Set(idx)
	}
	delete(st.inFlight, idx)
	st.lastActive = time.Now()
	st.dirty = true
	st.mu.Unlock()
	s.stats.bytesIn.Add(int64(len(payload)))
	s.stats.chunksIn.Add(1)
	// Будим pullLoop: место в окне освободилось / bitmap мог стать complete —
	// он сам решит, дозапрашивать или финишировать (finishDownload не вызываем
	// отсюда, чтобы не плодить race с loop'ом).
	st.wakePull()

	s.scheduleBitmapPersist(st)
	s.emitFileProgress(st)
}

// onChunkNak — sender сказал что chunk недоступен. Переключаемся на другой
// source для этого chunk'а (если есть в группе) или маркируем download failed.
func (s *FilesService) onChunkNak(fromPeer string, frame []byte) {
	fileSha, idx, reason, err := wire.DecodeChunkNak(frame)
	if err != nil {
		return
	}
	key := fileSha + "|" + fromPeer
	s.mu.Lock()
	st, ok := s.downloads[key]
	s.mu.Unlock()
	if !ok {
		s.log.Debug("chunk nak: unknown download",
			slog.String("file", safeShort8(fileSha)),
			slog.Int("idx", idx),
			slog.Int("reason", int(reason)))
		return
	}
	// Снимаем chunk с inFlight чтобы не зависнуть, но не Set bit. Removed /
	// Forbidden — sender говорит, что файла у него больше нет (или изменён):
	// повторять бессмысленно, останавливаем download как lost. NotFound —
	// откладываем чанк на nakRetryDelay; если NAK'ов набралось больше, чем
	// chunks × nakGiveUpFactor, тоже сдаёмся.
	st.mu.Lock()
	delete(st.inFlight, idx)
	// NAK — тоже ответ sender'а: не считать поток NAK'ов стойлом.
	st.lastActive = time.Now()
	st.nakCount++
	if st.deferred != nil {
		st.deferred[idx] = time.Now().Add(nakRetryDelay)
	}
	firstNak := !st.nakLogged
	st.nakLogged = true
	giveUp := reason == wire.NakRemoved || reason == wire.NakForbidden ||
		st.nakCount > st.bitmap.Total()*nakGiveUpFactor
	st.mu.Unlock()
	s.stats.naks.Add(1)
	if giveUp {
		s.log.Warn("attachment lost: sender не отдаёт файл, останавливаем download",
			slog.String("file", safeShort8(fileSha)),
			slog.String("peer", safeShort(fromPeer)),
			slog.Int("idx", idx),
			slog.Int("reason", int(reason)),
			slog.Int("naks", st.nakCount))
		s.markStateFailed(st)
		return
	}
	st.wakePull()
	if firstNak {
		s.log.Warn("chunk nak",
			slog.String("file", safeShort8(fileSha)),
			slog.Int("idx", idx),
			slog.Int("reason", int(reason)))
	} else {
		s.log.Debug("chunk nak",
			slog.String("file", safeShort8(fileSha)),
			slog.Int("idx", idx),
			slog.Int("reason", int(reason)))
	}
}

// finishDownload — bitmap полный. Все байты уже записаны в .vox-partial
// через WriteAt в onChunkData; нам остаётся sync + close + rename → final.
func (s *FilesService) finishDownload(st *downloadState, msgID string) {
	s.persistBitmap(st, "complete")
	s.mu.Lock()
	key := st.fileSha + "|" + st.sourcePeer
	// Identity-guard: startDownload мог заменить stale state под тем же
	// ключом — старый pullLoop не должен удалять/затирать новый.
	if s.downloads[key] == st {
		delete(s.downloads, key)
	}
	s.mu.Unlock()
	st.closeCancel()
	s.log.Info("📥 FILE COMPLETE",
		slog.String("file", safeShort8(st.fileSha)),
		slog.String("peer", safeShort(st.sourcePeer)),
		slog.Int("chunks", st.bitmap.Total()))

	st.mu.Lock()
	pf := st.partialFile
	partialPath := st.partialPath
	finalPath := st.finalPath
	st.partialFile = nil
	st.mu.Unlock()

	if pf != nil {
		if err := pf.Sync(); err != nil {
			s.log.Warn("finish: fsync",
				slog.String("file", safeShort8(st.fileSha)),
				logsafe.Err("err", err))
		}
		if err := pf.Close(); err != nil {
			s.log.Warn("finish: close",
				slog.String("file", safeShort8(st.fileSha)),
				logsafe.Err("err", err))
		}
	}

	// Атомарный переход: .vox-partial → final имя. Если final уже занято
	// (теоретически невозможно — uniqueDownloadPath гарантирует уникальность
	// при создании, но кто-то мог положить файл руками) — пытаемся ещё раз
	// с уникальным именем.
	if err := os.Rename(partialPath, finalPath); err != nil {
		s.log.Warn("finish: rename .vox-partial → final, retrying with unique",
			slog.String("partial", partialPath),
			slog.String("final", finalPath),
			logsafe.Err("err", err))
		name := s.fileNameForSha(st.fileSha)
		if alt, aerr := uniqueDownloadPath(name); aerr == nil {
			if rerr := os.Rename(partialPath, alt); rerr == nil {
				finalPath = alt
			} else {
				s.log.Error("finish: rename failed twice", logsafe.Err("err", rerr))
				return
			}
		} else {
			s.log.Error("finish: uniqueDownloadPath", logsafe.Err("err", aerr))
			return
		}
	}

	if err := cas.SetFileLocalPath(s.db, st.fileSha, finalPath); err != nil {
		s.log.Warn("finish: set local_path",
			slog.String("file", safeShort8(st.fileSha)),
			logsafe.Err("err", err))
	}
	if _, err := s.db.Exec(
		`UPDATE file_downloads SET partial_path = '' WHERE file_sha = ?`,
		st.fileSha); err != nil {
		s.log.Warn("finish: clear partial_path", logsafe.Err("err", err))
	}

	s.log.Info("📁 file ready in Vox Downloads",
		slog.String("file", safeShort8(st.fileSha)),
		slog.Int64("bytes", st.manifest.Size))

	s.emitFileReceived(st.fileSha, msgID)
}

// markStateInactive — peer не доступен; download приостанавливаем, state
// удаляем из активных. Файл .vox-partial остаётся на диске (resume по
// partial_path в БД). Закрываем handle чтобы не держать открытым в idle.
// markStateFailed — sender не отдаёт файл (NAK Removed/Forbidden или лимит
// NAK'ов): останавливаем pull, статус в БД "failed", UI возвращаем к кнопке
// «Скачать» (vox:file-cancelled). .vox-partial остаётся для повторной
// попытки. Раньше NAK-шторм крутился бесконечно, а кнопка «Скачать» молчала:
// startDownload видел «живой» state (lastActive обновлялся NAK'ами).
func (s *FilesService) markStateFailed(st *downloadState) {
	s.persistBitmap(st, "failed")
	s.mu.Lock()
	key := st.fileSha + "|" + st.sourcePeer
	if s.downloads[key] == st {
		delete(s.downloads, key)
	}
	s.mu.Unlock()
	st.mu.Lock()
	pf := st.partialFile
	st.partialFile = nil
	st.mu.Unlock()
	if pf != nil {
		_ = pf.Sync()
		_ = pf.Close()
	}
	st.closeCancel()
	s.emitFileCancelled(st.fileSha)
}

// markStateInactive — пауза (peer offline / files PC не поднялся / send
// failed): state снимается, bitmap остаётся 'pending' и OnPeerOnline
// перезапустит pull. UI получает cancelled, чтобы не висеть с «×» и 0 % —
// при авто-resume startDownload снова эмитит progress.
func (s *FilesService) markStateInactive(st *downloadState) {
	s.persistBitmap(st, "pending")
	s.mu.Lock()
	key := st.fileSha + "|" + st.sourcePeer
	// Identity-guard: startDownload мог заменить stale state под тем же
	// ключом — старый pullLoop не должен удалять/затирать новый.
	if s.downloads[key] == st {
		delete(s.downloads, key)
	}
	s.mu.Unlock()
	st.mu.Lock()
	pf := st.partialFile
	st.partialFile = nil
	st.mu.Unlock()
	if pf != nil {
		_ = pf.Sync()
		_ = pf.Close()
	}
	st.closeCancel()
	s.emitFileCancelled(st.fileSha)
}

// loadOrCreateBitmap — БД-инициализация. Если row уже есть (resume) — читаем
// bitmap, chunks_received и partial_path оттуда. Иначе — empty + INSERT.
// partialPath возвращается чтобы caller мог переоткрыть тот же .vox-partial
// файл на resume и не перекачивать с нуля.
func (s *FilesService) loadOrCreateBitmap(fileSha, peerID string, chunkCount int) (*Bitmap, string, error) {
	var data []byte
	var status string
	var partialPath string
	err := s.db.QueryRow(
		`SELECT received_bitmap, status, partial_path FROM file_downloads
		 WHERE file_sha = ? AND source_peer = ?`,
		fileSha, peerID).Scan(&data, &status, &partialPath)
	if err == nil {
		return LoadBitmap(data, chunkCount), partialPath, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, "", err
	}
	bitmap := NewBitmap(chunkCount)
	if _, err := s.db.Exec(
		`INSERT INTO file_downloads (file_sha, source_peer, received_bitmap, chunks_received, last_active_at, status, partial_path)
		 VALUES (?, ?, ?, 0, ?, 'active', '')`,
		fileSha, peerID, bitmap.Bytes(), time.Now().Unix()); err != nil {
		return nil, "", err
	}
	return bitmap, "", nil
}

// openOrResumePartialFile — открывает .vox-partial файл для приёма чанков
// напрямую через WriteAt. Если existingPartial непустой и файл существует —
// reopen без перетирания (resume). Если новый — uniqueDownloadPath для
// финального имени, partial = final + ".vox-partial", truncate до totalSize.
//
// truncate с totalSize >= текущего размера не обнуляет существующие байты —
// только аллоцирует пустые блоки в конце. На NTFS отложенно (real disk usage
// растёт по мере WriteAt'ов), на ext4/APFS аналогично.
//
// Возвращает (file, partialPath, finalPath, error). partialPath persistится
// в file_downloads.partial_path чтобы caller мог при resume снова найти файл.
func (s *FilesService) openOrResumePartialFile(fileSha, peerID, existingPartial string, totalSize int64) (*os.File, string, string, error) {
	var partialPath, finalPath string
	if existingPartial != "" && FileExists(existingPartial) {
		partialPath = existingPartial
		// Восстановление finalPath по partialPath: новые partial'ы лежат в
		// .vox-tmp/<basename>.partial → final = <parent of .vox-tmp>/<basename>.
		// Старые (legacy) partial'ы лежат в корне Vox Downloads с суффиксом
		// .vox-partial → final = trim ".vox-partial".
		dir := filepath.Dir(partialPath)
		base := filepath.Base(partialPath)
		if filepath.Base(dir) == partialSubdirName {
			finalPath = filepath.Join(filepath.Dir(dir), strings.TrimSuffix(base, ".partial"))
		} else {
			finalPath = strings.TrimSuffix(partialPath, ".vox-partial")
		}
	} else {
		name := s.fileNameForSha(fileSha)
		fp, err := uniqueDownloadPath(name)
		if err != nil {
			return nil, "", "", fmt.Errorf("uniqueDownloadPath: %w", err)
		}
		finalPath = fp
		pp, err := partialDownloadPath(finalPath)
		if err != nil {
			return nil, "", "", err
		}
		partialPath = pp
	}

	f, err := os.OpenFile(partialPath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, "", "", fmt.Errorf("open partial: %w", err)
	}
	if err := f.Truncate(totalSize); err != nil {
		_ = f.Close()
		return nil, "", "", fmt.Errorf("truncate partial: %w", err)
	}

	if existingPartial != partialPath {
		if _, err := s.db.Exec(
			`UPDATE file_downloads SET partial_path = ? WHERE file_sha = ? AND source_peer = ?`,
			partialPath, fileSha, peerID); err != nil {
			_ = f.Close()
			return nil, "", "", fmt.Errorf("persist partial_path: %w", err)
		}
	}
	return f, partialPath, finalPath, nil
}

// persistBitmap — сброс bitmap в БД. Вызывается из scheduleBitmapPersist
// (debounced) и финально при complete/inactive.
func (s *FilesService) persistBitmap(st *downloadState, newStatus string) {
	st.mu.Lock()
	bytes := append([]byte(nil), st.bitmap.Bytes()...)
	count := st.bitmap.Count()
	st.dirty = false
	st.mu.Unlock()
	if _, err := s.db.Exec(
		`UPDATE file_downloads SET received_bitmap = ?, chunks_received = ?,
		 last_active_at = ?, status = ? WHERE file_sha = ? AND source_peer = ?`,
		bytes, count, time.Now().Unix(), newStatus, st.fileSha, st.sourcePeer); err != nil {
		s.log.Warn("persist bitmap",
			slog.String("file", safeShort8(st.fileSha)),
			logsafe.Err("err", err))
	}
}

// scheduleBitmapPersist — debounced flush. После N мс без новых chunks — persist.
// Без debounce каждый chunk = UPDATE → SQLite WAL пухнет.
func (s *FilesService) scheduleBitmapPersist(st *downloadState) {
	// После терминального closeCancel (complete/cancelled/inactive) новый таймер
	// не взводим — отложенный persist со status="active" откатил бы финальный
	// статус в БД и на следующем старте вызвал ложный resume.
	select {
	case <-st.cancel:
		return
	default:
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.persistTimer != nil {
		st.persistTimer.Stop()
	}
	st.persistTimer = time.AfterFunc(bitmapPersistInterval, func() {
		select {
		case <-st.cancel:
			return
		default:
		}
		s.persistBitmap(st, "active")
	})
}

// ResumePendingDownloads — на старте приложения и при reconnect: смотрим
// downloads в pending/active state, перезапускаем pull-loop для тех чьи
// peers онлайн. 'paused' (отменено пользователем) и 'failed' не трогаем —
// только ручной StartFileDownload.
func (s *FilesService) ResumePendingDownloads() {
	rows, err := s.db.Query(
		`SELECT file_sha, source_peer FROM file_downloads
		 WHERE status IN ('pending', 'active')`)
	if err != nil {
		s.log.Warn("resume: list", logsafe.Err("err", err))
		return
	}
	defer rows.Close()

	type pair struct{ fileSha, peer string }
	var todo []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.fileSha, &p.peer); err == nil {
			todo = append(todo, p)
		}
	}
	rows.Close()

	for _, p := range todo {
		// Найдём какой message ссылается на этот fileSha (для emit'ов).
		var msgID, groupID string
		_ = s.db.QueryRow(
			`SELECT COALESCE(message_id, ''), COALESCE(group_msg_id, '')
			 FROM message_files WHERE file_sha = ? LIMIT 1`,
			p.fileSha).Scan(&msgID, &groupID)
		if msgID == "" && groupID == "" {
			continue
		}
		// Загружаем manifest для start.
		m, err := cas.LoadManifest(s.db, p.fileSha)
		if err != nil {
			continue
		}
		entry := wire.FileManifestEntry{
			Sha: m.Sha, Size: m.Size, ChunkSize: m.ChunkSize,
			ChunkCount: m.ChunkCount, Chunks: m.Chunks, Mime: m.Mime,
			Width: m.Width, Height: m.Height,
		}
		anyMsgID := msgID
		if anyMsgID == "" {
			anyMsgID = groupID
		}
		s.startDownload(p.peer, anyMsgID, groupID, &entry)
	}
}

// cloneEntry — глубокая копия chunks slice (чтобы downloadState не зависел
// от аллокаций parser'а frame'а).
func cloneEntry(e *wire.FileManifestEntry) *wire.FileManifestEntry {
	c := *e
	c.Chunks = append([]string(nil), e.Chunks...)
	return &c
}

// для совместимости — sync.Mutex placeholder если кому-то нужен от пакета.
var _ sync.Mutex
