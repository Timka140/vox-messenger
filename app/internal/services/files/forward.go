package files

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"vox_app/internal/services/files/wire"
	"vox_app/internal/storage/cas"

	"github.com/google/uuid"
)

// ErrNotDownloaded — у пересылающего нет локальной копии файла. Получатель
// тянет чанки только у отправителя манифеста, поэтому переслать можно лишь
// то, что лежит у нас на диске.
var ErrNotDownloaded = errors.New("not_downloaded")

// ErrLegacyFormat — у сообщения нет строк в message_files (старый формат).
var ErrLegacyFormat = errors.New("legacy_format")

// loadMessageFiles собирает манифесты и FileItem'ы сообщения из message_files.
// col — "message_id" (DM) или "group_msg_id" (группа).
func (s *FilesService) loadMessageFiles(col, msgID string) ([]FileItem, []wire.FileManifestEntry, error) {
	if col != "message_id" && col != "group_msg_id" {
		return nil, nil, fmt.Errorf("bad column %q", col)
	}
	rows, err := s.db.Query(
		`SELECT file_sha, name, kind FROM message_files WHERE `+col+` = ? ORDER BY ord ASC`, msgID)
	if err != nil {
		return nil, nil, fmt.Errorf("query message_files: %w", err)
	}
	type linkRow struct{ sha, name, kind string }
	var links []linkRow
	for rows.Next() {
		var lr linkRow
		if err := rows.Scan(&lr.sha, &lr.name, &lr.kind); err != nil {
			continue
		}
		links = append(links, lr)
	}
	rows.Close()
	if len(links) == 0 {
		return nil, nil, ErrLegacyFormat
	}
	items := make([]FileItem, 0, len(links))
	manifests := make([]wire.FileManifestEntry, 0, len(links))
	for _, lr := range links {
		m, err := cas.LoadManifest(s.db, lr.sha)
		if err != nil {
			return nil, nil, fmt.Errorf("load manifest %s: %w", safeShort8(lr.sha), err)
		}
		manifests = append(manifests, wire.FileManifestEntry{
			Sha: m.Sha, Size: m.Size, ChunkSize: m.ChunkSize,
			ChunkCount: m.ChunkCount, Chunks: m.Chunks, Mime: m.Mime,
			Width: m.Width, Height: m.Height,
			Name: lr.name, Kind: lr.kind,
		})
		items = append(items, FileItem{Name: lr.name, Kind: lr.kind})
	}
	return items, manifests, nil
}

// canSeed — отдаст ли seeder этот файл: local_path на месте и первый чанк
// читается с теми же проверками, что в onChunkRequest (streaming — size+mtime,
// cas — sha чанка).
func (s *FilesService) canSeed(sha string) bool {
	m, err := cas.LoadManifest(s.db, sha)
	if err != nil {
		return false
	}
	path := cas.GetFileLocalPath(s.db, sha)
	if path == "" || !FileExists(path) {
		return false
	}
	if m.ChunkCount == 0 || len(m.Chunks) == 0 {
		return true
	}
	if m.Source == "streaming" {
		_, err := cas.ReadChunkFromFile(path, 0, m.ChunkSize, m.OriginSize, m.OriginMtime)
		return err == nil
	}
	data, err := cas.ReadChunkFromFile(path, 0, m.ChunkSize, 0, 0)
	return err == nil && cas.HashOf(data) == m.Chunks[0]
}

// loadFilesBySha — как loadMessageFiles, но по списку файлов (выбор в
// «Медиа», «Переслать» у вложения): имя и тип — из самой ранней строки
// message_files файла. Повторы sha пропускаются, порядок сохраняется.
func (s *FilesService) loadFilesBySha(shas []string) ([]FileItem, []wire.FileManifestEntry, error) {
	if len(shas) == 0 {
		return nil, nil, errors.New("нет файлов")
	}
	items := make([]FileItem, 0, len(shas))
	manifests := make([]wire.FileManifestEntry, 0, len(shas))
	seen := make(map[string]bool, len(shas))
	for _, sha := range shas {
		if seen[sha] {
			continue
		}
		seen[sha] = true
		var name, kind string
		if err := s.db.QueryRow(
			`SELECT name, kind FROM message_files WHERE file_sha = ? ORDER BY id ASC LIMIT 1`, sha).Scan(&name, &kind); err != nil {
			return nil, nil, fmt.Errorf("file %s: %w", safeShort8(sha), err)
		}
		m, err := cas.LoadManifest(s.db, sha)
		if err != nil {
			return nil, nil, fmt.Errorf("load manifest %s: %w", safeShort8(sha), err)
		}
		manifests = append(manifests, wire.FileManifestEntry{
			Sha: m.Sha, Size: m.Size, ChunkSize: m.ChunkSize,
			ChunkCount: m.ChunkCount, Chunks: m.Chunks, Mime: m.Mime,
			Width: m.Width, Height: m.Height,
			Name: name, Kind: kind,
		})
		items = append(items, FileItem{Name: name, Kind: kind})
	}
	return items, manifests, nil
}

