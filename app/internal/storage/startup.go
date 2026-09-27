package storage

import (
	"bytes"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	sqlite3 "github.com/mutecomm/go-sqlcipher/v4"
)

// IsWrongKey — ошибка Open означает, что ключ не подходит к файлу (или файл
// не база). Занятость, миграция и I/O сюда не относятся.
func IsWrongKey(err error) bool {
	var se sqlite3.Error
	return errors.As(err, &se) && se.Code == sqlite3.ErrNotADB
}

// OpenAtStartup открывает vox.db при старте и разбирает vox.db.bak.
//
// Базу, которую текущий ключ не открывает (ключ утерян), переносит в .bak и
// создаёт пустую — recovery=true, экран восстановления. Любая другая ошибка
// открытия возвращается как есть: файлы не трогаем, история не должна уехать
// в .bak из-за блокировки или сбоя миграции.
//
// Рядом с открытой базой .bak:
//   - база пустая, а .bak открывается текущим ключом — .bak ставится на место;
//   - база пустая, .bak чужим ключом — recovery=true (восстановить фразой);
//   - в базе тот же профиль, что и в .bak, — копия уже перенесена, удаляем;
//   - другой профиль — не трогаем.
func OpenAtStartup(log *slog.Logger, dbPath string, key []byte) (store *Storage, recovery bool, err error) {
	bakPath := dbPath + ".bak"

	store, err = Open(dbPath, key)
	if err != nil {
		if !IsWrongKey(err) {
			return nil, false, err
		}
		if KeyOpens(bakPath, key) {
			// Ключ восстановлен из бэкапа — .bak и есть рабочая база.
			log.Info("текущий ключ открывает .bak — восстанавливаю из неё")
			if err := restoreBak(log, dbPath); err != nil {
				return nil, false, err
			}
			if store, err = Open(dbPath, key); err != nil {
				return nil, false, fmt.Errorf("после восстановления .bak: %w", err)
			}
			return store, false, nil
		}
		if fileExists(dbPath) {
			aside := bakPath
			if fileExists(bakPath) {
				// В .bak уже лежит неразобранная история — не затираем её,
				// а эту базу откладываем отдельной копией.
				aside = dbPath + ".bak-" + strconv.FormatInt(time.Now().Unix(), 10)
			}
			log.Warn("БД не открывается текущим ключом (ключ утерян?), откладываю копию и пересоздаю",
				slog.String("error", err.Error()), slog.String("backup", aside))
			if err := moveDBFiles(dbPath, aside); err != nil {
				return nil, false, err
			}
		}
		if store, err = Open(dbPath, key); err != nil {
			return nil, false, fmt.Errorf("после пересоздания: %w", err)
		}
		return store, true, nil
	}

	if !fileExists(bakPath) {
		return store, false, nil
	}
	pub, err := identityPub(store.DB())
	switch {
	case err != nil:
		// Профиль не проверить — загрузка профиля покажет ошибку.
		log.Error("проверка профиля рядом с .bak не удалась", slog.String("error", err.Error()))
		return store, false, nil
	case pub == nil:
		if !KeyOpens(bakPath, key) {
			// Пустая база прошлого экрана восстановления: .bak ждёт фразу.
			return store, true, nil
		}
		log.Info("база пустая, а .bak открывается текущим ключом — восстанавливаю из неё")
		store.Close()
		if err := restoreBak(log, dbPath); err != nil {
			return nil, false, err
		}
		if store, err = Open(dbPath, key); err != nil {
			return nil, false, fmt.Errorf("после восстановления .bak: %w", err)
		}
		return store, false, nil
	}

	if bakPub, err := keyedIdentityPub(bakPath, key); err == nil && bytes.Equal(bakPub, pub) {
		// Тот же профиль — копия уже сделанного восстановления фразой.
		if err := removeDBFiles(bakPath); err != nil {
			log.Warn("старую .bak удалить не удалось", slog.String("error", err.Error()))
		} else {
			log.Info("удалена старая .bak, уже перенесённая в рабочую базу")
		}
		return store, false, nil
	}
	log.Warn("рядом с рабочей базой лежит .bak другого профиля, не трогаю", slog.String("backup", bakPath))
	return store, false, nil
}

// restoreBak ставит vox.db.bak на место vox.db.
func restoreBak(log *slog.Logger, dbPath string) error {
	if err := removeDBFiles(dbPath); err != nil {
		return err
	}
	if err := moveDBFiles(dbPath+".bak", dbPath); err != nil {
		return err
	}
	log.Info("БД восстановлена из .bak")
	return nil
}

// identityPub — публичный ключ профиля в базе, nil если профиля нет.
func identityPub(db *sql.DB) ([]byte, error) {
	var pub []byte
	err := db.QueryRow("SELECT public_key FROM identity WHERE id = 1").Scan(&pub)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return pub, err
}

// keyedIdentityPub читает публичный ключ профиля из другой базы без миграций
// и без повторов Open.
func keyedIdentityPub(path string, key []byte) ([]byte, error) {
	dsn := path + fmt.Sprintf("?_pragma_key=x'%s'&_pragma_cipher_page_size=4096", hex.EncodeToString(key))
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return identityPub(db)
}

// moveDBFiles переносит базу SQLite вместе с -wal/-shm: без WAL в копии
// пропали бы последние записи.
func moveDBFiles(from, to string) error {
	if err := removeDBFiles(to); err != nil {
		return err
	}
	for _, sfx := range []string{"", "-wal", "-shm"} {
		if err := os.Rename(from+sfx, to+sfx); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("перенос %s: %w", from+sfx, err)
		}
	}
	return nil
}

func removeDBFiles(path string) error {
	for _, sfx := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + sfx); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("удаление %s: %w", path+sfx, err)
		}
	}
	return nil
}
