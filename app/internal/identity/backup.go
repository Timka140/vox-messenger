package identity

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

const backupPrefix = "vox-key://"

var (
	ErrInvalidBackup    = errors.New("invalid backup format")
	ErrDecryptionFailed = errors.New("wrong passphrase or corrupted backup")
)

// Export шифрует приватный ключ + ключ БД passphrase и возвращает строку vox-key://...
// Plaintext: [64-byte Ed25519 private key] + [32-byte DB key] = 96 bytes.
func Export(priv ed25519.PrivateKey, dbKey []byte, passphrase string) (string, error) {
	const op = "identity.Export"

	// Склеиваем identity key + DB key в один plaintext
	plaintext := make([]byte, 0, len(priv)+len(dbKey))
	plaintext = append(plaintext, priv...)
	plaintext = append(plaintext, dbKey...)

	key := deriveKey(passphrase)

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}

	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}

	ciphertext := aead.Seal(nonce, nonce, plaintext, nil)
	encoded := base64.RawURLEncoding.EncodeToString(ciphertext)

	return backupPrefix + encoded, nil
}

// Import расшифровывает строку vox-key://... и возвращает пару ключей + ключ БД (если есть).
// Поддерживает оба формата:
//   - старый (64 байта): только Ed25519 private key, dbKey = nil
//   - новый (96 байт): Ed25519 private key (64) + DB key (32)
func Import(backupStr, passphrase string) (ed25519.PrivateKey, ed25519.PublicKey, []byte, error) {
	const op = "identity.Import"

	if !strings.HasPrefix(backupStr, backupPrefix) {
		return nil, nil, nil, fmt.Errorf("%s: %w", op, ErrInvalidBackup)
	}

	encoded := strings.TrimPrefix(backupStr, backupPrefix)
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%s: decode: %w", op, err)
	}

	key := deriveKey(passphrase)

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%s: %w", op, err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%s: %w", op, err)
	}

	nonceSize := aead.NonceSize()
	if len(data) < nonceSize {
		return nil, nil, nil, fmt.Errorf("%s: %w", op, ErrInvalidBackup)
	}

	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	plaintext, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%s: %w", op, ErrDecryptionFailed)
	}

	switch len(plaintext) {
	case ed25519.PrivateKeySize + dbKeySize:
		// Новый формат: identity + DB key
		priv := ed25519.PrivateKey(plaintext[:ed25519.PrivateKeySize])
		pub := priv.Public().(ed25519.PublicKey)
		dbKey := plaintext[ed25519.PrivateKeySize:]
		return priv, pub, dbKey, nil

	case ed25519.PrivateKeySize:
		// Старый формат: только identity (обратная совместимость)
		priv := ed25519.PrivateKey(plaintext)
		pub := priv.Public().(ed25519.PublicKey)
		return priv, pub, nil, nil

	default:
		return nil, nil, nil, fmt.Errorf("%s: invalid key size: %w", op, ErrInvalidBackup)
	}
}

// deriveKey выводит 32-байтный AES ключ из passphrase через SHA-256.
func deriveKey(passphrase string) []byte {
	h := sha256.Sum256([]byte("nexus-backup-" + passphrase))
	return h[:]
}
