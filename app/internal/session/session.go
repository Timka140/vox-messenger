package session

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"sync"
)

var (
	ErrNoIdentity = errors.New("no active identity")
	ErrAlreadySet = errors.New("session already active")
)

// Session хранит загруженную идентичность в памяти на время работы приложения.
type Session struct {
	mu         sync.RWMutex
	privateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey
	peerID     string // hex(publicKey)
}

func New() *Session {
	return &Session{}
}

// Set сохраняет ключи после успешной авторизации.
func (s *Session) Set(priv ed25519.PrivateKey, pub ed25519.PublicKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.privateKey != nil {
		return ErrAlreadySet
	}

	s.privateKey = make(ed25519.PrivateKey, len(priv))
	copy(s.privateKey, priv)

	s.publicKey = make(ed25519.PublicKey, len(pub))
	copy(s.publicKey, pub)

	s.peerID = hex.EncodeToString(pub)
	return nil
}

// IsActive проверяет, авторизован ли пользователь.
func (s *Session) IsActive() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.privateKey != nil
}

// PeerID возвращает hex-идентификатор текущего пользователя.
func (s *Session) PeerID() (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.privateKey == nil {
		return "", ErrNoIdentity
	}
	return s.peerID, nil
}

// PrivateKey возвращает копию приватного ключа.
func (s *Session) PrivateKey() (ed25519.PrivateKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.privateKey == nil {
		return nil, ErrNoIdentity
	}
	key := make(ed25519.PrivateKey, len(s.privateKey))
	copy(key, s.privateKey)
	return key, nil
}

// PublicKey возвращает копию публичного ключа.
func (s *Session) PublicKey() (ed25519.PublicKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.privateKey == nil {
		return nil, ErrNoIdentity
	}
	key := make(ed25519.PublicKey, len(s.publicKey))
	copy(key, s.publicKey)
	return key, nil
}

// Clear очищает сессию (logout), зануляет ключи в памяти.
func (s *Session) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.privateKey {
		s.privateKey[i] = 0
	}
	s.privateKey = nil
	s.publicKey = nil
	s.peerID = ""
}
