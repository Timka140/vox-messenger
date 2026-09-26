package files

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"vox_app/internal/logsafe"
	"vox_app/internal/storage/cas"
)

// UnlinkFilesForMessage — обратная сторона persistDM/persistGroup. Вызывается
// при удалении сообщения через UnlinkAttachmentsForMessage hook.
//
// Шаги:
//  1. SELECT file_sha из message_files WHERE message_id|group_msg_id = ?
//  2. DELETE message_files row
//  3. cas.DecFileRefcount для каждого file_sha (если refcount=0 → файл row
//     удаляется, и мы удаляем local_path-файл если он внутри Vox Downloads)
//  4. DELETE FROM file_downloads — bitmap состояние больше не нужно
//
// Идемпотентно: если message_files row нет — просто no-op.
func (s *FilesService) UnlinkFilesForMessage(msgID, msgKind string) {
	col := "message_id"
	if msgKind == "group" {
		col = "group_msg_id"
	}

	rows, err := s.db.Query(
		`SELECT file_sha FROM message_files WHERE `+col+` = ?`, msgID)
	if err != nil {
		s.log.Warn("unlink files: select",
			slog.String("msg_id", msgID),
			logsafe.Err("err", err))
		return
	}
	var shas []string
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err == nil {
			shas = append(shas, sha)
		}
	}
	rows.Close()

	if len(shas) == 0 {
		return
	}

	if _, err := s.db.Exec(
		`DELETE FROM message_files WHERE `+col+` = ?`, msgID); err != nil {
		s.log.Warn("unlink files: delete links",
			slog.String("msg_id", msgID),
			logsafe.Err("err", err))
	}

	totalFilesDeleted := 0
	for _, sha := range shas {
		// Перед DecFileRefcount запоминаем local_path — после Dec до 0 row исчезнет.
		localPath := cas.GetFileLocalPath(s.db, sha)

		dropped, err := cas.DecFileRefcount(s.db, sha)
		if err != nil {
			s.log.Warn("unlink files: dec refcount",
				slog.String("file_sha", safeShort8(sha)),
				logsafe.Err("err", err))
			continue
		}
		// Если refcount упал до 0 — удаляем local-файл если он внутри Vox
		// Downloads (т.е. это assembled receiver-копия). Sender-side, где
		// local_path указывает на оригинал юзера — НЕ удаляем, это его файл.
		if dropped && localPath != "" && isInsideVoxDownloads(localPath) {
			if err := os.Remove(localPath); err != nil && !os.IsNotExist(err) {
				s.log.Warn("unlink files: remove local file",
					logsafe.Err("err", err))
			} else {
				totalFilesDeleted++
			}
		}
		// Также чистим file_downloads-state — этот файл больше не нужен.
		if _, err := s.db.Exec(
			`DELETE FROM file_downloads WHERE file_sha = ?`, sha); err != nil {
			s.log.Debug("unlink files: clean downloads",
				slog.String("file_sha", safeShort8(sha)),
				logsafe.Err("err", err))
		}
	}

	if totalFilesDeleted > 0 {
		s.log.Info("🧹 file cleanup",
			slog.String("msg_id", msgID),
			slog.Int("files", len(shas)),
			slog.Int("local_files_deleted", totalFilesDeleted))
	}
}

// isInsideVoxDownloads — true если path лежит в нашей системной папке
// (~/Downloads/Vox Downloads). Защита от случайного удаления юзерских файлов
// которые sender ссылается напрямую (не копировались в Vox Downloads).
func isInsideVoxDownloads(p string) bool {
	root := VoxDownloadsDir()
	abs, err := filepath.Abs(p)
	if err != nil {
		return false
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(rootAbs, abs)
	if err != nil {
		return false
	}
	return rel != "" && !strings.HasPrefix(rel, "..")
}