// ForwardFiles пересылает вложения сообщения srcMsgID (srcKind "dm" | "group")
// в DM dstPeerID или группу dstGroupID без повторной загрузки: новое сообщение
// ссылается на те же файлы (refcount++), получатели тянут чанки у нас.
func (s *FilesService) ForwardFiles(srcMsgID, srcKind, dstPeerID, dstGroupID, caption, fwdName string) (*SendResult, error) {
	const op = "FilesService.ForwardFiles"
	if (dstPeerID == "") == (dstGroupID == "") {
		return nil, fmt.Errorf("%s: нужен ровно один получатель", op)
	}
	col := "message_id"
	switch srcKind {
	case "dm":
	case "group":
		col = "group_msg_id"
	default:
		return nil, fmt.Errorf("%s: bad srcKind %q", op, srcKind)
	}
	items, manifests, err := s.loadMessageFiles(col, srcMsgID)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	fwd := ""
	if fwdName != "" {
		r := []rune(fwdName)
		if len(r) > 64 {
			r = r[:64]
		}
		if b, err := json.Marshal(wire.ForwardRef{Name: string(r)}); err == nil {
			fwd = string(b)
		}
	}
	res, err := s.forwardManifests(items, manifests, dstPeerID, dstGroupID, caption, fwd, srcMsgID)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return res, nil
}

// ForwardFileShas пересылает отдельные файлы нового формата (мультивыбор в
// «Медиа», «Переслать» у вложения) одним сообщением без повторной загрузки.
// Ровно один из dstPeerID / dstGroupID. Ошибка "not_downloaded" — у нас нет
// локальной копии какого-то файла.
func (s *FilesService) ForwardFileShas(shas []string, dstPeerID, dstGroupID, caption string) (*SendResult, error) {
	const op = "FilesService.ForwardFileShas"
	if (dstPeerID == "") == (dstGroupID == "") {
		return nil, fmt.Errorf("%s: нужен ровно один получатель", op)
	}
	items, manifests, err := s.loadFilesBySha(shas)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	res, err := s.forwardManifests(items, manifests, dstPeerID, dstGroupID, caption, "", "")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return res, nil
}

// forwardManifests — общая часть пересылки: все файлы должны лежать у нас
// (получатели тянут чанки у отправителя манифеста), новое сообщение
// ссылается на те же файлы, манифесты уходят получателям. src — для лога.
func (s *FilesService) forwardManifests(items []FileItem, manifests []wire.FileManifestEntry, dstPeerID, dstGroupID, caption, fwd, src string) (*SendResult, error) {
	myID, err := s.sess.PeerID()
	if err != nil {
		return nil, err
	}
	for _, m := range manifests {
		if !s.canSeed(m.Sha) {
			return nil, fmt.Errorf("%s: %w", safeShort8(m.Sha), ErrNotDownloaded)
		}
	}

	msgID := uuid.New().String()
	now := time.Now().Unix()
	totalBytes := int64(0)
	for _, m := range manifests {
		totalBytes += m.Size
	}

	var recipients []string
	if dstGroupID != "" {
		if s.groups == nil {
			return nil, errors.New("groups service not wired")
		}
		members := s.groups.GetGroupMemberIDs(dstGroupID)
		if err := s.persistGroup(msgID, dstGroupID, myID, caption, fwd, now, items, manifests, members); err != nil {
			return nil, err
		}
		for _, m := range members {
			if m != myID {
				recipients = append(recipients, m)
			}
		}
	} else {
		if err := s.persistDM(msgID, dstPeerID, myID, caption, fwd, now, items, manifests); err != nil {
			return nil, err
		}
		recipients = []string{dstPeerID}
	}

	s.log.Info("📎 FILES FORWARDED (CAS)",
		slog.String("src", src),
		slog.String("msg_id", msgID),
		slog.String("peer", safeShort(dstPeerID)),
		slog.String("group", safeShort8(dstGroupID)),
		slog.Int("count", len(manifests)),
		slog.Int64("bytes", totalBytes))

	s.registerUpload(msgID, dstGroupID, recipients, totalBytes)
	for _, peer := range recipients {
		go s.sendManifestsToPeer(msgID, dstGroupID, peer, caption, items, manifests)
	}
	return &SendResult{ID: msgID, Delivered: false}, nil
}

