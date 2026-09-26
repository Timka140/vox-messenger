package services

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

// TestVerifyLogic проверяет что логика SHA-256 + Ed25519 verify (как в DownloadUpdate)
// корректно принимает валидную подпись и отвергает тамперные данные.
func TestVerifyLogic(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	// Оригинальные "bytes бинаря"
	content := []byte("this is a release binary payload for testing")
	sum := sha256.Sum256(content)
	sig := ed25519.Sign(priv, sum[:])

	sigB64 := base64.StdEncoding.EncodeToString(sig)
	sumHex := hex.EncodeToString(sum[:])

	// Симулируем download path: hasher считает sha256 → сверяем с ожидаемым → verify
	h := sha256.New()
	h.Write(content)
	gotSum := h.Sum(nil)
	gotSumHex := hex.EncodeToString(gotSum)

	if gotSumHex != sumHex {
		t.Fatal("sha256 mismatch in sane path")
	}

	sigBytes, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(pub, gotSum, sigBytes) {
		t.Fatal("valid signature rejected")
	}

	// Тампер: меняем содержимое → sha256 другой → verify упадёт
	tampered := append([]byte{}, content...)
	tampered[0] ^= 0xff
	h2 := sha256.New()
	h2.Write(tampered)
	tamperedSum := h2.Sum(nil)

	if ed25519.Verify(pub, tamperedSum, sigBytes) {
		t.Fatal("tampered content accepted")
	}

	// Тампер подписи
	sigTampered := append([]byte{}, sigBytes...)
	sigTampered[0] ^= 0xff
	if ed25519.Verify(pub, gotSum, sigTampered) {
		t.Fatal("tampered signature accepted")
	}
}

// TestManifestSignatureFlow проверяет что подпись манифеста считается и
// верифицируется тем же способом что и в sign-release tool'е — ed25519.Sign
// над sha256(manifestBytes). Это критично для новой схемы v2.
func TestManifestSignatureFlow(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	manifestJSON := []byte(`{"version":2,"latest":"v0.00.000.0001","releases":[]}`)
	sum := sha256.Sum256(manifestJSON)
	sig := ed25519.Sign(priv, sum[:])

	// Передача sig через X-Signature header (base64)
	sigB64 := base64.StdEncoding.EncodeToString(sig)

	// На recv стороне: decode sig, compute sha256 of raw bytes, verify
	decodedSig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		t.Fatal(err)
	}
	recvSum := sha256.Sum256(manifestJSON)
	if !ed25519.Verify(pub, recvSum[:], decodedSig) {
		t.Fatal("valid manifest signature rejected")
	}

	// Тампер манифеста — подпись должна упасть
	tampered := append([]byte{}, manifestJSON...)
	tampered[10] ^= 0xff
	tamperedSum := sha256.Sum256(tampered)
	if ed25519.Verify(pub, tamperedSum[:], decodedSig) {
		t.Fatal("tampered manifest accepted")
	}
}
