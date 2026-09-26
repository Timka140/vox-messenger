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

	data, err := os.ReadFile(path)
	if err == nil {
		raw := strings.TrimSpace(string(data))
		key, err := hex.DecodeString(raw)
		if err != nil || len(key) != dbKeySize {
			return nil, fmt.Errorf("%s: invalid key file", op)
		}
		return key, nil
	}

	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("%s: read: %w", op, err)
	}

	// Генерируем новый ключ
	key := make([]byte, dbKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("%s: generate: %w", op, err)
	}

	encoded := hex.EncodeToString(key)
	if err := os.WriteFile(path, []byte(encoded), 0600); err != nil {
		return nil, fmt.Errorf("%s: write: %w", op, err)
	}

	return key, nil
}
