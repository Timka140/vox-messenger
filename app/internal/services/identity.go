package services

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"vox_app/internal/logsafe"
	"vox_app/internal/identity"
	"vox_app/internal/session"
	"vox_app/internal/storage"
	"os"
	"path/filepath"
)

type IdentityService struct {
	log     *slog.Logger
	sess    *session.Session
	store   *storage.Storage
	dataDir string
	network *NetworkService
}

// NewIdentityService принимает network для UpdateNickTag + BroadcastNicknameUpdate
// при смене ника (единственные исходящие вызовы из identity в сеть).
func NewIdentityService(log *slog.Logger, sess *session.Session, store *storage.Storage, dataDir string, network *NetworkService) *IdentityService {
	return &IdentityService{
		log:     log,
		sess:    sess,
		store:   store,
		dataDir: dataDir,
		network: network,
	}
}

// HasIdentity проверяет, есть ли сохранённая идентичность в БД.
func (s *IdentityService) HasIdentity() (bool, error) {
	var count int
	err := s.store.DB().QueryRow("SELECT COUNT(*) FROM identity").Scan(&count)
	if err != nil {
		return false, fmt.Errorf("HasIdentity: %w", err)
	}
	return count > 0, nil
}

// CreateIdentity генерирует новую пару ключей, сохраняет в БД и активирует сессию.
// Возвращает peerID (hex публичного ключа).
func (s *IdentityService) CreateIdentity(passphrase string) (string, error) {
	const op = "IdentityService.CreateIdentity"

	// Проверяем что сессия ещё не активна ДО генерации и записи в БД,
	// иначе повторный вызов перезапишет identity в БД но упадёт на sess.Set
	if s.sess.IsActive() {
		return "", fmt.Errorf("%s: session: %w", op, session.ErrAlreadySet)
	}

	priv, pub, err := identity.Generate()
	if err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}

	if err := s.sess.Set(priv, pub); err != nil {
		return "", fmt.Errorf("%s: session: %w", op, err)
	}

	_, err = s.store.DB().Exec(
		"INSERT OR REPLACE INTO identity (id, private_key, public_key) VALUES (1, ?, ?)",
		[]byte(priv), []byte(pub),
	)
	if err != nil {
		s.sess.Clear()
		return "", fmt.Errorf("%s: save: %w", op, err)
	}

	peerID := identity.PeerID(pub)
	s.log.Info("Identity created", slog.String("peer_id", identity.ShortID(pub)))

	// Принудительный checkpoint WAL — identity не должен теряться при kill процесса
	if err := s.store.Checkpoint(); err != nil {
		s.log.Warn("checkpoint после создания identity не удался", slog.String("error", err.Error()))
	}

	return peerID, nil
}

// LoadIdentity загружает идентичность из БД и активирует сессию.
// Возвращает peerID.
func (s *IdentityService) LoadIdentity() (string, error) {
	const op = "IdentityService.LoadIdentity"

	var privBytes, pubBytes []byte
	err := s.store.DB().QueryRow(
		"SELECT private_key, public_key FROM identity WHERE id = 1",
	).Scan(&privBytes, &pubBytes)
	if err != nil {
		return "", fmt.Errorf("%s: load: %w", op, err)
	}

	priv := ed25519.PrivateKey(privBytes)
	pub := ed25519.PublicKey(pubBytes)

	if err := s.sess.Set(priv, pub); err != nil {
		// Повторный вызов (перезагрузка страницы в WebView2 — F5 при включённом
		// DevTools): сессия уже держит эту же identity. Возвращаем её id
		// идемпотентно, иначе фронт падал в showSetup() при живом аккаунте.
		if errors.Is(err, session.ErrAlreadySet) {
			if cur, e2 := s.sess.PeerID(); e2 == nil && cur == identity.PeerID(pub) {
				return cur, nil
			}
		}
		return "", fmt.Errorf("%s: session: %w", op, err)
	}

	peerID := identity.PeerID(pub)
	s.log.Info("Identity loaded", slog.String("peer_id", identity.ShortID(pub)))
	return peerID, nil
}

// ExportKey шифрует приватный ключ + ключ БД passphrase и возвращает строку vox-key://...
func (s *IdentityService) ExportKey(passphrase string) (string, error) {
	priv, err := s.sess.PrivateKey()
	if err != nil {
		return "", fmt.Errorf("ExportKey: %w", err)
	}

	// Читаем ключ шифрования БД для включения в бэкап
	dbKeyPath := filepath.Join(s.dataDir, "vox.key")
	dbKey, err := identity.LoadOrCreateDBKey(dbKeyPath)
	if err != nil {
		return "", fmt.Errorf("ExportKey: load db key: %w", err)
	}

	return identity.Export(priv, dbKey, passphrase)
}

