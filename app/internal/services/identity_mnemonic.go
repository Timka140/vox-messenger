package services

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"vox_app/internal/identity"
	"vox_app/internal/storage"
)

// Коды ошибок ApplyMnemonic — фронт разбирает их по тексту ошибки.
const (
	mnemonicErrWordCount   = "word_count"
	mnemonicErrUnknownWord = "unknown_word:" // + номер слова с 1
	mnemonicErrChecksum    = "checksum"
	mnemonicErrMixedLang   = "mixed_lang"
	mnemonicErrMismatch    = "phrase_mismatch"
	mnemonicErrHasProfile  = "profile_exists"
)

// MnemonicPhrase — одна и та же новая фраза на двух языках.
type MnemonicPhrase struct {
	RU string `json:"ru"`
	EN string `json:"en"`
}

// SetEmitter подключает отправку событий во фронт.
func (s *IdentityService) SetEmitter(fn func(string, any)) {
	s.emit = fn
}

// NewMnemonic генерирует секретную фразу для нового профиля. Фраза нигде не
// сохраняется и не пишется в лог.
func (s *IdentityService) NewMnemonic() (MnemonicPhrase, error) {
	const op = "IdentityService.NewMnemonic"
	e, err := identity.NewEntropy()
	if err != nil {
		return MnemonicPhrase{}, fmt.Errorf("%s: %w", op, err)
	}
	ru, err := identity.EntropyToMnemonic(e, identity.LangRU)
	if err != nil {
		return MnemonicPhrase{}, fmt.Errorf("%s: %w", op, err)
	}
	en, err := identity.EntropyToMnemonic(e, identity.LangEN)
	if err != nil {
		return MnemonicPhrase{}, fmt.Errorf("%s: %w", op, err)
	}
	return MnemonicPhrase{RU: ru, EN: en}, nil
}

// ApplyMnemonic создаёт или восстанавливает профиль из секретной фразы и
// перезапускает приложение. Возвращает peerID профиля.
//
// Если на диске есть vox.db.bak (ключ базы был утерян), сначала проверяем,
// открывает ли её ключ из фразы: да — пишем vox.key, при старте main.go поднимет
// .bak; нет — ошибка phrase_mismatch, если только discardBackup не просит
// удалить старую историю.
//
// Иначе профиль пишется в текущую базу, база копируется в vox.db.new под ключом
// из фразы, ключ — в vox.key.new; подмена файлов — при старте (main.go).
func (s *IdentityService) ApplyMnemonic(phrase string, discardBackup bool) (string, error) {
	const op = "IdentityService.ApplyMnemonic"

	if s.sess.IsActive() {
		return "", errors.New(mnemonicErrHasProfile)
	}
	entropy, lang, err := identity.ParseMnemonic(phrase)
	if err != nil {
		return "", mnemonicErrorCode(err)
	}
	priv, pub, dbKey, err := identity.DeriveKeys(entropy)
	if err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}
	peerID := identity.PeerID(pub)

	dbPath := filepath.Join(s.dataDir, "vox.db")
	bakPath := dbPath + ".bak"
	keyPath := filepath.Join(s.dataDir, "vox.key")
	newDB := filepath.Join(s.dataDir, storage.PendingDBName)
	newKey := filepath.Join(s.dataDir, storage.PendingKeyName)
	// Ключ первым: одинокий vox.key.new ApplyPendingRekey считает прерванной
	// подменой и доводит её до конца.
	_ = os.Remove(newKey)
	_ = os.Remove(newDB)

	if _, statErr := os.Stat(bakPath); statErr == nil {
		if !discardBackup {
			if !storage.KeyOpens(bakPath, dbKey) {
				s.log.Info("фраза не открывает сохранённую базу", slog.String("peer_id", identity.ShortID(pub)))
				return "", errors.New(mnemonicErrMismatch)
			}
			if err := identity.WriteDBKey(keyPath, dbKey); err != nil {
				return "", fmt.Errorf("%s: %w", op, err)
			}
			s.log.Info("ключ базы восстановлен из фразы, перезапуск",
				slog.String("peer_id", identity.ShortID(pub)), slog.String("lang", lang))
			return peerID, s.relaunch()
		}
		for _, p := range []string{bakPath, bakPath + "-wal", bakPath + "-shm"} {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return "", fmt.Errorf("%s: remove backup: %w", op, err)
			}
		}
		s.log.Info("старая база удалена по выбору пользователя")
	}

	// Профиль попадает только в копию под ключом из фразы. В текущей базе
	// (случайный ключ) строку сразу убираем: если подмена не дойдёт до конца,
	// старт покажет экран создания, а не профиль, чья база не связана с фразой.
	if _, err := s.store.DB().Exec(
		"INSERT OR REPLACE INTO identity (id, private_key, public_key) VALUES (1, ?, ?)",
		[]byte(priv), []byte(pub),
	); err != nil {
		return "", fmt.Errorf("%s: save: %w", op, err)
	}
	if err := s.store.Checkpoint(); err != nil {
		s.log.Warn("checkpoint перед перешифровкой не удался", slog.String("error", err.Error()))
	}
	exportErr := s.store.ExportEncrypted(newDB, dbKey)
	if _, err := s.store.DB().Exec("DELETE FROM identity WHERE id = 1"); err != nil {
		_ = os.Remove(newDB)
		return "", fmt.Errorf("%s: unsave: %w", op, err)
	}
	if exportErr != nil {
		_ = os.Remove(newDB)
		return "", fmt.Errorf("%s: %w", op, exportErr)
	}
	if err := identity.WriteDBKey(newKey, dbKey); err != nil {
		_ = os.Remove(newDB)
		return "", fmt.Errorf("%s: %w", op, err)
	}
	s.log.Info("профиль создан из фразы, перезапуск",
		slog.String("peer_id", identity.ShortID(pub)), slog.String("lang", lang))
	return peerID, s.relaunch()
}

