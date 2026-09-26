package services

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/wailsapp/wails/v3/pkg/application"

	"vox_app/internal/logsafe"
)

// attachments_native.go — Wails-методы для нативных Save As / Open.
// Альтернатива browser-стороне (blob URL + <a download>) — даёт юзеру
// настоящий picker и интеграцию с системным Open With.

// SaveAttachmentAs показывает native SaveFileDialog с suggestedName.
//
// Fast-path: если есть local_path — io.Copy(localPath → выбранный путь), без
// расшифровки/CAS. Slow-path: legacy через blobs.OpenReader / cas reader.
func (s *MessagingService) SaveAttachmentAs(attachmentID, suggestedName string) (string, error) {
	const op = "MessagingService.SaveAttachmentAs"

	app := application.Get()
	if app == nil {
		return "", fmt.Errorf("%s: no app instance", op)
	}

	dlg := app.Dialog.SaveFile().SetFilename(suggestedName)
	path, err := dlg.PromptForSingleSelection()
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "cancel") {
			return "", nil
		}
		return "", fmt.Errorf("%s: dialog: %w", op, err)
	}
	if path == "" {
		return "", nil
	}

	// Fast-path: для CAS sha — копируем local_path → выбор юзера.
	if len(attachmentID) == 64 {
		var localPath string
		_ = s.store.DB().QueryRow(
			`SELECT local_path FROM files WHERE sha = ?`, attachmentID,
		).Scan(&localPath)
		if localPath != "" {
			if _, err := os.Stat(localPath); err == nil {
				if n, err := copyFileFast(localPath, path); err == nil {
					s.log.Info("attachment saved (fast-path)",
						slog.String("sha", safeShort8(attachmentID)),
						slog.String("dst", path),
						slog.Int64("bytes", n))
					return path, nil
				}
			}
		}
	}

	if s.blobs == nil {
		return "", fmt.Errorf("%s: blob store not configured", op)
	}

	n, err := s.copyAttachmentTo(attachmentID, path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}

	s.log.Info("attachment saved",
		slog.String("att_id", attachmentID),
		slog.Int64("bytes", n))
	return path, nil
}

// SaveAllResult — результат SaveAllAttachmentsForMessage. Wails-binding
// возвращает как один объект (tuple multiple-returns в alpha.74 не унифицирован).
type SaveAllResult struct {
	Saved     int    `json:"saved"`
	Total     int    `json:"total"`
	Dir       string `json:"dir"`
	Cancelled bool   `json:"cancelled"`
}

// SaveAllAttachmentsForMessage сохраняет ВСЕ вложения одного сообщения в
// выбранную папку. Открывает native folder picker, потом потоково копирует
// каждый blob под оригинальным именем. Конфликты имён разруливаются суффиксом
// " (N)" перед расширением (как в Windows-копировании).
//
// msgKind: "dm" → ищем по messages.id, "group" → по group_messages.id.
func (s *MessagingService) SaveAllAttachmentsForMessage(messageID, msgKind string) (*SaveAllResult, error) {
	const op = "MessagingService.SaveAllAttachmentsForMessage"
	if s.blobs == nil {
		return nil, fmt.Errorf("%s: blob store not configured", op)
	}
	if messageID == "" {
		return nil, fmt.Errorf("%s: empty messageID", op)
	}

	app := application.Get()
	if app == nil {
		return nil, fmt.Errorf("%s: no app instance", op)
	}

	atts := s.LoadAttachmentsForMessage(messageID, msgKind)
	if len(atts) == 0 {
		return nil, fmt.Errorf("%s: no attachments", op)
	}

	dlg := app.Dialog.OpenFile().
		CanChooseFiles(false).
		CanChooseDirectories(true).
		CanCreateDirectories(true).
		SetTitle("Куда сохранить файлы")
	dir, err := dlg.PromptForSingleSelection()
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "cancel") {
			return &SaveAllResult{Cancelled: true, Total: len(atts)}, nil
		}
		return nil, fmt.Errorf("%s: dialog: %w", op, err)
	}
	if dir == "" {
		return &SaveAllResult{Cancelled: true, Total: len(atts)}, nil
	}

	saved := 0
	for _, m := range atts {
		name := sanitizeFilename(m.Name)
		if name == "" {
			name = m.ID
		}
		dest := uniquePath(filepath.Join(dir, name))
		if _, cerr := s.copyAttachmentTo(m.ID, dest); cerr != nil {
			s.log.Warn("save all: copy attachment failed",
				slog.String("att_id", m.ID),
				logsafe.Err("err", cerr))
			continue
		}
		saved++
	}
	s.log.Info("attachments batch saved",
		slog.String("msg_id", messageID),
		slog.String("dir", dir),
		slog.Int("saved", saved),
		slog.Int("of", len(atts)))
	return &SaveAllResult{Saved: saved, Total: len(atts), Dir: dir}, nil
}

