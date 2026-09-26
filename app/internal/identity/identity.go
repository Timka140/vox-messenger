package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// Generate создаёт новую пару ключей Ed25519.
func Generate() (ed25519.PrivateKey, ed25519.PublicKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("identity.Generate: %w", err)
	}
	return priv, pub, nil
}

// PeerID возвращает hex-строку публичного ключа.
func PeerID(pub ed25519.PublicKey) string {
	return hex.EncodeToString(pub)
}

// ShortID возвращает первые 16 символов PeerID.
func ShortID(pub ed25519.PublicKey) string {
	id := PeerID(pub)
	if len(id) > 16 {
		return id[:16]
	}
	return id
}