func (s *IdentityService) relaunch() error {
	if err := relaunchSelf(); err != nil {
		// Файлы уже готовы: при ручном запуске подмена всё равно произойдёт.
		s.log.Error("перезапуск не удался", slog.String("error", err.Error()))
		return err
	}
	// Выход — прямым вызовом, не событием: app.Event.Emit + app.Quit из
	// обработчика того же события взаимно блокируются в Wails v3 alpha
	// (рассылка окнам держит windowsLock и ждёт главный поток, а Quit на
	// главном потоке ждёт windowsLock).
	if s.quit != nil {
		go s.quit()
	}
	return nil
}

// SetQuit — чем завершить процесс после запуска нового (main.go: app.Quit).
func (s *IdentityService) SetQuit(fn func()) { s.quit = fn }

// CopyMnemonic кладёт фразу в буфер обмена и через минуту очищает его, если
// там всё ещё она.
func (s *IdentityService) CopyMnemonic(text string) error {
	app := application.Get()
	if app == nil {
		return errors.New("IdentityService.CopyMnemonic: no app instance")
	}
	if !app.Clipboard.SetText(text) {
		return errors.New("IdentityService.CopyMnemonic: clipboard unavailable")
	}
	time.AfterFunc(mnemonicClipboardTTL, func() {
		if cur, ok := app.Clipboard.Text(); ok && cur == text {
			app.Clipboard.SetText("")
		}
	})
	return nil
}

const mnemonicClipboardTTL = time.Minute

func mnemonicErrorCode(err error) error {
	var unk *identity.ErrUnknownWord
	switch {
	case errors.As(err, &unk):
		return errors.New(mnemonicErrUnknownWord + strconv.Itoa(unk.Index))
	case errors.Is(err, identity.ErrWordCount):
		return errors.New(mnemonicErrWordCount)
	case errors.Is(err, identity.ErrChecksum):
		return errors.New(mnemonicErrChecksum)
	case errors.Is(err, identity.ErrMixedLang):
		return errors.New(mnemonicErrMixedLang)
	}
	return err
}
