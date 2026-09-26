package files

import (
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"log/slog"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"vox_app/internal/logsafe"
	"vox_app/internal/services/files/wire"

	"vox_app/internal/storage/cas"

	"github.com/google/uuid"
	xdraw "golang.org/x/image/draw"
)

const (
	// chunkSizeForUpload — plaintext-байт на chunk при подготовке файла.
	chunkSizeForUpload = cas.DefaultChunkSize

	// compressMaxEdge — при compress=true для image сжимаем до этой длинной
	// стороны. Совпадает со старым attachments_picker'ом.
	compressMaxEdge = 1920
	compressQuality = 85

	// maxFileSizeBytes — hard-cap для одного файла. 100 GB — теоретический
	// потолок для CAS (chunks_received bitmap не превысит ~4 MB).
	maxFileSizeBytes = 100 * 1024 * 1024 * 1024

	// maxFilesPerMessage — сколько файлов можно за один SendFilesByPath.
	maxFilesPerMessage = 30

	// streamingThreshold — файлы >= этого размера НЕ копируются в CAS.
	// Считается только sha (один read pass), чанки отдаются по запросу
	// прямо из оригинала. Экономит диск (нет 2× копии для 100 GB) ценой
	// зависимости от того, что юзер не переместит/удалит оригинал во время
	// передачи. См. cas.HashFileForManifest и seeder.streamChunkFromOriginal.
	streamingThreshold = 100 * 1024 * 1024 // 100 MiB
)

// SendFilesByPath — главный publich Wails метод. Готовит файлы (chunk → CAS),
// создаёт message, рассылает FILE_MANIFEST всем активным peer'ам.
//
// Возвращает SendResult{ID} сразу после INSERT в БД. Передача chunks происходит
// pull-моделью (по запросу receiver'а), seeder работает на фоне до завершения.
func (s *FilesService) SendFilesByPath(peerID string, items []FileItem, caption string) (*SendResult, error) {
	const op = "FilesService.SendFilesByPath"
	if peerID == "" {
		return nil, fmt.Errorf("%s: peerID required", op)
	}
	if err := validateItems(items); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	myID, err := s.sess.PeerID()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	manifests, err := s.prepareFilesToCAS(items)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	msgID := uuid.New().String()
	now := time.Now().Unix()

	if err := s.persistDM(msgID, peerID, myID, caption, "", now, items, manifests); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	totalBytes := int64(0)
	for _, m := range manifests {
		totalBytes += m.Size
	}
	s.log.Info("📎 FILES QUEUED (CAS)",
		slog.String("msg_id", msgID),
		slog.String("peer", safeShort(peerID)),
		slog.Int("count", len(items)),
		slog.Int64("bytes", totalBytes))

	s.registerUpload(msgID, "", []string{peerID}, totalBytes)
	go s.sendManifestsToPeer(msgID, "", peerID, caption, items, manifests)

	return &SendResult{ID: msgID, Delivered: false}, nil
}