// ImportKey импортирует ключ из строки vox-key://..., сохраняет в БД, активирует сессию.
// Если бэкап содержит ключ БД (новый формат) — восстанавливает vox.key.
func (s *IdentityService) ImportKey(keyStr, passphrase string) (string, error) {
	const op = "IdentityService.ImportKey"

	priv, pub, dbKey, err := identity.Import(keyStr, passphrase)
	if err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}

	// Если бэкап содержит ключ БД — восстанавливаем vox.key
	if len(dbKey) > 0 {
		dbKeyPath := filepath.Join(s.dataDir, "vox.key")
		encoded := hex.EncodeToString(dbKey)
		if err := os.WriteFile(dbKeyPath, []byte(encoded), 0600); err != nil {
			s.log.Warn("не удалось восстановить vox.key", slog.String("error", err.Error()))
		} else {
			s.log.Info("vox.key восстановлен из бэкапа")
		}
	}

	_, err = s.store.DB().Exec(
		"INSERT OR REPLACE INTO identity (id, private_key, public_key) VALUES (1, ?, ?)",
		[]byte(priv), []byte(pub),
	)
	if err != nil {
		return "", fmt.Errorf("%s: save: %w", op, err)
	}

	if err := s.sess.Set(priv, pub); err != nil {
		return "", fmt.Errorf("%s: session: %w", op, err)
	}

	peerID := identity.PeerID(pub)
	s.log.Info("Identity imported", slog.String("peer_id", identity.ShortID(pub)))

	// Принудительный checkpoint WAL — identity не должен теряться при kill процесса
	if err := s.store.Checkpoint(); err != nil {
		s.log.Warn("checkpoint после импорта identity не удался", slog.String("error", err.Error()))
	}

	return peerID, nil
}

// GetMyID возвращает полный peerID.
func (s *IdentityService) GetMyID() (string, error) {
	return s.sess.PeerID()
}

// GetMyShortID возвращает первые 16 символов peerID.
func (s *IdentityService) GetMyShortID() (string, error) {
	pub, err := s.sess.PublicKey()
	if err != nil {
		return "", err
	}
	return identity.ShortID(pub), nil
}

// GetMyTag возвращает тег вида Nickname#shortID.
func (s *IdentityService) GetMyTag() (string, error) {
	nick, _ := s.GetNickname()
	shortID, err := s.GetMyShortID()
	if err != nil {
		return "", err
	}
	if nick == "" {
		nick = shortID
	}
	return nick + "#" + shortID, nil
}

// GetNickname возвращает сохранённый никнейм.
func (s *IdentityService) GetNickname() (string, error) {
	var val string
	err := s.store.DB().QueryRow(
		"SELECT value FROM settings WHERE key = 'nickname'",
	).Scan(&val)
	if err != nil {
		return "", nil // нет никнейма — не ошибка
	}
	return val, nil
}

// SaveSetting сохраняет произвольную настройку.
func (s *IdentityService) SaveSetting(key, value string) error {
	_, err := s.store.DB().Exec(
		"INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)", key, value)
	return err
}

// GetSetting возвращает значение настройки.
func (s *IdentityService) GetSetting(key string) (string, error) {
	var value string
	err := s.store.DB().QueryRow(
		"SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	if err != nil {
		return "", nil // не найдено — пустая строка, не ошибка
	}
	return value, nil
}

// SetNickname сохраняет никнейм.
func (s *IdentityService) SetNickname(nickname string) error {
	if len(nickname) > 100 {
		return fmt.Errorf("никнейм слишком длинный (макс 100 символов)")
	}
	_, err := s.store.DB().Exec(
		"INSERT OR REPLACE INTO settings (key, value) VALUES ('nickname', ?)",
		nickname,
	)
	if err != nil {
		return err
	}

	s.log.Info("ник обновлён", logsafe.Len("len", nickname))

	if s.network != nil {
		// Обновляем nick_tag на сигналке in-place (не разрывая WS — иначе падают все P2P).
		if err := s.network.UpdateNickTag(); err != nil {
			s.log.Warn("не удалось обновить nick_tag на сигналке", slog.String("error", err.Error()))
		}

		// Рассылаем обновление ника всем подключённым пирам через P2P
		s.network.BroadcastNicknameUpdate(nickname)
	}

	return nil
}

// GetMyContactURI возвращает URI для добавления контакта: vox://peerID.
func (s *IdentityService) GetMyContactURI() (string, error) {
	peerID, err := s.sess.PeerID()
	if err != nil {
		return "", err
	}

	pub, err := s.sess.PublicKey()
	if err != nil {
		return "", err
	}

	return "vox://" + peerID + "/" + hex.EncodeToString(pub), nil
}
