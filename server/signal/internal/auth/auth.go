package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

const challengeSize = 32 // 32 bytes = 64 hex chars

// GenerateChallenge создаёт случайный challenge для аутентификации
func GenerateChallenge() (string, error) {
	buf := make([]byte, challengeSize)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth.GenerateChallenge: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// VerifySignature проверяет Ed25519 подпись challenge
func VerifySignature(publicKeyHex, challenge, signatureHex string) error {
	pubKeyBytes, err := hex.DecodeString(publicKeyHex)
	if err != nil {
		return fmt.Errorf("invalid public key hex: %w", err)
	}
	if len(pubKeyBytes) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid public key length: %d", len(pubKeyBytes))
	}

	sigBytes, err := hex.DecodeString(signatureHex)
	if err != nil {
		return fmt.Errorf("invalid signature hex: %w", err)
	}
	if len(sigBytes) != ed25519.SignatureSize {
		return fmt.Errorf("invalid signature length: %d", len(sigBytes))
	}

	challengeBytes, err := hex.DecodeString(challenge)
	if err != nil {
		return fmt.Errorf("invalid challenge hex: %w", err)
	}

	pubKey := ed25519.PublicKey(pubKeyBytes)
	if !ed25519.Verify(pubKey, challengeBytes, sigBytes) {
		return fmt.Errorf("signature verification failed")
	}

	return nil
}
