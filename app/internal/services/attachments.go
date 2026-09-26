package services

import (
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
	"vox_app/internal/storage"

	"github.com/google/uuid"
)

// AttachmentInput — структура от фронтенда (см. frontend/src/modules/attach.js).
//   Kind: "photo" (сжатый JPEG) или "file" (сырые байты)
//   Data: base64 без data:URL префикса
//   Width/Height: размеры в пикселях для photo (после EXIF rotate). 0 для не-photo.
type AttachmentInput struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Mime   string `json:"mime"`
	Size   int64  `json:"size"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Data   string `json:"data"`
}

// AttachmentMeta — то что фронтенд получает обратно при чтении сообщения.
//   ID — UUID для последующих GetAttachmentBytes / DeleteAttachment.
//   Sha256 — для дедупликации на клиенте (cache).
//   Available — для CAS-файлов: true если все chunks локально на диске. Фронт
//     решает: рендерить как готовый (bubble + image) или показывать Download-кнопку.
//   FromPeer — кто прислал (для StartFileDownload надо знать у кого качать).
type AttachmentMeta struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Mime      string `json:"mime"`
	Size      int64  `json:"size"`
	Sha256    string `json:"sha256"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	Available bool   `json:"available"`
	FromPeer  string `json:"fromPeer,omitempty"`
	// Progress — доля уже скачанных чанков (0..1) для inbound CAS-файла,
	// который ещё не собран (остановленная/отменённая загрузка). Фронт
	// показывает «45 % · 61.9 MB» и полоску на карточке с кнопкой «Скачать».
	Progress float64 `json:"progress,omitempty"`
}

// ChatMediaItem — attachment + метаданные сообщения. Возвращается из
// GetChatMedia для рендера media-gallery (вкладка в info-panel).
type ChatMediaItem struct {
	AttachmentMeta
	MsgID     string `json:"msgId"`
	Timestamp int64  `json:"timestamp"` // ms
	Caption   string `json:"caption"`   // текст сообщения-носителя: поиск в медиа по подписи
}

// Лимиты применяются на бекенде. Backend ловит злонамеренные/багнутые клиенты.
// Размер до 100 GB на файл — рассчитан под передачу видео/архивов через path-based
// streaming (см. SendAttachmentsByPath). Старый base64-путь умирает на ~200 MB
// из-за heap, поэтому он отдельно ограничен maxBase64AttachmentSize.
const (
	maxAttachmentSize        = 100 * 1024 * 1024 * 1024 // 100 GB на файл (path-based)
	maxBase64AttachmentSize  = 50 * 1024 * 1024         // 50 MB на файл (legacy base64-через-Wails)
	maxAttachmentsPerMessage = 20                       // 20 файлов в одном сообщении
)

