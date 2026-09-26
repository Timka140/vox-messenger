// Package identity управляет Ed25519 ключом сигнального сервера.
// Ключ хранится hex-encoded в файле vox-signal.key рядом с бинарём
// (аналогично клиентскому vox.key). При первом запуске генерируется
// автоматически. Pubkey предъявляется клиенту в challenge-сообщении,
// а свежий per-connection nonce подписывается приватником в auth_ok —
// что даёт клиенту mutual auth (защита от MITM поверх адреса).
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// Identity — пара Ed25519 ключей сигнального сервера.
type Identity struct {
	Public  ed25519.PublicKey
	Private ed25519.PrivateKey
}

// LoadOrCreate читает ключ из path; если файла нет — генерирует новый
// и сохраняет с правами 0600. Файл хранит hex-encoded SeedSize=32 байта
// (приватник восстанавливается через ed25519.NewKeyFromSeed).
func LoadOrCreate(path string) (*Identity, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return parseSeedHex(strings.TrimSpace(string(data)))
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	seedHex := hex.EncodeToString(priv.Seed())
	if err := os.WriteFile(path, []byte(seedHex+"\n"), 0600); err != nil {
		return nil, fmt.Errorf("write %s: %w", path, err)
	}
	return &Identity{Public: pub, Private: priv}, nil
}

func parseSeedHex(s string) (*Identity, error) {
	seed, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("decode seed hex: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("invalid seed size %d, want %d", len(seed), ed25519.SeedSize)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	return &Identity{Public: pub, Private: priv}, nil
}

// PubKeyHex возвращает публичный ключ в hex-encoded виде (для логов и для
// раздачи hardcoded-списка клиентам).
func (id *Identity) PubKeyHex() string {
	return hex.EncodeToString(id.Public)
}
