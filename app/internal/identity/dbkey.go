package identity

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

const dbKeySize = 32

// LoadOrCreateDBKey загружает ключ БД из файла (hex), или создаёт новый.
func LoadOrCreateDBKey(path string) ([]byte, error) {
	const op = "identity.LoadOrCreateDBKey"

	key, err := ReadDBKey(path)
	if err == nil {
		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("%s: read: %w", op, err)
	}

	// Генерируем новый ключ
	key = make([]byte, dbKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("%s: generate: %w", op, err)
	}

	encoded := hex.EncodeToString(key)
	if err := os.WriteFile(path, []byte(encoded), 0600); err != nil {
		return nil, fmt.Errorf("%s: write: %w", op, err)
	}

	return key, nil
}

// ReadDBKey читает ключ БД из файла (hex). Нет файла — ошибка с os.IsNotExist.
func ReadDBKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(key) != dbKeySize {
		return nil, fmt.Errorf("identity.ReadDBKey: invalid key file")
	}
	return key, nil
}

// WriteDBKey записывает ключ БД (hex, 0600) атомарно: через временный файл и rename.
func WriteDBKey(path string, key []byte) error {
	const op = "identity.WriteDBKey"
	if len(key) != dbKeySize {
		return fmt.Errorf("%s: key must be %d bytes", op, dbKeySize)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(hex.EncodeToString(key)), 0600); err != nil {
		return fmt.Errorf("%s: write: %w", op, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("%s: rename: %w", op, err)
	}
	return nil
}