// RetryUploadDM пере-отправляет FILE_MANIFEST для уже зарегистрированного DM
// сообщения. Используется из MessagingService.DeliverPendingMessages когда
// сообщение типа 'attachment' с delivered=0 нужно повторно доставить (предыдущая
// попытка не прошла — например Files PC упал до markDeliveredDM).
//
// Идемпотентно: если manifest пройдёт, markDeliveredDM проставит delivered=1.
// Если peer оффлайн или PC не поднимается — sendManifestsToPeer тихо выйдет.
func (s *FilesService) RetryUploadDM(peerID, msgID string) {
	var caption string
	if err := s.db.QueryRow(
		`SELECT content FROM messages WHERE id = ? AND type = 'attachment' AND delivered = 0`,
		msgID).Scan(&caption); err != nil {
		s.log.Debug("retry upload DM: message not pending",
			slog.String("msg_id", msgID),
			logsafe.Err("err", err))
		return
	}

	rows, err := s.db.Query(
		`SELECT file_sha, name, kind FROM message_files
		 WHERE message_id = ? ORDER BY ord ASC`,
		msgID)
	if err != nil {
		s.log.Warn("retry upload DM: query message_files",
			slog.String("msg_id", msgID),
			logsafe.Err("err", err))
		return
	}
	defer rows.Close()

	type linkRow struct {
		sha, name, kind string
	}
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
		// Нет message_files — это либо не наша запись (legacy), либо данные
		// потерялись. В любом случае retry'ить через FilesService нечего.
		return
	}

	manifests := make([]wire.FileManifestEntry, 0, len(links))
	items := make([]FileItem, 0, len(links))
	for _, lr := range links {
		m, err := cas.LoadManifest(s.db, lr.sha)
		if err != nil {
			s.log.Warn("retry upload DM: load manifest",
				slog.String("msg_id", msgID),
				slog.String("file_sha", safeShort8(lr.sha)),
				logsafe.Err("err", err))
			return
		}
		manifests = append(manifests, wire.FileManifestEntry{
			Sha: m.Sha, Size: m.Size, ChunkSize: m.ChunkSize,
			ChunkCount: m.ChunkCount, Chunks: m.Chunks, Mime: m.Mime,
			Width: m.Width, Height: m.Height,
			Name: lr.name, Kind: lr.kind,
		})
		items = append(items, FileItem{Name: lr.name, Kind: lr.kind})
	}

	totalBytes := int64(0)
	for _, m := range manifests {
		totalBytes += m.Size
	}
	s.log.Info("📎 FILES RETRY (CAS)",
		slog.String("msg_id", msgID),
		slog.String("peer", safeShort(peerID)),
		slog.Int("count", len(manifests)),
		slog.Int64("bytes", totalBytes))

	s.registerUpload(msgID, "", []string{peerID}, totalBytes)
	go s.sendManifestsToPeer(msgID, "", peerID, caption, items, manifests)
}

// SendGroupFilesByPath — групповая версия. Persist'им один group_message + N
// per-member rows в group_message_delivery. Manifest рассылаем активным членам;
// неактивные дополучат при reconnect через DeliverPendingGroupMessages.
func (s *FilesService) SendGroupFilesByPath(groupID string, items []FileItem, caption string) (*SendResult, error) {
	const op = "FilesService.SendGroupFilesByPath"
	if groupID == "" {
		return nil, fmt.Errorf("%s: groupID required", op)
	}
	if err := validateItems(items); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if s.groups == nil {
		return nil, fmt.Errorf("%s: groups service not wired", op)
	}
	myID, err := s.sess.PeerID()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	manifests, err := s.prepareFilesToCAS(items)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	msgID := uuid.New().String()
	now := time.Now().Unix()
	members := s.groups.GetGroupMemberIDs(groupID)

	if err := s.persistGroup(msgID, groupID, myID, caption, "", now, items, manifests, members); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	totalBytes := int64(0)
	for _, m := range manifests {
		totalBytes += m.Size
	}

	recipients := make([]string, 0, len(members))
	for _, m := range members {
		if m != myID {
			recipients = append(recipients, m)
		}
	}
	s.log.Info("📎 GROUP FILES QUEUED (CAS)",
		slog.String("msg_id", msgID),
		slog.String("group", safeShort8(groupID)),
		slog.Int("count", len(items)),
		slog.Int64("bytes", totalBytes),
		slog.Int("recipients", len(recipients)))

	s.registerUpload(msgID, groupID, recipients, totalBytes)
	for _, peer := range recipients {
		go s.sendManifestsToPeer(msgID, groupID, peer, caption, items, manifests)
	}

	return &SendResult{ID: msgID, Delivered: false}, nil
}

// ──── Подготовка файлов в CAS ────────────────────────────────────────────

