// Package e2e — сквозное шифрование поверх серверов Vox: групповые сообщения
// через relay сигналки и голос через SFU. Ключи — те же Ed25519 профиля,
// для обмена ключами переводятся в X25519 (одна группа точек, отдельной пары
// не нужно). Так же устроен LoRa-транспорт (internal/meshtastic/crypto.go).
package e2e

import (
	"crypto/ed25519"
	"crypto/sha512"
	"fmt"

	"filippo.io/edwards25519"
)

// KeySize — длина ключа X25519.
const KeySize = 32

// PublicToX25519 переводит публичный ключ Ed25519 (форма Эдвардса) в X25519
// (форма Монтгомери). Ошибка — не точка кривой.
func PublicToX25519(pub ed25519.PublicKey) (*[KeySize]byte, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("e2e: ed25519 pubkey: %d bytes", len(pub))
	}
	p, err := new(edwards25519.Point).SetBytes(pub)
	if err != nil {
		return nil, fmt.Errorf("e2e: ed25519 pubkey decode: %w", err)
	}
	var out [KeySize]byte
	copy(out[:], p.BytesMontgomery())
	return &out, nil
}

// PrivateToX25519 переводит закрытый ключ Ed25519 (seed||pub) в скаляр X25519
// (RFC 8032: SHA-512(seed)[:32] с clamp по RFC 7748).
func PrivateToX25519(priv ed25519.PrivateKey) (*[KeySize]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("e2e: ed25519 privkey: %d bytes", len(priv))
	}
	digest := sha512.Sum512(priv[:32])
	var out [KeySize]byte
	copy(out[:], digest[:32])
	out[0] &= 248
	out[31] &= 127
	out[31] |= 64
	return &out, nil
}