// SaveAttachmentsTo — «Сохранить» выбранных во вкладке «Медиа» (фаза 7e):
// одна папка на все. ids — attachment id (старый формат) или sha (новый),
// names — имена в том же порядке; конфликты имён — суффикс " (N)".
func (s *MessagingService) SaveAttachmentsTo(ids, names []string) (*SaveAllResult, error) {
	const op = "MessagingService.SaveAttachmentsTo"
	if len(ids) == 0 {
		return nil, fmt.Errorf("%s: no attachments", op)
	}
	if s.blobs == nil {
		return nil, fmt.Errorf("%s: blob store not configured", op)
	}
	app := application.Get()
	if app == nil {
		return nil, fmt.Errorf("%s: no app instance", op)
	}
	dlg := app.Dialog.OpenFile().
		CanChooseFiles(false).
		CanChooseDirectories(true).
		CanCreateDirectories(true).
		SetTitle("Куда сохранить файлы")
	dir, err := dlg.PromptForSingleSelection()
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "cancel") {
			return &SaveAllResult{Cancelled: true, Total: len(ids)}, nil
		}
		return nil, fmt.Errorf("%s: dialog: %w", op, err)
	}
	if dir == "" {
		return &SaveAllResult{Cancelled: true, Total: len(ids)}, nil
	}

	saved := 0
	for i, id := range ids {
		name := ""
		if i < len(names) {
			name = sanitizeFilename(names[i])
		}
		if name == "" {
			name = safeShort8(id)
		}
		dest := uniquePath(filepath.Join(dir, name))
		if _, cerr := s.copyAttachmentTo(id, dest); cerr != nil {
			s.log.Warn("save selected: copy attachment failed",
				slog.String("att_id", safeShort8(id)),
				logsafe.Err("err", cerr))
			continue
		}
		saved++
	}
	s.log.Info("selected attachments saved",
		slog.String("dir", dir),
		slog.Int("saved", saved),
		slog.Int("of", len(ids)))
	return &SaveAllResult{Saved: saved, Total: len(ids), Dir: dir}, nil
}

// uniquePath возвращает путь, которого ещё нет на диске. Если path занят —
// добавляет " (1)", " (2)", ... перед расширением. Идиома Windows-копирования.
func uniquePath(path string) string {
	if _, err := os.Stat(path); err != nil {
		return path
	}
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 1; i < 10000; i++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
		if _, err := os.Stat(candidate); err != nil {
			return candidate
		}
	}
	return path // практически недостижимо
}

// ErrAttachmentLost — файл недоступен ни локально, ни в CAS. Frontend
// показывает toast «Файл утерян» вместо попытки открытия.
var ErrAttachmentLost = fmt.Errorf("файл утерян")

// ErrAttachmentNotOnDisk — у файла нет пути на диске (legacy-blob или только
// чанки CAS): показать в папке нечего, фронт предлагает «Сохранить как…».
var ErrAttachmentNotOnDisk = fmt.Errorf("not_on_disk")

// RevealAttachment показывает файл вложения в Проводнике (меню «Показать в
// папке»). Работает только для CAS-файлов с живым files.local_path.
func (s *MessagingService) RevealAttachment(attachmentID string) error {
	const op = "MessagingService.RevealAttachment"
	if len(attachmentID) != 64 {
		return ErrAttachmentNotOnDisk
	}
	var localPath string
	_ = s.store.DB().QueryRow(`SELECT local_path FROM files WHERE sha = ?`, attachmentID).Scan(&localPath)
	if localPath == "" {
		return ErrAttachmentNotOnDisk
	}
	if _, err := os.Stat(localPath); err != nil {
		return ErrAttachmentNotOnDisk
	}
	if err := revealInFolder(localPath); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	s.log.Info("attachment revealed", slog.String("sha", safeShort8(attachmentID)))
	return nil
}

