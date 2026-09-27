package storage

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	_ "github.com/mutecomm/go-sqlcipher/v4"
)

// Storage — обёртка над зашифрованной SQLite (SQLCipher).
type Storage struct {
	db *sql.DB
}

// Open открывает (или создаёт) зашифрованную БД.
// dbKey — 32-байтный ключ шифрования (из session.DBKey()).
//
// Safety net против WAL-lock race при autoupdate: PRAGMA journal_mode=WAL —
// file-level операция, которая виснет в C-коде sqlite если другой процесс
// держит БД (busy_timeout не покрывает file-level locks). Чтобы не виснуть
// бесконечно, делаем PRAGMA через retry с backoff. waitForParentExit в main.go
// уже должен был дождаться смерти старого процесса, но это страховка на случай
// если PID не передался / процесс висит дольше timeout.
func Open(path string, dbKey []byte) (*Storage, error) {
	const op = "storage.Open"

	dsn := path + fmt.Sprintf("?_pragma_key=x'%s'&_pragma_cipher_page_size=4096", hex.EncodeToString(dbKey))

	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("%s: open: %w", op, err)
	}

	// Проверяем что ключ верный (с retry для случая когда другой процесс
	// держит файл, например после autoupdate — старый exe ещё жив).
	if err := pingWithRetry(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: ping (wrong key or locked?): %w", op, err)
	}

	// WAL mode + busy_timeout с retry — см. комментарий выше про file-level lock.
	if err := execWithRetry(db, "PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: WAL mode: %w", op, err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: busy_timeout: %w", op, err)
	}

	s := &Storage{db: db}

	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: migrate: %w", op, err)
	}

	return s, nil
}

// pingWithRetry — ping БД с повторами; покрывает race с предыдущим процессом
// на autoupdate (старый exe ещё держит БД).
func pingWithRetry(db *sql.DB) error {
	const maxAttempts = 30
	const delay = 500 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := db.Ping(); err == nil {
			return nil
		} else {
			lastErr = err
		}
		// Чужой ключ ожиданием не лечится — повторы только для занятого файла.
		if IsWrongKey(lastErr) {
			return lastErr
		}
		time.Sleep(delay)
	}
	return lastErr
}

// execWithRetry — Exec PRAGMA с повторами; см. pingWithRetry.
func execWithRetry(db *sql.DB, query string) error {
	const maxAttempts = 30
	const delay = 500 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if _, err := db.Exec(query); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(delay)
	}
	return lastErr
}

// OpenInMemory открывает in-memory БД без шифрования (для тестов).
func OpenInMemory() (*Storage, error) {
	db, err := sql.Open("sqlite3", "file::memory:?cache=shared")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("in-memory: busy_timeout: %w", err)
	}

	s := &Storage{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close закрывает соединение с БД.
func (s *Storage) Close() error {
	return s.db.Close()
}

// DB возвращает *sql.DB для прямых запросов.
func (s *Storage) DB() *sql.DB {
	return s.db
}

// ExportEncrypted копирует всю БД в новый файл path, зашифрованный ключом newKey
// (ATTACH + sqlcipher_export на одном соединении пула). Существующий path удаляется.
// Используется при создании профиля: база перешифровывается ключом из секретной
// фразы, а файлы подменяются при следующем старте.
func (s *Storage) ExportEncrypted(path string, newKey []byte) error {
	const op = "storage.ExportEncrypted"
	if len(newKey) != 32 {
		return fmt.Errorf("%s: key must be 32 bytes", op)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("%s: remove old: %w", op, err)
	}

	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("%s: conn: %w", op, err)
	}
	defer conn.Close()

	keyLit := fmt.Sprintf("x'%s'", hex.EncodeToString(newKey))
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS vox_export KEY "`+keyLit+`"`, path); err != nil {
		return fmt.Errorf("%s: attach: %w", op, err)
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA vox_export.cipher_page_size = 4096"); err != nil {
		_, _ = conn.ExecContext(ctx, "DETACH DATABASE vox_export")
		return fmt.Errorf("%s: page size: %w", op, err)
	}
	if _, err := conn.ExecContext(ctx, "SELECT sqlcipher_export('vox_export')"); err != nil {
		_, _ = conn.ExecContext(ctx, "DETACH DATABASE vox_export")
		return fmt.Errorf("%s: export: %w", op, err)
	}
	if _, err := conn.ExecContext(ctx, "DETACH DATABASE vox_export"); err != nil {
		return fmt.Errorf("%s: detach: %w", op, err)
	}
	return nil
}

// Checkpoint принудительно сбрасывает WAL в основной файл БД.
// Вызывать после критических операций (создание identity, импорт ключа),
// чтобы данные не потерялись при аварийном завершении процесса.
func (s *Storage) Checkpoint() error {
	_, err := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return err
}