// SendAttachments сохраняет attachments на диск, создаёт DM-сообщение и
// связывает blob'ы через message_attachments. Phase 2: P2P transfer ещё не
// реализован — Delivered=false. Phase 3 добавит chunked send + ack.
func (s *MessagingService) SendAttachments(peerID string, atts []AttachmentInput, caption string) (*SendResult, error) {
	const op = "MessagingService.SendAttachments"

	if err := validatePeerID(peerID); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if err := validateAttachments(atts); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if s.blobs == nil {
		return nil, fmt.Errorf("%s: blob store not configured", op)
	}

	myID, err := s.sess.PeerID()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	// Сначала складываем blob'ы на диск; БД — строго внутри tx ниже.
	stored, err := s.putBlobsOnly(atts)
	if err != nil {
		s.cleanupOrphanBlobsOnDisk(stored)
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	msgID := uuid.New().String()
	now := time.Now().Unix()

	tx, err := s.store.DB().Begin()
	if err != nil {
		s.cleanupOrphanBlobsOnDisk(stored)
		return nil, fmt.Errorf("%s: begin: %w", op, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
			s.cleanupOrphanBlobsOnDisk(stored)
		}
	}()

	// content = caption (может быть пустым). type='attachment' — фронтенд по этому
	// типу подгружает message_attachments.
	if _, err := tx.Exec(
		`INSERT INTO messages (id, peer_id, sender_id, content, type, read, delivered, created_at)
		 VALUES (?, ?, ?, ?, 'attachment', 1, 0, ?)`,
		msgID, peerID, myID, caption, now); err != nil {
		return nil, fmt.Errorf("%s: insert message: %w", op, err)
	}

	if err := s.attachBlobsToMsgTx(tx, stored, atts, msgID, ""); err != nil {
		return nil, fmt.Errorf("%s: link: %w", op, err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("%s: commit: %w", op, err)
	}
	committed = true

	totalBytes := int64(0)
	for _, a := range atts {
		totalBytes += a.Size
	}
	s.log.Info("📎 ATTACHMENTS QUEUED",
		slog.String("peer", safeShort(peerID)),
		slog.String("msg_id", msgID),
		slog.Int("count", len(atts)),
		slog.Int64("bytes", totalBytes),
		slog.Int("caption_len", len(caption)))

	// Запускаем background P2P transfer. Если peer оффлайн — sendAttachmentsToPeer
	// делает early-return; повторная попытка случится в DeliverPendingMessages.
	go s.sendAttachmentsToPeer(peerID, msgID, caption)

	// Возвращаем сразу: фронтенд показывает bubble в статусе uploading,
	// прогресс приходит через vox:attach-progress events.
	return &SendResult{ID: msgID, Delivered: false}, nil
}

// AttachByPathInput — то что фронт передаёт в SendAttachmentsByPath.
//
//	Path     — абсолютный путь к файлу (получен через PickAttachments)
//	Name     — пользовательское имя (опц.); если пусто — basename(Path)
//	Kind     — "photo" | "file" (фронт сам решает по UI-радио)
//	Compress — true: ресайз до 1920px long edge + JPEG q85 (для kind=photo);
//	           false: байты идут как есть в свой mime
type AttachByPathInput struct {
	Path     string `json:"path"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Compress bool   `json:"compress"`
}

// SendAttachmentsByPath — path-based pipeline без base64 в Wails-IPC.
// В отличие от SendAttachments, файл стримится с диска (или через image-resize)
// прямо в BlobStore.Writer. Работает с файлами до maxAttachmentSize (100 GB).
//
// После INSERT в БД запускает background sendAttachmentsToPeer (тот же что у
// base64-пути) — поэтому P2P-передача чанков делится с легаси-кодом, и постепенно
// будет переключена на бинарный DataChannel "attachments".
func (s *MessagingService) SendAttachmentsByPath(peerID string, items []AttachByPathInput, caption string) (*SendResult, error) {
	const op = "MessagingService.SendAttachmentsByPath"

	if err := validatePeerID(peerID); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if err := validatePathInputs(items); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if s.blobs == nil {
		return nil, fmt.Errorf("%s: blob store not configured", op)
	}

	myID, err := s.sess.PeerID()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	stored, fakeAtts, err := s.putBlobsByPath(items)
	if err != nil {
		s.cleanupOrphanBlobsOnDisk(stored)
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	msgID := uuid.New().String()
	now := time.Now().Unix()

	tx, err := s.store.DB().Begin()
	if err != nil {
		s.cleanupOrphanBlobsOnDisk(stored)
		return nil, fmt.Errorf("%s: begin: %w", op, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
			s.cleanupOrphanBlobsOnDisk(stored)
		}
	}()

	if _, err := tx.Exec(
		`INSERT INTO messages (id, peer_id, sender_id, content, type, read, delivered, created_at)
		 VALUES (?, ?, ?, ?, 'attachment', 1, 0, ?)`,
		msgID, peerID, myID, caption, now); err != nil {
		return nil, fmt.Errorf("%s: insert message: %w", op, err)
	}

	if err := s.attachBlobsToMsgTx(tx, stored, fakeAtts, msgID, ""); err != nil {
		return nil, fmt.Errorf("%s: link: %w", op, err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("%s: commit: %w", op, err)
	}
	committed = true

	totalBytes := int64(0)
	for _, sb := range stored {
		totalBytes += sb.Bytes
	}
	s.log.Info("📎 ATTACHMENTS QUEUED (path-based)",
		slog.String("peer", safeShort(peerID)),
		slog.String("msg_id", msgID),
		slog.Int("count", len(items)),
		slog.Int64("bytes", totalBytes),
		slog.Int("caption_len", len(caption)))

	go s.sendAttachmentsToPeer(peerID, msgID, caption)

	return &SendResult{ID: msgID, Delivered: false}, nil
}

// SendGroupAttachmentsByPath — групповая версия SendAttachmentsByPath.
// Аналогично DM-варианту: stream с диска (или resize image), per-member tracking
// доставки через group_message_delivery, fan-out в sendAttachmentsToPeer.
func (s *MessagingService) SendGroupAttachmentsByPath(groupID string, items []AttachByPathInput, caption string) (*SendResult, error) {
	const op = "MessagingService.SendGroupAttachmentsByPath"

	if groupID == "" {
		return nil, fmt.Errorf("%s: group_id required", op)
	}
	if err := validatePathInputs(items); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if s.blobs == nil {
		return nil, fmt.Errorf("%s: blob store not configured", op)
	}
	if s.groups == nil {
		return nil, fmt.Errorf("%s: groups service not configured", op)
	}

	myID, err := s.sess.PeerID()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	var memberStatus string
	err = s.store.DB().QueryRow(
		"SELECT status FROM group_members WHERE group_id = ? AND peer_id = ?",
		groupID, myID).Scan(&memberStatus)
	if err != nil || memberStatus != "active" {
		return nil, fmt.Errorf("%s: not a member", op)
	}

	stored, fakeAtts, err := s.putBlobsByPath(items)
	if err != nil {
		s.cleanupOrphanBlobsOnDisk(stored)
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	msgID := uuid.New().String()
	now := time.Now().Unix()
	members := s.groups.GetGroupMemberIDs(groupID)

	tx, err := s.store.DB().Begin()
	if err != nil {
		s.cleanupOrphanBlobsOnDisk(stored)
		return nil, fmt.Errorf("%s: begin: %w", op, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
			s.cleanupOrphanBlobsOnDisk(stored)
		}
	}()

	if _, err := tx.Exec(
		`INSERT INTO group_messages (id, group_id, sender_id, content, type, created_at)
		 VALUES (?, ?, ?, ?, 'attachment', ?)`,
		msgID, groupID, myID, caption, now); err != nil {
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

	if err := s.attachBlobsToMsgTx(tx, stored, fakeAtts, "", msgID); err != nil {
		return nil, fmt.Errorf("%s: link: %w", op, err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("%s: commit: %w", op, err)
	}
	committed = true

	totalBytes := int64(0)
	for _, sb := range stored {
		totalBytes += sb.Bytes
	}
	s.log.Info("📎 GROUP ATTACHMENTS QUEUED (path-based)",
		slog.String("group", safeShort8(groupID)),
		slog.String("msg_id", msgID),
		slog.Int("count", len(items)),
		slog.Int64("bytes", totalBytes),
		slog.Int("members", len(members)))

	for _, peerID := range members {
		if peerID == myID {
			continue
		}
		go s.sendAttachmentsToPeer(peerID, msgID, caption)
	}

	return &SendResult{ID: msgID, Delivered: false}, nil
}

func validatePathInputs(items []AttachByPathInput) error {
	if len(items) == 0 {
		return fmt.Errorf("attachments empty")
	}
	if len(items) > maxAttachmentsPerMessage {
		return fmt.Errorf("too many attachments (%d > %d)", len(items), maxAttachmentsPerMessage)
	}
	for i, it := range items {
		if it.Path == "" {
			return fmt.Errorf("attachment %d: path empty", i)
		}
		if it.Kind != "photo" && it.Kind != "file" {
			return fmt.Errorf("attachment %d: invalid kind %q", i, it.Kind)
		}
		fi, err := os.Stat(it.Path)
		if err != nil {
			return fmt.Errorf("attachment %d (%s): stat: %w", i, filepath.Base(it.Path), err)
		}
		if fi.IsDir() {
			return fmt.Errorf("attachment %d: %s is a directory", i, it.Path)
		}
		if fi.Size() <= 0 {
			return fmt.Errorf("attachment %d (%s): empty file", i, fi.Name())
		}
		if fi.Size() > maxAttachmentSize {
			return fmt.Errorf("attachment %d: size %d > limit %d", i, fi.Size(), maxAttachmentSize)
		}
	}
	return nil
}

// SendGroupAttachments — то же для групповых сообщений.
// Per-member group_message_delivery записывается, но фактическая P2P доставка
// — Phase 3.
func (s *MessagingService) SendGroupAttachments(groupID string, atts []AttachmentInput, caption string) (*SendResult, error) {
	const op = "MessagingService.SendGroupAttachments"

	if groupID == "" {
		return nil, fmt.Errorf("%s: group_id required", op)
	}
	if err := validateAttachments(atts); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if s.blobs == nil {
		return nil, fmt.Errorf("%s: blob store not configured", op)
	}
	if s.groups == nil {
		return nil, fmt.Errorf("%s: groups service not configured", op)
	}

	myID, err := s.sess.PeerID()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	// Проверка членства (как в SendGroupMessage).
	var memberStatus string
	err = s.store.DB().QueryRow(
		"SELECT status FROM group_members WHERE group_id = ? AND peer_id = ?",
		groupID, myID).Scan(&memberStatus)
	if err != nil || memberStatus != "active" {
		return nil, fmt.Errorf("%s: not a member", op)
	}

	stored, err := s.putBlobsOnly(atts)
	if err != nil {
		s.cleanupOrphanBlobsOnDisk(stored)
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	msgID := uuid.New().String()
	now := time.Now().Unix()
	members := s.groups.GetGroupMemberIDs(groupID)

	tx, err := s.store.DB().Begin()
	if err != nil {
		s.cleanupOrphanBlobsOnDisk(stored)
		return nil, fmt.Errorf("%s: begin: %w", op, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
			s.cleanupOrphanBlobsOnDisk(stored)
		}
	}()

	if _, err := tx.Exec(
		`INSERT INTO group_messages (id, group_id, sender_id, content, type, created_at)
		 VALUES (?, ?, ?, ?, 'attachment', ?)`,
		msgID, groupID, myID, caption, now); err != nil {
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

	if err := s.attachBlobsToMsgTx(tx, stored, atts, "", msgID); err != nil {
		return nil, fmt.Errorf("%s: link: %w", op, err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("%s: commit: %w", op, err)
	}
	committed = true

	totalBytes := int64(0)
	for _, a := range atts {
		totalBytes += a.Size
	}
	s.log.Info("📎 GROUP ATTACHMENTS QUEUED",
		slog.String("group_id", safeShort8(groupID)),
		slog.String("msg_id", msgID),
		slog.Int("count", len(atts)),
		slog.Int64("bytes", totalBytes),
		slog.Int("members", len(members)-1))

	// Fanout: запускаем sender per-member параллельно.
	// Тот кто оффлайн — sendAttachmentsToPeer тихо выйдет; повторим в DeliverPendingMessages.
	for _, peerID := range members {
		if peerID == myID {
			continue
		}
		go s.sendAttachmentsToPeer(peerID, msgID, caption)
	}

	// Считаем доставленным если я один в группе (UI покажет ✓ сразу).
	// Для остальных случаев delivered отметится per-member внутри sendAttachmentsToPeer.
	delivered := len(members)-1 == 0
	return &SendResult{ID: msgID, Delivered: delivered}, nil
}

// GetAttachmentBytes возвращает расшифрованные байты blob'а по attachment ID.
func (s *MessagingService) GetAttachmentBytes(attachmentID string) ([]byte, error) {
	const op = "MessagingService.GetAttachmentBytes"
	if s.blobs == nil {
		return nil, fmt.Errorf("%s: blob store not configured", op)
	}
	var sha string
	err := s.store.DB().QueryRow(
		"SELECT sha256 FROM attachments WHERE id = ?", attachmentID).Scan(&sha)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%s: not found", op)
		}
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	data, err := s.blobs.Get(sha)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return data, nil
}

// DeleteAttachment уменьшает refcount; при достижении 0 удаляет blob с диска.
// Caller обычно НЕ вызывает напрямую — вызовы идут из DeleteMessage.
// Доступно как Wails-метод для ручной очистки orphan'ов из dev-tooling.
func (s *MessagingService) DeleteAttachment(attachmentID string) error {
	const op = "MessagingService.DeleteAttachment"
	return s.unlinkOneAttachment(attachmentID, op)
}

// RetryAttachmentSend перезапускает P2P-доставку attachment-сообщения.
// Находит сообщение (DM или group), вычисляет pending peers и стартует
// sendAttachmentsToPeer для каждого онлайн-пира. Для UI Retry-кнопки.
//
// Идемпотентно: если transfer уже в полёте, sendAttachmentsToPeer увидит
// существующий sendState и сразу выйдет. Оффлайн-пиры подхватит
// DeliverPendingMessages при их реконнекте.
func (s *MessagingService) RetryAttachmentSend(msgID string) error {
	const op = "MessagingService.RetryAttachmentSend"

	// DM?
	var peerID, caption string
	err := s.store.DB().QueryRow(
		`SELECT peer_id, content FROM messages
		 WHERE id = ? AND type = 'attachment' AND deleted = 0`, msgID).Scan(&peerID, &caption)
	if err == nil {
		go s.sendAttachmentsToPeer(peerID, msgID, caption)
		s.log.Info("📎 retry DM attach", slog.String("msg_id", msgID), slog.String("peer", safeShort(peerID)))
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s: lookup dm: %w", op, err)
	}

	// Group?
	var groupID string
	err = s.store.DB().QueryRow(
		`SELECT group_id, content FROM group_messages
		 WHERE id = ? AND type = 'attachment' AND deleted = 0`, msgID).Scan(&groupID, &caption)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%s: message not found or not an attachment", op)
		}
		return fmt.Errorf("%s: lookup group: %w", op, err)
	}
	if s.groups == nil {
		return fmt.Errorf("%s: groups service not configured", op)
	}

	// Pending members = те у кого delivered=0.
	rows, err := s.store.DB().Query(
		`SELECT peer_id FROM group_message_delivery WHERE msg_id = ? AND delivered = 0`, msgID)
	if err != nil {
		return fmt.Errorf("%s: pending members: %w", op, err)
	}
	var pendingPeers []string
	for rows.Next() {
		var pid string
		if err := rows.Scan(&pid); err == nil {
			pendingPeers = append(pendingPeers, pid)
		}
	}
	rows.Close()

	for _, pid := range pendingPeers {
		go s.sendAttachmentsToPeer(pid, msgID, caption)
	}
	s.log.Info("📎 retry group attach",
		slog.String("msg_id", msgID),
		slog.String("group_id", safeShort8(groupID)),
		slog.Int("peers", len(pendingPeers)))
	return nil
}

// --- Internal helpers ---

func validateAttachments(atts []AttachmentInput) error {
	if len(atts) == 0 {
		return fmt.Errorf("attachments empty")
	}
	if len(atts) > maxAttachmentsPerMessage {
		return fmt.Errorf("too many attachments (%d > %d)", len(atts), maxAttachmentsPerMessage)
	}
	for i, a := range atts {
		if a.Name == "" {
			return fmt.Errorf("attachment %d: name empty", i)
		}
		if a.Size <= 0 {
			return fmt.Errorf("attachment %d (%s): empty file (0 bytes)", i, a.Name)
		}
		// validateAttachments вызывается из base64-пути SendAttachments — там
		// reasonable cap 50 MB. Path-based pipeline свою валидацию делает отдельно.
		if a.Size > maxBase64AttachmentSize {
			return fmt.Errorf("attachment %d: size %d > base64 limit %d (use path-based send for large files)", i, a.Size, maxBase64AttachmentSize)
		}
		if a.Kind != "photo" && a.Kind != "file" {
			return fmt.Errorf("attachment %d: invalid kind %q", i, a.Kind)
		}
		if a.Data == "" {
			return fmt.Errorf("attachment %d: empty data", i)
		}
	}
	return nil
}

// storedBlob — internal результат записи на диск. Sha используется для
// привязки к attachments-row внутри tx. WasNew=true означает что этот blob
// впервые положен на диск текущим вызовом — при rollback он будет удалён.
type storedBlob struct {
	Sha    string
	Bytes  int64
	Mime   string
	WasNew bool
}

// putBlobsOnly декодирует base64, кладёт на диск через BlobStore.Put. Ничего
// в БД не трогает — это делает attachBlobsToMsgTx под единой транзакцией.
func (s *MessagingService) putBlobsOnly(atts []AttachmentInput) ([]storedBlob, error) {
	out := make([]storedBlob, len(atts))
	for i, a := range atts {
		raw, err := base64.StdEncoding.DecodeString(a.Data)
		if err != nil {
			return out, fmt.Errorf("att %d (%s): bad base64: %w", i, a.Name, err)
		}
		existed := s.blobs.Exists(storage.HashOf(raw))
		sha, err := s.blobs.Put(raw)
		if err != nil {
			return out, fmt.Errorf("att %d (%s): blob put: %w", i, a.Name, err)
		}
		out[i] = storedBlob{
			Sha:    sha,
			Bytes:  int64(len(raw)),
			Mime:   a.Mime,
			WasNew: !existed,
		}
	}
	return out, nil
}

// attachBlobsToMsgTx регистрирует attachments-row (или переиспользует по sha) и
// создаёт message_attachments + refcount++ — всё в пределах одной транзакции.
// Ровно одно из (msgID, groupMsgID) должно быть непустым.
func (s *MessagingService) attachBlobsToMsgTx(tx *sql.Tx, stored []storedBlob, inputs []AttachmentInput, msgID, groupMsgID string) error {
	if (msgID == "") == (groupMsgID == "") {
		return fmt.Errorf("attachBlobsToMsgTx: exactly one of msgID/groupMsgID must be set")
	}
	now := time.Now().Unix()
	for i, sb := range stored {
		var attID string
		err := tx.QueryRow("SELECT id FROM attachments WHERE sha256 = ?", sb.Sha).Scan(&attID)
		if errors.Is(err, sql.ErrNoRows) {
			attID = uuid.New().String()
			if _, err := tx.Exec(
				`INSERT INTO attachments (id, sha256, mime, size, width, height, path, refcount, created_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?)`,
				attID, sb.Sha, sb.Mime, sb.Bytes, inputs[i].Width, inputs[i].Height,
				s.blobs.PathRel(sb.Sha), now); err != nil {
				return fmt.Errorf("att %d: insert attachments: %w", i, err)
			}
		} else if err != nil {
			return fmt.Errorf("att %d: lookup sha: %w", i, err)
		}

		if msgID != "" {
			if _, err := tx.Exec(
				`INSERT INTO message_attachments (message_id, group_msg_id, attachment_id, ord, name, kind)
				 VALUES (?, NULL, ?, ?, ?, ?)`,
				msgID, attID, i, inputs[i].Name, inputs[i].Kind); err != nil {
				return fmt.Errorf("att %d: insert link (dm): %w", i, err)
			}
		} else {
			if _, err := tx.Exec(
				`INSERT INTO message_attachments (message_id, group_msg_id, attachment_id, ord, name, kind)
				 VALUES (NULL, ?, ?, ?, ?, ?)`,
				groupMsgID, attID, i, inputs[i].Name, inputs[i].Kind); err != nil {
				return fmt.Errorf("att %d: insert link (group): %w", i, err)
			}
		}
		if _, err := tx.Exec(
			"UPDATE attachments SET refcount = refcount + 1 WHERE id = ?", attID); err != nil {
			return fmt.Errorf("att %d: refcount++: %w", i, err)
		}
	}
	return nil
}

// UnlinkAttachmentsForMessage уменьшает refcount всех attachments сообщения и
// удаляет orphan blob'ы. Используется при DeleteMessage / DeleteGroupMessage.
// Принимает msgKind: "dm" (message_id) или "group" (group_msg_id).
// Public: GroupsService вызывает её через callback (см. main.go wire).
//
// Race-protection: сначала отменяем активные sender'ы для этого msgID — иначе
// goroutine может всё ещё читать blob'ы которые мы вот-вот удалим (видели
// "blob not found" в логах после соседнего DeleteMessage).
func (s *MessagingService) UnlinkAttachmentsForMessage(msgID, msgKind string) {
	s.cancelActiveSendersForMsg(msgID)
	const op = "MessagingService.unlinkAttachmentsForMessage"

	col := "message_id"
	if msgKind == "group" {
		col = "group_msg_id"
	}

	rows, err := s.store.DB().Query(
		"SELECT attachment_id FROM message_attachments WHERE "+col+" = ?", msgID)
	if err != nil {
		s.log.Warn(op+": list", slog.String("msg_id", msgID), slog.String("error", err.Error()))
		return
	}
	var attIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			attIDs = append(attIDs, id)
		}
	}
	rows.Close()

	if _, err := s.store.DB().Exec(
		"DELETE FROM message_attachments WHERE "+col+" = ?", msgID); err != nil {
		s.log.Warn(op+": delete links", slog.String("msg_id", msgID), slog.String("error", err.Error()))
	}

	for _, id := range attIDs {
		if err := s.unlinkOneAttachment(id, op); err != nil {
			s.log.Warn(op+": unlink", slog.String("att_id", id), slog.String("error", err.Error()))
		}
	}

	// Параллельно чистим CAS message_files. Файлы (file_sha) могут шариться
	// между сообщениями — DecFileRefcount уменьшит refcount, при достижении 0
	// удалит row и вернёт chunks к удалению. Caller (cas-cleanup hook) удалит
	// chunks физически через cas.DeleteChunk.
	if s.casCleanup != nil {
		s.casCleanup(msgID, msgKind)
	}
}

// SetCASCleanup инжектится из main.go. Хук вызывается из UnlinkAttachmentsForMessage
// для удаления message_files-строк и orphan chunks (через FilesService).
func (s *MessagingService) SetCASCleanup(fn func(msgID, msgKind string)) {
	s.casCleanup = fn
}

// SetFilesRetryUploadDM инжектит FilesService.RetryUploadDM. Используется в
// DeliverPendingMessages для CAS-сообщений (message_files-table).
func (s *MessagingService) SetFilesRetryUploadDM(fn func(peerID, msgID string)) {
	s.filesRetryUploadDM = fn
}

// casFileAvailable — true если файл доступен для использования (single-file
// модель: source of truth = local_path в Vox Downloads). Если файл удалён
// извне — сбрасываем local_path и возвращаем false, bubble показывает Download.
func (s *MessagingService) casFileAvailable(fileSha string) bool {
	var localPath string
	_ = s.store.DB().QueryRow(`SELECT local_path FROM files WHERE sha = ?`, fileSha).Scan(&localPath)
	if localPath == "" {
		return false
	}
	if st, err := os.Stat(localPath); err == nil && !st.IsDir() {
		return true
	}
	// Файл удалён извне — reset, чтобы UI показал Download.
	_, _ = s.store.DB().Exec(`UPDATE files SET local_path = '' WHERE sha = ?`, fileSha)
	return false
}

// casFileProgress — доля скачанных чанков (0..1) для ещё не собранного
// CAS-файла: max(file_downloads.chunks_received) по всем source_peer /
// files.chunk_count. 0 — ничего не качали или нет строки.
func (s *MessagingService) casFileProgress(fileSha string) float64 {
	var received, count int
	_ = s.store.DB().QueryRow(
		`SELECT COALESCE(MAX(d.chunks_received), 0), COALESCE(f.chunk_count, 0)
		 FROM files f LEFT JOIN file_downloads d ON d.file_sha = f.sha
		 WHERE f.sha = ?`, fileSha).Scan(&received, &count)
	if count <= 0 || received <= 0 {
		return 0
	}
	if received >= count {
		return 1
	}
	return float64(received) / float64(count)
}

// unlinkOneAttachment: refcount--, если стало 0 — удалить blob+row.
func (s *MessagingService) unlinkOneAttachment(attachmentID, op string) error {
	tx, err := s.store.DB().Begin()
	if err != nil {
		return fmt.Errorf("%s: begin: %w", op, err)
	}
	defer tx.Rollback()

	var sha string
	var rc int
	err = tx.QueryRow(
		"SELECT sha256, refcount FROM attachments WHERE id = ?", attachmentID).Scan(&sha, &rc)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // already gone
		}
		return fmt.Errorf("%s: select: %w", op, err)
	}
	if rc <= 1 {
		if _, err := tx.Exec("DELETE FROM attachments WHERE id = ?", attachmentID); err != nil {
			return fmt.Errorf("%s: delete row: %w", op, err)
		}
	} else {
		if _, err := tx.Exec(
			"UPDATE attachments SET refcount = refcount - 1 WHERE id = ?", attachmentID); err != nil {
			return fmt.Errorf("%s: refcount--: %w", op, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: commit: %w", op, err)
	}

	// После commit'а: если sha больше нигде не используется (другая row с тем же
	// sha не должна существовать благодаря UNIQUE — поэтому достаточно проверить
	// что мы удалили последнюю row).
	if rc <= 1 && s.blobs != nil {
		if err := s.blobs.Delete(sha); err != nil {
			s.log.Warn(op+": blob delete", slog.String("sha", safeShort8(sha)), slog.String("error", err.Error()))
		}
	}
	return nil
}

// cleanupOrphanBlobsOnDisk — НЕ удаляем blob'ы немедленно. Раньше делали SELECT
// + Delete, но между этими шагами параллельный receiver мог Finalize() тот же
// sha и начать свою tx → мы удалили бы файл, на который потом сослалась его
// committed row. Окно микро, но возможно при rollback под нагрузкой.
//
// Решение: orphan blob'ы (диск без БД-row) подбирает периодический disk-scan
// внутри CleanupStaleAttachments. Это стоит одно-двух прогонов раз в сутки
// и не имеет race conditions.
//
// Сигнатура сохранена ради вызовов из SendAttachments / SendGroupAttachments
// — функция работает как маркер «отказались от tx», без побочных эффектов.
func (s *MessagingService) cleanupOrphanBlobsOnDisk(stored []storedBlob) {
	if len(stored) == 0 {
		return
	}
	news := 0
	for _, sb := range stored {
		if sb.WasNew {
			news++
		}
	}
	if news > 0 {
		s.log.Debug("attach: tx rollback — orphan blob(s) deferred to periodic GC",
			slog.Int("orphan_candidates", news))
	}
}

// LoadAttachmentsForMessage возвращает []AttachmentMeta для одного сообщения.
// Сливает оба источника: legacy message_attachments и новый message_files (CAS).
// Сортируется по ord — оба источника соблюдают порядок.
func (s *MessagingService) LoadAttachmentsForMessage(msgID, msgKind string) []AttachmentMeta {
	col := "message_id"
	if msgKind == "group" {
		col = "group_msg_id"
	}

	type row struct {
		ord int
		m   AttachmentMeta
	}
	var all []row

	// Legacy: message_attachments → attachments.
	rowsA, err := s.store.DB().Query(
		`SELECT ma.ord, ma.attachment_id, ma.name, ma.kind, a.mime, a.size, a.sha256, a.width, a.height
		 FROM message_attachments ma
		 JOIN attachments a ON a.id = ma.attachment_id
		 WHERE ma.`+col+` = ?`, msgID)
	if err == nil {
		for rowsA.Next() {
			var r row
			if err := rowsA.Scan(&r.ord, &r.m.ID, &r.m.Name, &r.m.Kind, &r.m.Mime, &r.m.Size, &r.m.Sha256, &r.m.Width, &r.m.Height); err == nil {
				all = append(all, r)
			}
		}
		rowsA.Close()
	}

	// New CAS: message_files → files. Заполняем Available проверкой что все
	// chunks локально на диске (cas.HasChunk через инжектированный hook).
	// FromPeer — sender сообщения (для StartFileDownload надо знать у кого качать).
	var senderID string
	if msgKind == "group" {
		_ = s.store.DB().QueryRow(`SELECT sender_id FROM group_messages WHERE id = ?`, msgID).Scan(&senderID)
	} else {
		_ = s.store.DB().QueryRow(`SELECT sender_id FROM messages WHERE id = ?`, msgID).Scan(&senderID)
	}

	rowsB, err := s.store.DB().Query(
		`SELECT mf.ord, mf.file_sha, mf.name, mf.kind, f.mime, f.size, f.width, f.height
		 FROM message_files mf
		 JOIN files f ON f.sha = mf.file_sha
		 WHERE mf.`+col+` = ?`, msgID)
	if err == nil {
		for rowsB.Next() {
			var r row
			if err := rowsB.Scan(&r.ord, &r.m.Sha256, &r.m.Name, &r.m.Kind, &r.m.Mime, &r.m.Size, &r.m.Width, &r.m.Height); err == nil {
				r.m.ID = r.m.Sha256
				r.m.FromPeer = senderID
				r.m.Available = s.casFileAvailable(r.m.Sha256)
				if !r.m.Available {
					r.m.Progress = s.casFileProgress(r.m.Sha256)
				}
				all = append(all, r)
			}
		}
		rowsB.Close()
	}

	// Сортируем по ord. Простая сортировка вручную чтобы не тащить sort.Slice
	// для типичных N=1..20.
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && all[j-1].ord > all[j].ord; j-- {
			all[j-1], all[j] = all[j], all[j-1]
		}
	}
	out := make([]AttachmentMeta, len(all))
	for i, r := range all {
		out[i] = r.m
	}
	return out
}

// SetBlobStore инжектится из main.go после инициализации (избегаем менять
// сигнатуру конструктора, чтобы не ломать смежные сервисы).
func (s *MessagingService) SetBlobStore(b *storage.BlobStore) {
	s.blobs = b
}

// SetAssetBridge инжектится из main.go. Используется PrepareAttachments
// для регистрации tokens на локальные файлы (превью в picker-модалке).
func (s *MessagingService) SetAssetBridge(b *AssetBridge) {
	s.bridge = b
}

// RestartAttachmentDelivery — публичный wrapper для sendAttachmentsToPeer.
// Используется как callback от GroupsService.DeliverPendingGroupMessages,
// чтобы при reconnect peer'а перезапустить chunked attachment transfer.
func (s *MessagingService) RestartAttachmentDelivery(peerID, msgID, caption string) {
	go s.sendAttachmentsToPeer(peerID, msgID, caption)
}

// GetChatMedia возвращает attachments для конкретного чата (DM или группа).
// kind: "all" / "photo" / "file". Сортировка — по time DESC (новые сверху).
//
// Один из peerID / groupID должен быть непустым.
func (s *MessagingService) GetChatMedia(peerID, groupID, kind string, limit, offset int) ([]ChatMediaItem, error) {
	const op = "MessagingService.GetChatMedia"

	if (peerID == "") == (groupID == "") {
		return nil, fmt.Errorf("%s: exactly one of peerID/groupID required", op)
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}

	var query string
	var args []any

	// UNION ALL legacy (message_attachments) + new CAS (message_files).
	// Сортировка по created_at DESC + ord — обе ветки в одном выводе.
	kindFilterMA := ""
	kindFilterMF := ""
	if kind == "photo" || kind == "file" {
		kindFilterMA = " AND ma.kind = ?"
		kindFilterMF = " AND mf.kind = ?"
	}

	if peerID != "" {
		query = `SELECT * FROM (
		    SELECT ma.attachment_id AS att_id, ma.name AS name, ma.kind AS kind,
		           a.mime AS mime, a.size AS size, a.sha256 AS sha,
		           a.width AS w, a.height AS h, ma.ord AS ord,
		           m.id AS msg_id, m.created_at AS ts, 0 AS source, m.content AS caption
		    FROM message_attachments ma
		    JOIN attachments a ON a.id = ma.attachment_id
		    JOIN messages m ON m.id = ma.message_id
		    WHERE m.peer_id = ? AND m.deleted = 0` + kindFilterMA + `
		    UNION ALL
		    SELECT mf.file_sha AS att_id, mf.name, mf.kind,
		           f.mime, f.size, mf.file_sha AS sha,
		           f.width, f.height, mf.ord,
		           m.id, m.created_at, 1 AS source, m.content
		    FROM message_files mf
		    JOIN files f ON f.sha = mf.file_sha
		    JOIN messages m ON m.id = mf.message_id
		    WHERE m.peer_id = ? AND m.deleted = 0` + kindFilterMF + `
		) ORDER BY ts DESC, ord ASC LIMIT ? OFFSET ?`
		args = append(args, peerID)
		if kindFilterMA != "" {
			args = append(args, kind)
		}
		args = append(args, peerID)
		if kindFilterMF != "" {
			args = append(args, kind)
		}
		args = append(args, limit, offset)
	} else {
		query = `SELECT * FROM (
		    SELECT ma.attachment_id, ma.name, ma.kind,
		           a.mime, a.size, a.sha256,
		           a.width, a.height, ma.ord,
		           gm.id, gm.created_at, 0 AS source, gm.content
		    FROM message_attachments ma
		    JOIN attachments a ON a.id = ma.attachment_id
		    JOIN group_messages gm ON gm.id = ma.group_msg_id
		    WHERE gm.group_id = ? AND gm.deleted = 0` + kindFilterMA + `
		    UNION ALL
		    SELECT mf.file_sha, mf.name, mf.kind,
		           f.mime, f.size, mf.file_sha,
		           f.width, f.height, mf.ord,
		           gm.id, gm.created_at, 1 AS source, gm.content
		    FROM message_files mf
		    JOIN files f ON f.sha = mf.file_sha
		    JOIN group_messages gm ON gm.id = mf.group_msg_id
		    WHERE gm.group_id = ? AND gm.deleted = 0` + kindFilterMF + `
		) ORDER BY created_at DESC, ord ASC LIMIT ? OFFSET ?`
		args = append(args, groupID)
		if kindFilterMA != "" {
			args = append(args, kind)
		}
		args = append(args, groupID)
		if kindFilterMF != "" {
			args = append(args, kind)
		}
		args = append(args, limit, offset)
	}

	rows, err := s.store.DB().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer rows.Close()

	out := []ChatMediaItem{}
	for rows.Next() {
		var item ChatMediaItem
		var createdAt int64
		var ord int
		var source int
		if err := rows.Scan(
			&item.ID, &item.Name, &item.Kind, &item.Mime, &item.Size, &item.Sha256,
			&item.Width, &item.Height, &ord,
			&item.MsgID, &createdAt, &source, &item.Caption,
		); err != nil {
			s.log.Warn(op+": scan", slog.String("err", err.Error()))
			continue
		}
		item.Timestamp = createdAt * 1000
		// Для CAS-source проверяем локальную доступность (только реально
		// скачанные файлы попадают в медиа-вкладку — иначе юзер видел бы
		// «загруженные» файлы которых на диске нет, click открывал бы 500).
		if source == 1 {
			item.Available = s.casFileAvailable(item.Sha256)
			if !item.Available {
				continue // фильтр: не показываем нескачанные
			}
		} else {
			item.Available = true // legacy: считаем что blob есть
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// ClearChatMedia — удаляет ВСЕ attachment-сообщения в чате (DM или группе).
// Для каждого зовёт UnlinkAttachmentsForMessage → cleanup blobs (legacy)
// + UnlinkFilesForMessage через CAS hook → DecFileRefcount → DeleteChunk.
//
// Само сообщение помечается deleted=1 (soft-delete) — UI больше его не показывает.
// Возвращает кол-во удалённых сообщений и освобождённых байт (приблизительно
// по сумме file.size; реальный disk-saving может быть меньше из-за dedup
// между разными файлами/чатами).
//
// Один из peerID/groupID должен быть непустым.
func (s *MessagingService) ClearChatMedia(peerID, groupID string) (*ClearChatMediaResult, error) {
	const op = "MessagingService.ClearChatMedia"
	if (peerID == "") == (groupID == "") {
		return nil, fmt.Errorf("%s: exactly one of peerID/groupID required", op)
	}

	var msgIDs []string
	var freed int64
	var msgKind string

	if peerID != "" {
		msgKind = "dm"
		rows, err := s.store.DB().Query(
			`SELECT id FROM messages WHERE peer_id = ? AND type = 'attachment' AND deleted = 0`,
			peerID)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err == nil {
				msgIDs = append(msgIDs, id)
			}
		}
		rows.Close()
	} else {
		msgKind = "group"
		rows, err := s.store.DB().Query(
			`SELECT id FROM group_messages WHERE group_id = ? AND type = 'attachment' AND deleted = 0`,
			groupID)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err == nil {
				msgIDs = append(msgIDs, id)
			}
		}
		rows.Close()
	}

	for _, msgID := range msgIDs {
		// Считаем освобождённые байты ДО unlink (после row не будет).
		atts := s.LoadAttachmentsForMessage(msgID, msgKind)
		for _, a := range atts {
			freed += a.Size
		}
		s.UnlinkAttachmentsForMessage(msgID, msgKind)
		// Soft-delete сообщения чтобы оно не показывалось в чате.
		if peerID != "" {
			_, _ = s.store.DB().Exec(
				`UPDATE messages SET deleted = 1 WHERE id = ?`, msgID)
		} else {
			_, _ = s.store.DB().Exec(
				`UPDATE group_messages SET deleted = 1 WHERE id = ?`, msgID)
		}
	}

	s.log.Info("🧹 ChatMedia cleared",
		slog.String("kind", msgKind),
		slog.Int("messages", len(msgIDs)),
		slog.Int64("bytes_freed", freed))

	return &ClearChatMediaResult{
		Messages:   len(msgIDs),
		BytesFreed: freed,
	}, nil
}

// ClearChatMediaResult — возвращается фронту для toast'а «N сообщений, X MB освобождено».
type ClearChatMediaResult struct {
	Messages   int   `json:"messages"`
	BytesFreed int64 `json:"bytesFreed"`
}

// ForwardAttachment пересылает существующий attachment (по ID) в другой чат.
// Не перезаливает blob: новая message_attachments-row ссылается на тот же
// attachments-row, refcount инкрементится. Если получатель уже имеет blob
// (sha dedup) — chunked-transfer пропустит чанки и сразу зарегистрирует у себя.
//
// dstPeerID или dstGroupID должен быть непустым (взаимоисключающие).
func (s *MessagingService) ForwardAttachment(attachmentID, dstPeerID, dstGroupID, caption string) (*SendResult, error) {
	const op = "MessagingService.ForwardAttachment"

	if (dstPeerID == "") == (dstGroupID == "") {
		return nil, fmt.Errorf("%s: exactly one destination required", op)
	}
	if s.blobs == nil {
		return nil, fmt.Errorf("%s: blob store not configured", op)
	}

	// 1. Проверить что attachment существует. Для name/kind берём самое раннее
	// известное message_attachments-использование (детерминированный ORDER BY ma.id)
	// — это изначальное имя, с которым файл был загружен/получен.
	var sha, mime string
	var size int64
	var width, height int
	if err := s.store.DB().QueryRow(
		`SELECT sha256, mime, size, width, height FROM attachments WHERE id = ?`,
		attachmentID).Scan(&sha, &mime, &size, &width, &height); err != nil {
		return nil, fmt.Errorf("%s: source not found: %w", op, err)
	}

	var name, kind string
	err := s.store.DB().QueryRow(
		`SELECT name, kind FROM message_attachments
		 WHERE attachment_id = ? ORDER BY id ASC LIMIT 1`,
		attachmentID).Scan(&name, &kind)
	if err != nil {
		// Orphan attachment-row (крайне редко: все сообщения удалены, но refcount>0).
		// Используем безопасный fallback — без него имя пустое и файл приедет как "".
		name = "attachment-" + safeShort8(sha)
		kind = "file"
	}

	myID, err := s.sess.PeerID()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	msgID := uuid.New().String()
	now := time.Now().Unix()

	// Members группы собираем один раз ДО tx и используем как для delivery-rows,
	// так и для fanout. Иначе между двумя вызовами GetGroupMemberIDs (внутри tx
	// и в fanout после commit) состав группы может разойтись.
	var members []string
	if dstGroupID != "" {
		members = s.groups.GetGroupMemberIDs(dstGroupID)
	}

	tx, err := s.store.DB().Begin()
	if err != nil {
		return nil, fmt.Errorf("%s: begin: %w", op, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if dstGroupID != "" {
		var status string
		_ = tx.QueryRow(
			"SELECT status FROM group_members WHERE group_id = ? AND peer_id = ?",
			dstGroupID, myID).Scan(&status)
		if status != "active" {
			return nil, fmt.Errorf("%s: not a member of group", op)
		}
		if _, err := tx.Exec(
			`INSERT INTO group_messages (id, group_id, sender_id, content, type, created_at)
			 VALUES (?, ?, ?, ?, 'attachment', ?)`,
			msgID, dstGroupID, myID, caption, now); err != nil {
			return nil, fmt.Errorf("%s: insert group msg: %w", op, err)
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
		if _, err := tx.Exec(
			`INSERT INTO message_attachments (message_id, group_msg_id, attachment_id, ord, name, kind)
			 VALUES (NULL, ?, ?, 0, ?, ?)`,
			msgID, attachmentID, name, kind); err != nil {
			return nil, fmt.Errorf("%s: link group: %w", op, err)
		}
	} else {
		if _, err := tx.Exec(
			`INSERT INTO messages (id, peer_id, sender_id, content, type, read, delivered, created_at)
			 VALUES (?, ?, ?, ?, 'attachment', 1, 0, ?)`,
			msgID, dstPeerID, myID, caption, now); err != nil {
			return nil, fmt.Errorf("%s: insert dm: %w", op, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO message_attachments (message_id, group_msg_id, attachment_id, ord, name, kind)
			 VALUES (?, NULL, ?, 0, ?, ?)`,
			msgID, attachmentID, name, kind); err != nil {
			return nil, fmt.Errorf("%s: link dm: %w", op, err)
		}
	}

	if _, err := tx.Exec(
		"UPDATE attachments SET refcount = refcount + 1 WHERE id = ?", attachmentID); err != nil {
		return nil, fmt.Errorf("%s: refcount++: %w", op, err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("%s: commit: %w", op, err)
	}
	committed = true

	s.log.Info("📎 ATTACHMENT FORWARDED",
		slog.String("att_id", attachmentID),
		slog.String("sha", safeShort8(sha)),
		slog.String("new_msg_id", msgID),
		slog.Int64("size", size))

	if dstGroupID != "" {
		for _, peerID := range members {
			if peerID == myID {
				continue
			}
			go s.sendAttachmentsToPeer(peerID, msgID, caption)
		}
		delivered := len(members)-1 == 0
		return &SendResult{ID: msgID, Delivered: delivered}, nil
	}

	go s.sendAttachmentsToPeer(dstPeerID, msgID, caption)
	return &SendResult{ID: msgID, Delivered: false}, nil
}

// CleanupStaleAttachments — periodic GC для:
//   1. Pending attachment-сообщений старше 7 дней (peer не вернулся за это время)
//      → soft-delete сообщения + unlink attachments → blob освобождается если refcount=0
//   2. Orphan attachments-rows (refcount=0) — защита от багов когда unlink не довёл blob
//      delete до конца (например, crash mid-операция)
//
// Вызывается из main.go: при старте + каждые 24 часа.
func (s *MessagingService) CleanupStaleAttachments() {
	const op = "MessagingService.CleanupStaleAttachments"
	const pendingTTL = 7 * 24 * time.Hour

	cutoff := time.Now().Add(-pendingTTL).Unix()

	// 1. DM: pending attachment сообщения
	rows, err := s.store.DB().Query(
		`SELECT id FROM messages
		 WHERE type = 'attachment' AND delivered = 0 AND deleted = 0 AND created_at < ?`,
		cutoff)
	if err != nil {
		s.log.Warn(op+": list dm", slog.String("err", err.Error()))
	} else {
		var stale []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err == nil {
				stale = append(stale, id)
			}
		}
		rows.Close()
		for _, id := range stale {
			if _, err := s.store.DB().Exec(
				"UPDATE messages SET deleted = 1 WHERE id = ?", id); err != nil {
				s.log.Warn(op+": soft-delete dm", slog.String("id", id), slog.String("err", err.Error()))
				continue
			}
			s.UnlinkAttachmentsForMessage(id, "dm")
		}
		if len(stale) > 0 {
			s.log.Info("🧹 stale DM attachments cleaned",
				slog.Int("count", len(stale)),
				slog.Duration("ttl", pendingTTL))
		}
	}

	// 2. Group: pending group attachments — message delivery row stale
	//    (группа считается pending если есть hоть один pending member > TTL)
	rows, err = s.store.DB().Query(
		`SELECT DISTINCT gm.id FROM group_messages gm
		 JOIN group_message_delivery d ON d.msg_id = gm.id
		 WHERE gm.type = 'attachment' AND gm.deleted = 0 AND gm.created_at < ?
		 GROUP BY gm.id
		 HAVING SUM(CASE WHEN d.delivered = 0 THEN 1 ELSE 0 END) > 0`,
		cutoff)
	if err != nil {
		s.log.Warn(op+": list group", slog.String("err", err.Error()))
	} else {
		var stale []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err == nil {
				stale = append(stale, id)
			}
		}
		rows.Close()
		for _, id := range stale {
			if _, err := s.store.DB().Exec(
				"UPDATE group_messages SET deleted = 1 WHERE id = ?", id); err != nil {
				continue
			}
			s.UnlinkAttachmentsForMessage(id, "group")
		}
		if len(stale) > 0 {
			s.log.Info("🧹 stale GROUP attachments cleaned",
				slog.Int("count", len(stale)))
		}
	}

	// 3. Orphan attachments-rows (refcount=0) — защита от багов
	if s.blobs != nil {
		orphanRows, err := s.store.DB().Query(
			"SELECT id, sha256 FROM attachments WHERE refcount = 0")
		if err == nil {
			type orphan struct{ id, sha string }
			var orphans []orphan
			for orphanRows.Next() {
				var o orphan
				if err := orphanRows.Scan(&o.id, &o.sha); err == nil {
					orphans = append(orphans, o)
				}
			}
			orphanRows.Close()
			for _, o := range orphans {
				_, _ = s.store.DB().Exec("DELETE FROM attachments WHERE id = ?", o.id)
				if err := s.blobs.Delete(o.sha); err != nil {
					s.log.Warn(op+": orphan blob delete",
						slog.String("sha", safeShort8(o.sha)),
						slog.String("err", err.Error()))
				}
			}
			if len(orphans) > 0 {
				s.log.Info("🧹 orphan blobs cleaned", slog.Int("count", len(orphans)))
			}
		}
	}

	// 4. Orphan blob-files (на диске, без row в attachments). Возникают когда
	// SendAttachments положил blob, но tx упала и rollback'нулся. Раньше мы
	// чистили это immediate, что давало race с конкурентным receiver Finalize.
	// Теперь — только периодический disk-scan, без race.
	if s.blobs != nil {
		s.gcOrphanBlobFiles()
	}
}