// OpenAttachment открывает файл в default OS-приложении.
//
// Fast-path: если у нас в files.local_path записан реальный путь и файл там
// существует — открываем его напрямую (никаких tmp-копий, мгновенно).
// Slow-path (legacy / удалённый local_path): копируем blob во временный файл
// и открываем оттуда.
//
// Если файл недоступен (local_path удалён + chunks в CAS отсутствуют либо
// stream-original не работает) — возвращает ErrAttachmentLost.
func (s *MessagingService) OpenAttachment(attachmentID, suggestedName string) error {
	const op = "MessagingService.OpenAttachment"

	// Fast-path: для CAS attachmentID = sha. Если есть local_path — открываем сразу.
	if len(attachmentID) == 64 {
		var localPath string
		_ = s.store.DB().QueryRow(
			`SELECT local_path FROM files WHERE sha = ?`, attachmentID,
		).Scan(&localPath)
		if localPath != "" {
			if _, err := os.Stat(localPath); err == nil {
				if err := openInDefaultApp(localPath); err != nil {
					return fmt.Errorf("%s: open: %w", op, err)
				}
				s.log.Info("attachment opened (fast-path)",
					slog.String("sha", safeShort8(attachmentID)))
				return nil
			}
			// Файл удалён извне — сбрасываем флаг, продолжаем со slow-path.
			_, _ = s.store.DB().Exec(`UPDATE files SET local_path = '' WHERE sha = ?`, attachmentID)
		}
		// Local-path нет/невалиден. Если и CAS-чанков тоже нет — файл утерян.
		// Streaming-source файлы тут тоже отсеиваются (чанков по определению
		// нет, а оригинал недоступен).
		if !s.casFileAvailable(attachmentID) {
			s.log.Info("attachment lost",
				slog.String("sha", safeShort8(attachmentID)))
			return ErrAttachmentLost
		}
	}

	if s.blobs == nil {
		return fmt.Errorf("%s: blob store not configured", op)
	}

	// Кладём в os.TempDir с stable именем по attID — повторный Open того же файла
	// не плодит копии и переиспользует уже-открытый Handle (Word/PDF reader).
	safeName := sanitizeFilename(suggestedName)
	if safeName == "" {
		safeName = attachmentID
	}
	tmpDir := filepath.Join(os.TempDir(), "vox-attach", attachmentID)
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return fmt.Errorf("%s: mkdir: %w", op, err)
	}
	tmpPath := filepath.Join(tmpDir, safeName)

	// Переиспользуем уже распакованный файл, если его размер совпадает с
	// размером plaintext'а в БД — сравнение по size, без чтения содержимого.
	// Содержимое не меняется под неизменным attID (sha256 → unique blob),
	// поэтому size — достаточный инвариант.
	if fresh, err := tmpFileMatchesAttachment(s.store.DB(), attachmentID, tmpPath); err == nil && fresh {
		s.log.Debug("attachment reusing cached tmp")
	} else {
		if _, err := s.copyAttachmentTo(attachmentID, tmpPath); err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
	}

	if err := openInDefaultApp(tmpPath); err != nil {
		return fmt.Errorf("%s: open: %w", op, err)
	}
	s.log.Info("attachment opened native", slog.String("att_id", attachmentID))
	return nil
}