// RetryUploadGroup пере-отправляет FILE_MANIFEST группового вложения одному
// участнику, у которого group_message_delivery.delivered = 0. Вызывается из
// GroupsService.DeliverPendingGroupMessages при реконнекте участника.
func (s *FilesService) RetryUploadGroup(peerID, msgID string) {
	var caption, groupID string
	if err := s.db.QueryRow(
		`SELECT gm.content, gm.group_id FROM group_messages gm
		 JOIN group_message_delivery d ON d.msg_id = gm.id
		 WHERE gm.id = ? AND gm.type = 'attachment' AND gm.deleted = 0
		   AND d.peer_id = ? AND d.delivered = 0`,
		msgID, peerID).Scan(&caption, &groupID); err != nil {
		s.log.Debug("retry upload group: not pending",
			slog.String("msg_id", msgID),
			slog.String("err", err.Error()))
		return
	}
	items, manifests, err := s.loadMessageFiles("group_msg_id", msgID)
	if err != nil {
		s.log.Warn("retry upload group: load files",
			slog.String("msg_id", msgID),
			slog.String("err", err.Error()))
		return
	}
	totalBytes := int64(0)
	for _, m := range manifests {
		totalBytes += m.Size
	}
	s.log.Info("📎 GROUP FILES RETRY (CAS)",
		slog.String("msg_id", msgID),
		slog.String("group", safeShort8(groupID)),
		slog.String("peer", safeShort(peerID)),
		slog.Int("count", len(manifests)))

	s.ensureUploadPeer(msgID, groupID, peerID, totalBytes)
	go s.sendManifestsToPeer(msgID, groupID, peerID, caption, items, manifests)
}

// ensureUploadPeer добавляет peer к уже зарегистрированной передаче (групповой
// повтор для нескольких участников не должен затирать друг друга) или
// регистрирует новую.
func (s *FilesService) ensureUploadPeer(msgID, groupID, peerID string, totalBytes int64) {
	s.mu.Lock()
	st, ok := s.uploads[msgID]
	s.mu.Unlock()
	if !ok {
		s.registerUpload(msgID, groupID, []string{peerID}, totalBytes)
		return
	}
	st.mu.Lock()
	if st.peers[peerID] == nil {
		st.peers[peerID] = &peerUpload{}
	}
	st.mu.Unlock()
}

// markDeliveredGroup — delivered=1 для участника после успешной отправки
// манифеста. Без этого групповые вложения вечно висели pending, а
// CleanupStaleAttachments удалял их через 7 дней.
func (s *FilesService) markDeliveredGroup(msgID, peerID string) {
	if _, err := s.db.Exec(
		`UPDATE group_message_delivery SET delivered = 1 WHERE msg_id = ? AND peer_id = ? AND delivered = 0`,
		msgID, peerID); err != nil {
		s.log.Debug("mark delivered group: skip", slog.String("err", err.Error()))
	}
}

// lookupForwardedFrom — «переслано от» сообщения для манифеста (в т.ч. при
// повторной отправке). nil — не пересланное.
func (s *FilesService) lookupForwardedFrom(msgID, groupID string) *wire.ForwardRef {
	table := "messages"
	if groupID != "" {
		table = "group_messages"
	}
	var raw string
	if err := s.db.QueryRow(`SELECT forwarded_from FROM `+table+` WHERE id = ?`, msgID).Scan(&raw); err != nil || raw == "" {
		return nil
	}
	var f wire.ForwardRef
	if json.Unmarshal([]byte(raw), &f) != nil || f.Name == "" {
		return nil
	}
	return &f
}