// gcOrphanBlobFiles обходит attachments/<aa>/<sha>.bin, для каждого .bin
// проверяет наличие row в attachments. Нет row → файл orphan, удаляем.
//
// Grace period: blob моложе orphanBlobGrace не трогаем. Это закрывает race
// между BlobWriter.Finalize() (который физически переименовывает tmp→final.bin)
// и finalizeReceivedAttachments() (который INSERT row). Без grace GC мог удалить
// только что принятый blob до того как sender'ский INSERT прошёл — потом фронт
// получал 500 на /vox-attach/<sha>. Видели в логах:
//   21:09:34 orphan FILES cleaned count=7
//   21:09:54 ATTACH RECEIVED + 5 vox-attach: blob not found.
const orphanBlobGrace = 10 * time.Minute

func (s *MessagingService) gcOrphanBlobFiles() {
	root := s.blobs.Root()
	dirs, err := os.ReadDir(root)
	if err != nil {
		s.log.Warn("attach gc: ReadDir root", slog.String("err", err.Error()))
		return
	}
	cutoff := time.Now().Add(-orphanBlobGrace)
	deleted := 0
	skippedYoung := 0
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		if len(d.Name()) != 2 {
			continue
		}
		shardPath := filepath.Join(root, d.Name())
		files, err := os.ReadDir(shardPath)
		if err != nil {
			continue
		}
		for _, f := range files {
			name := f.Name()
			if !strings.HasSuffix(name, ".bin") {
				continue
			}
			sha := strings.TrimSuffix(name, ".bin")
			if len(sha) != 64 {
				continue
			}
			var rowID string
			err := s.store.DB().QueryRow(
				"SELECT id FROM attachments WHERE sha256 = ?", sha).Scan(&rowID)
			if err == nil {
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				continue
			}
			// Orphan-row check прошёл. Теперь grace по mtime — иначе зачищаем
			// blob который только что записал receiver, а row ещё не успел сесть.
			info, ierr := f.Info()
			if ierr == nil && info.ModTime().After(cutoff) {
				skippedYoung++
				continue
			}
			if err := s.blobs.Delete(sha); err != nil {
				s.log.Warn("attach gc: delete orphan file",
					slog.String("sha", safeShort8(sha)),
					slog.String("err", err.Error()))
				continue
			}
			deleted++
		}
	}
	if deleted > 0 || skippedYoung > 0 {
		s.log.Info("🧹 orphan blob FILES scan",
			slog.Int("deleted", deleted),
			slog.Int("skipped_young", skippedYoung))
	}
}