// prepareFilesToCAS обрабатывает каждый item:
//   - kind="photo"+Compress=true: image.Decode → resize → jpeg.Encode → CAS
//   - иначе: open → stream chunk-by-chunk в CAS
//
// Каждый файл получает FileManifestEntry с chunk_shas и file_sha. Также
// прописывается files.local_path = оригинальный путь (для compress=true —
// путь к compress-tmp файлу временно; для file — оригинал на диске юзера).
// Sender открывает файл из своего bubble через этот path напрямую.
func (s *FilesService) prepareFilesToCAS(items []FileItem) ([]wire.FileManifestEntry, error) {
	out := make([]wire.FileManifestEntry, len(items))
	for i, it := range items {
		name := it.Name
		if name == "" {
			name = filepath.Base(it.Path)
		}

		var entry wire.FileManifestEntry
		var localPath string
		if it.Kind == "photo" && it.Compress {
			// Compress pipeline: resize+JPEG в файл в Vox Downloads, манифест
			// через HashFileForManifest. local_path = compressed-JPEG.
			e, dst, err := s.compressImage(it.Path, name)
			if err == nil {
				e.Kind = "photo"
				entry = e
				localPath = dst
			} else {
				s.log.Warn("compress failed, fallback to raw",
					logsafe.Err("err", err))
				e2, err2 := s.streamFileToCAS(it.Path, name, it.Kind)
				if err2 != nil {
					return nil, fmt.Errorf("file %d (%s): %w", i, filepath.Base(it.Path), err2)
				}
				entry = e2
				localPath = it.Path
			}
		} else {
			e, err := s.streamFileToCAS(it.Path, name, it.Kind)
			if err != nil {
				return nil, fmt.Errorf("file %d (%s): %w", i, filepath.Base(it.Path), err)
			}
			entry = e
			localPath = it.Path
		}
		_ = cas.SetFileLocalPath(s.db, entry.Sha, localPath)
		out[i] = entry
	}
	return out, nil
}

// streamFileToCAS готовит файл к отправке: hash-only pass, source='streaming'.
// Никаких CAS-чанков на диске не создаётся — seeder читает из оригинала по
// offset. local_path указывает на оригинальный файл юзера (см. prepareFilesToCAS).
func (s *FilesService) streamFileToCAS(path, name, kind string) (wire.FileManifestEntry, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return wire.FileManifestEntry{}, fmt.Errorf("stat: %w", err)
	}
	chunkShas, total, err := cas.HashFileForManifest(path, chunkSizeForUpload, maxFileSizeBytes)
	if err != nil {
		return wire.FileManifestEntry{}, fmt.Errorf("hash: %w", err)
	}

	mt := mimeOf(path)
	width, height := 0, 0
	if kind == "photo" {
		if w, h, ok := decodeImageDims(path); ok {
			width, height = w, h
		}
	}
	entry := wire.FileManifestEntry{
		Sha:        cas.ManifestHash(chunkShas, total, chunkSizeForUpload, mt),
		Size:       fi.Size(),
		ChunkSize:  chunkSizeForUpload,
		ChunkCount: len(chunkShas),
		Chunks:     chunkShas,
		Mime:       mt,
		Width:      width,
		Height:     height,
		Name:       name,
		Kind:       kind,
	}
	manifest := &cas.FileManifest{
		Sha: entry.Sha, Size: entry.Size, ChunkSize: entry.ChunkSize,
		ChunkCount: entry.ChunkCount, Chunks: entry.Chunks, Mime: entry.Mime,
		Width: entry.Width, Height: entry.Height,
		Source:      "streaming",
		OriginSize:  fi.Size(),
		OriginMtime: fi.ModTime().Unix(),
	}
	if err := cas.SaveManifest(s.db, manifest); err != nil {
		return wire.FileManifestEntry{}, fmt.Errorf("manifest: %w", err)
	}
	s.log.Info("📦 streaming manifest prepared",
		slog.String("file", safeShort8(entry.Sha)),
		slog.Int64("bytes", entry.Size),
		slog.Int("chunks", entry.ChunkCount))
	return entry, nil
}

