package services

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// «Диагностика» в «О программе» (фаза 7d, макет SettingsAbout): папка логов,
// архив логов и «Подробное логирование» до перезапуска.

// logArchiveKeep — сколько прошлых сессий класть в архив (как logRotateKeep
// в main.go: vox-app.log.1 … .4).
const logArchiveKeep = 4

// VerboseLogging — состояние тумблера «Подробное логирование».
type VerboseLogging struct {
	On     bool `json:"on"`
	Always bool `json:"always"` // сборка и так пишет Debug (local/dev) — тумблер заблокирован
}

// SetLogControl — из main.go: общий LevelVar обработчиков, уровень окружения
// и путь к текущему логу.
func (s *AppService) SetLogControl(level *slog.LevelVar, base slog.Level, logPath string) {
	s.logLevel = level
	s.logBase = base
	s.logPath = logPath
}

func (s *AppService) GetVerboseLogging() VerboseLogging {
	if s.logLevel == nil {
		return VerboseLogging{}
	}
	return VerboseLogging{
		On:     s.logLevel.Level() <= slog.LevelDebug,
		Always: s.logBase <= slog.LevelDebug,
	}
}

// SetVerboseLogging — Debug до перезапуска (в базу не пишется); off —
// обратно к уровню окружения.
func (s *AppService) SetVerboseLogging(on bool) error {
	if s.logLevel == nil {
		return errors.New("AppService.SetVerboseLogging: log control not set")
	}
	lvl := s.logBase
	if on && lvl > slog.LevelDebug {
		lvl = slog.LevelDebug
	}
	s.log.Info("verbose logging", slog.Bool("on", on), slog.String("level", lvl.String()))
	s.logLevel.Set(lvl)
	return nil
}

// OpenLogsFolder — Проводник с выделенным vox-app.log.
func (s *AppService) OpenLogsFolder() error {
	const op = "AppService.OpenLogsFolder"
	if s.logPath == "" {
		return fmt.Errorf("%s: log path not set", op)
	}
	path, err := filepath.Abs(s.logPath)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if err := revealInFolder(path); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	s.log.Info("logs folder opened", slog.String("path", path))
	return nil
}

// ExportLogsArchive — нативный «Сохранить как» и zip текущего лога с прошлыми
// сессиями. Отмена — "" без ошибки.
func (s *AppService) ExportLogsArchive() (string, error) {
	const op = "AppService.ExportLogsArchive"
	if s.logPath == "" {
		return "", fmt.Errorf("%s: log path not set", op)
	}
	app := application.Get()
	if app == nil {
		return "", fmt.Errorf("%s: no app instance", op)
	}
	name := "vox-logs-" + time.Now().Format("20060102-1504") + ".zip"
	dst, err := app.Dialog.SaveFile().SetFilename(name).PromptForSingleSelection()
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "cancel") {
			return "", nil
		}
		return "", fmt.Errorf("%s: dialog: %w", op, err)
	}
	if dst == "" {
		return "", nil
	}
	n, err := writeLogsZip(dst, s.logPath)
	if err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}
	s.log.Info("logs exported", slog.String("path", dst), slog.Int("files", n))
	return dst, nil
}

// writeLogsZip пишет в dst архив из logPath и существующих logPath.1…N
// (маркеры .unsent не берёт). Через временный файл рядом — битый архив на
// месте выбранного не останется. Текущий лог читается при открытом на запись
// дескрипторе: Go открывает файлы с FILE_SHARE_READ|WRITE.
func writeLogsZip(dst, logPath string) (int, error) {
	names := []string{logPath}
	for i := 1; i <= logArchiveKeep; i++ {
		names = append(names, fmt.Sprintf("%s.%d", logPath, i))
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".*.tmp")
	if err != nil {
		return 0, err
	}
	tmpPath := tmp.Name()
	fail := func(err error) (int, error) {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return 0, err
	}

	zw := zip.NewWriter(tmp)
	n := 0
	for _, p := range names {
		f, err := os.Open(p)
		if err != nil {
			continue // такой сессии ещё нет
		}
		hdr := &zip.FileHeader{Name: filepath.Base(p), Method: zip.Deflate}
		if st, err := f.Stat(); err == nil {
			hdr.Modified = st.ModTime()
		}
		w, err := zw.CreateHeader(hdr)
		if err == nil {
			_, err = io.Copy(w, f)
		}
		f.Close()
		if err != nil {
			return fail(err)
		}
		n++
	}
	if n == 0 {
		return fail(errors.New("логов нет"))
	}
	if err := zw.Close(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return 0, err
	}
	// На Windows Rename не перезапишет существующий файл — сначала удаляем.
	_ = os.Remove(dst)
	if err := os.Rename(tmpPath, dst); err != nil {
		_ = os.Remove(tmpPath)
		return 0, err
	}
	return n, nil
}
