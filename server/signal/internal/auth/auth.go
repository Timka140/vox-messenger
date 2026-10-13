package auth

import (
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