// copyAttachmentTo — streaming decrypt + write. Сначала ищет в legacy
// attachments table (UUID-based), fallback на CAS files (sha-based).
// Для CAS attachmentID = sha (фронт ставит ID = Sha256 в LoadAttachmentsForMessage).
func (s *MessagingService) copyAttachmentTo(attachmentID, destPath string) (int64, error) {
	var rdr io.ReadCloser
	var err error

	// 1. Legacy: attachmentID — это UUID, ищем в attachments.
	var sha string
	err = s.store.DB().QueryRow(
		"SELECT sha256 FROM attachments WHERE id = ?", attachmentID).Scan(&sha)
	if err == nil {
		rdr, err = s.blobs.OpenReader(sha)
		if err != nil {
			return 0, err
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		// 2. Single-file: attachmentID = sha (64 hex). Открываем local_path
		// напрямую — chunked CAS-reader больше не используется.
		var localPath string
		_ = s.store.DB().QueryRow(
			`SELECT local_path FROM files WHERE sha = ?`, attachmentID).Scan(&localPath)
		if localPath == "" {
			return 0, fmt.Errorf("файл утерян")
		}
		f, oerr := os.Open(localPath)
		if oerr != nil {
			// Файл был, но удалён извне — сбрасываем local_path в БД, фронт
			// перерисует bubble в "Скачать".
			_, _ = s.store.DB().Exec(`UPDATE files SET local_path = '' WHERE sha = ?`, attachmentID)
			return 0, fmt.Errorf("файл утерян")
		}
		rdr = f
	} else {
		return 0, err
	}
	defer rdr.Close()

	dir := filepath.Dir(destPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, fmt.Errorf("mkdir target: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(destPath)+".*.tmp")
	if err != nil {
		return 0, fmt.Errorf("create tmp: %w", err)
	}
	tmpPath := tmp.Name()

	n, cerr := io.Copy(tmp, rdr)
	if cerr != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return 0, fmt.Errorf("copy: %w", cerr)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return 0, fmt.Errorf("fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return 0, fmt.Errorf("close: %w", err)
	}
	// На Windows Rename не перезапишет существующий файл — сначала удаляем.
	_ = os.Remove(destPath)
	if err := os.Rename(tmpPath, destPath); err != nil {
		_ = os.Remove(tmpPath)
		return 0, fmt.Errorf("rename: %w", err)
	}
	return n, nil
}

// tmpFileMatchesAttachment возвращает true если tmpPath существует и его
// размер равен plaintext size attachment'а (сравнение через attachments.size).
func tmpFileMatchesAttachment(db *sql.DB, attachmentID, tmpPath string) (bool, error) {
	fi, err := os.Stat(tmpPath)
	if err != nil {
		return false, err
	}
	var want int64
	if err := db.QueryRow(
		"SELECT size FROM attachments WHERE id = ?", attachmentID).Scan(&want); err != nil {
		return false, err
	}
	return fi.Size() == want, nil
}

// sanitizeFilename убирает path separators и control chars, чтобы name
// от пользователя не вылез из tmpDir.
func sanitizeFilename(name string) string {
	if name == "" {
		return ""
	}
	// Берём только базу (на случай если пришёл "../../etc/passwd")
	name = filepath.Base(name)
	// Заменяем control + service chars на _
	var b strings.Builder
	for _, r := range name {
		if r < 32 || r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' {
			b.WriteRune('_')
		} else {
			b.WriteRune(r)
		}
	}
	res := b.String()
	if res == "." || res == ".." {
		return ""
	}
	return res
}

// copyFileFast — без расшифровки, просто os.Open(src) → io.Copy(dst). Используется
// в fast-path SaveAttachmentAs для уже-распакованных файлов из Vox Downloads.
func copyFileFast(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	n, err := io.Copy(out, in)
	if err != nil {
		return n, err
	}
	if err := out.Sync(); err != nil {
		return n, err
	}
	return n, nil
}

// openInDefaultApp кросс-платформенный запуск дефолтного OS-приложения для файла.
// На Windows — makeHiddenCmd со CREATE_NO_WINDOW, иначе мигает пустой cmd.exe.
func openInDefaultApp(path string) error {
	switch runtime.GOOS {
	case "windows":
		// `cmd /c start "" "<path>"` — пустой "title" аргумент обязателен,
		// иначе start интерпретирует первый кавычечный токен как title.
		return makeHiddenCmd("cmd", "/c", "start", "", path).Start()
	case "darwin":
		return exec.Command("open", path).Start()
	case "linux":
		return exec.Command("xdg-open", path).Start()
	default:
		return errors.New("unsupported OS for native open")
	}
}
