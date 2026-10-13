package e2e

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"

	"golang.org/x/crypto/nacl/box"
)

// ErrOpen — box не открылся: не тот отправитель, не тот получатель или порча.
var ErrOpen = errors.New("e2e: box open failed")

// SealTo шифрует plaintext для to от me (NaCl box, ключи профиля через
// X25519). Формат: nonce(24) || box. Отправителя удостоверяет сам box.
func SealTo(me ed25519.PrivateKey, to ed25519.PublicKey, plaintext []byte) ([]byte, error) {
	meX, err := PrivateToX25519(me)
	if err != nil {
		return nil, err
	}
	toX, err := PublicToX25519(to)
	if err != nil {
		return nil, err
	}
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	return box.Seal(nonce[:], plaintext, &nonce, toX, meX), nil
}

// OpenFrom открывает SealTo от from для me.
func OpenFrom(me ed25519.PrivateKey, from ed25519.PublicKey, blob []byte) ([]byte, error) {
	if len(blob) < 24+box.Overhead {
		return nil, ErrOpen
	}
	meX, err := PrivateToX25519(me)
	if err != nil {
		return nil, err
	}
	fromX, err := PublicToX25519(from)
	if err != nil {
		return nil, ErrOpen
	}
	var nonce [24]byte
	copy(nonce[:], blob[:24])
	out, ok := box.Open(nil, blob[24:], &nonce, fromX, meX)
	if !ok {
		return nil, ErrOpen
	}
	return out, nil
}