// StartCleanupLoop запускает фоновую очистку — раз при старте + каждые 24 часа.
func (s *MessagingService) StartCleanupLoop() {
	go func() {
		// Сразу: integrity-check для починки legacy state (row есть, blob нет).
		// Был баг GC до 2026-04-23 → orphan blobs удалялись пока row in flight,
		// у receiver'ов остались "битые" attachments-rows; bubble на них шлёт
		// /vox-attach/<sha> → 404. Зачищаем такие row сразу при старте — bubble
		// перестаёт их рисовать, caption остаётся.
		s.cleanupBrokenAttachmentRows()

		// At-startup full cleanup
		time.Sleep(30 * time.Second) // даём приложению полностью запуститься
		s.CleanupStaleAttachments()

		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			s.CleanupStaleAttachments()
		}
	}()
}

// cleanupBrokenAttachmentRows: для каждой attachments-row проверяем наличие
// blob на диске. Если blob отсутствует — удаляем row + все message_attachments
// которые на неё ссылаются. Сообщение остаётся в БД с пустыми attachments.
//
// Это идемпотентная операция; на здоровой установке возвращает почти сразу
// (один SELECT по индексу + os.Stat для каждой row).
func (s *MessagingService) cleanupBrokenAttachmentRows() {
	if s.blobs == nil {
		return
	}
	rows, err := s.store.DB().Query(`SELECT id, sha256 FROM attachments`)
	if err != nil {
		s.log.Warn("integrity: select attachments", slog.String("err", err.Error()))
		return
	}
	type brokenRow struct {
		ID  string
		Sha string
	}
	var broken []brokenRow
	for rows.Next() {
		var r brokenRow
		if err := rows.Scan(&r.ID, &r.Sha); err != nil {
			continue
		}
		if !s.blobs.Exists(r.Sha) {
			broken = append(broken, r)
		}
	}
	rows.Close()

	if len(broken) == 0 {
		return
	}
	deletedRows := 0
	deletedLinks := 0
	for _, b := range broken {
		// Удаляем сначала message_attachments-связи (NULL ON DELETE не задано),
		// потом саму attachments-row.
		res, err := s.store.DB().Exec(
			`DELETE FROM message_attachments WHERE attachment_id = ?`, b.ID)
		if err != nil {
			s.log.Warn("integrity: delete links",
				slog.String("att_id", b.ID),
				slog.String("err", err.Error()))
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			deletedLinks += int(n)
		}
		if _, err := s.store.DB().Exec(
			`DELETE FROM attachments WHERE id = ?`, b.ID); err != nil {
			s.log.Warn("integrity: delete row",
				slog.String("att_id", b.ID),
				slog.String("err", err.Error()))
			continue
		}
		deletedRows++
	}
	s.log.Info("🧹 integrity: cleaned broken attachment rows",
		slog.Int("rows", deletedRows),
		slog.Int("links", deletedLinks))
}