// compressImage — image.Decode → CatmullRom resize → JPEG в файл в Vox
// Downloads, потом HashFileForManifest для streaming-манифеста. Возвращает
// (entry, dstPath). Caller прописывает local_path = dstPath, чтобы seeder
// читал тот же compressed-JPEG по offset.
func (s *FilesService) compressImage(srcPath, name string) (wire.FileManifestEntry, string, error) {
	f, err := os.Open(srcPath)
	if err != nil {
		return wire.FileManifestEntry{}, "", fmt.Errorf("open: %w", err)
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return wire.FileManifestEntry{}, "", fmt.Errorf("decode: %w", err)
	}
	bounds := img.Bounds()
	srcW, srcH := bounds.Dx(), bounds.Dy()
	dstW, dstH := scaleToBound(srcW, srcH, compressMaxEdge)
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, bounds, xdraw.Over, nil)

	dstPath, err := uniqueDownloadPath(renameToJPG(name))
	if err != nil {
		return wire.FileManifestEntry{}, "", fmt.Errorf("uniqueDownloadPath: %w", err)
	}
	out, err := os.Create(dstPath)
	if err != nil {
		return wire.FileManifestEntry{}, "", fmt.Errorf("create: %w", err)
	}
	if err := jpeg.Encode(out, dst, &jpeg.Options{Quality: compressQuality}); err != nil {
		_ = out.Close()
		_ = os.Remove(dstPath)
		return wire.FileManifestEntry{}, "", fmt.Errorf("encode: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dstPath)
		return wire.FileManifestEntry{}, "", fmt.Errorf("close: %w", err)
	}

	chunkShas, total, err := cas.HashFileForManifest(dstPath, chunkSizeForUpload, maxFileSizeBytes)
	if err != nil {
		_ = os.Remove(dstPath)
		return wire.FileManifestEntry{}, "", fmt.Errorf("hash: %w", err)
	}
	fi, err := os.Stat(dstPath)
	if err != nil {
		return wire.FileManifestEntry{}, "", fmt.Errorf("stat: %w", err)
	}
	mt := "image/jpeg"
	entry := wire.FileManifestEntry{
		Sha:        cas.ManifestHash(chunkShas, total, chunkSizeForUpload, mt),
		Size:       total,
		ChunkSize:  chunkSizeForUpload,
		ChunkCount: len(chunkShas),
		Chunks:     chunkShas,
		Mime:       mt,
		Width:      dstW,
		Height:     dstH,
		Name:       renameToJPG(name),
		Kind:       "photo",
	}
	manifest := &cas.FileManifest{
		Sha: entry.Sha, Size: entry.Size, ChunkSize: entry.ChunkSize,
		ChunkCount: entry.ChunkCount, Chunks: entry.Chunks, Mime: entry.Mime,
		Width: entry.Width, Height: entry.Height,
		Source:      "streaming",
		OriginSize:  fi.Size(),
		OriginMtime: fi.ModTime().Unix(),
	}
	if err := cas.SaveManifest(s.db, manifest); err != nil {
		return wire.FileManifestEntry{}, "", fmt.Errorf("manifest: %w", err)
	}
	return entry, dstPath, nil
}

// ──── Helpers ────────────────────────────────────────────────────────────

func validateItems(items []FileItem) error {
	if len(items) == 0 {
		return fmt.Errorf("items empty")
	}
	if len(items) > maxFilesPerMessage {
		return fmt.Errorf("too many files (%d > %d)", len(items), maxFilesPerMessage)
	}
	for i, it := range items {
		if it.Path == "" {
			return fmt.Errorf("item %d: path empty", i)
		}
		if it.Kind != "photo" && it.Kind != "file" {
			return fmt.Errorf("item %d: bad kind %q", i, it.Kind)
		}
		fi, err := os.Stat(it.Path)
		if err != nil {
			return fmt.Errorf("item %d (%s): stat: %w", i, filepath.Base(it.Path), err)
		}
		if fi.IsDir() {
			return fmt.Errorf("item %d: %s is a directory", i, it.Path)
		}
		if fi.Size() <= 0 {
			return fmt.Errorf("item %d: empty file", i)
		}
		if fi.Size() > maxFileSizeBytes {
			return fmt.Errorf("item %d: size %d > limit", i, fi.Size())
		}
	}
	return nil
}

func mimeOf(path string) string {
	mt := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if mt == "" {
		return "application/octet-stream"
	}
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	return mt
}

func decodeImageDims(path string) (int, int, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0, false
	}
	return cfg.Width, cfg.Height, true
}

func scaleToBound(w, h, maxEdge int) (int, int) {
	if w <= maxEdge && h <= maxEdge {
		return w, h
	}
	if w >= h {
		return maxEdge, h * maxEdge / w
	}
	return w * maxEdge / h, maxEdge
}

func renameToJPG(name string) string {
	ext := filepath.Ext(name)
	if ext == "" {
		return name + ".jpg"
	}
	return strings.TrimSuffix(name, ext) + ".jpg"
}

func safeShort(s string) string {
	if len(s) < 16 {
		return s
	}
	return s[:16]
}
func safeShort8(s string) string {
	if len(s) < 8 {
		return s
	}
	return s[:8]
}
