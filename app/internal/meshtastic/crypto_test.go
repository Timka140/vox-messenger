package meshtastic

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
)

// TestEncryptDecryptRoundTrip — happy path: Alice encrypts to Bob with her
// Ed25519 keys, Bob decrypts with his and the original plaintext comes back.
func TestEncryptDecryptRoundTrip(t *testing.T) {
	alicePub, alicePriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("alice keygen: %v", err)
	}
	bobPub, bobPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("bob keygen: %v", err)
	}

	cases := []struct {
		name      string
		plaintext []byte
	}{
		{"empty", []byte{}},
		{"short", []byte("hi")},
		{"cyrillic", []byte("Привет, как дела?")},
		{"binary", []byte{0x00, 0xff, 0x01, 0xfe, 0x02, 0xfd}},
		{"max 150", bytes.Repeat([]byte("x"), 150)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blob, err := EncryptForPeer(bobPub, alicePriv, tc.plaintext)
			if err != nil {
				t.Fatalf("encrypt: %v", err)
			}
			if len(blob) != NonceSize+len(tc.plaintext)+MACSize {
				t.Errorf("blob size: got %d, want %d (nonce + plaintext + mac)",
					len(blob), NonceSize+len(tc.plaintext)+MACSize)
			}
			recovered, err := DecryptFromPeer(alicePub, bobPriv, blob)
			if err != nil {
				t.Fatalf("decrypt: %v", err)
			}
			if !bytes.Equal(recovered, tc.plaintext) {
				t.Errorf("plaintext mismatch:\n  got:  %x\n  want: %x", recovered, tc.plaintext)
			}
		})
	}
}

// TestDecryptWrongRecipient — Mallory has Bob's public key (it's public!) but
// not Bob's private key. She must NOT be able to read messages encrypted to Bob.
func TestDecryptWrongRecipient(t *testing.T) {
	alicePub, alicePriv, _ := ed25519.GenerateKey(rand.Reader)
	bobPub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, malloryPriv, _ := ed25519.GenerateKey(rand.Reader)

	blob, err := EncryptForPeer(bobPub, alicePriv, []byte("secret"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	_, err = DecryptFromPeer(alicePub, malloryPriv, blob)
	if err == nil {
		t.Fatal("decrypt with wrong recipient succeeded — security broken")
	}
}

// TestDecryptWrongSender — Bob got a blob and tries to decrypt claiming it was
// from Alice. If it was actually from Mallory, it must fail (the MAC is keyed
// off the sender's identity).
func TestDecryptWrongSender(t *testing.T) {
	alicePub, _, _ := ed25519.GenerateKey(rand.Reader)
	bobPub, bobPriv, _ := ed25519.GenerateKey(rand.Reader)
	_, malloryPriv, _ := ed25519.GenerateKey(rand.Reader)

	blob, err := EncryptForPeer(bobPub, malloryPriv, []byte("from mallory"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	_, err = DecryptFromPeer(alicePub, bobPriv, blob)
	if err == nil {
		t.Fatal("decrypt with wrong claimed sender succeeded — sender authentication broken")
	}
}

// TestDecryptTamperedCiphertext — flipping a byte in the ciphertext must fail
// the MAC check.
func TestDecryptTamperedCiphertext(t *testing.T) {
	alicePub, alicePriv, _ := ed25519.GenerateKey(rand.Reader)
	bobPub, bobPriv, _ := ed25519.GenerateKey(rand.Reader)

	blob, _ := EncryptForPeer(bobPub, alicePriv, []byte("important data"))
	tamper := make([]byte, len(blob))
	copy(tamper, blob)
	// Flip a bit in the ciphertext (not the nonce, to isolate MAC behaviour).
	tamper[NonceSize+2] ^= 0x01

	_, err := DecryptFromPeer(alicePub, bobPriv, tamper)
	if err == nil {
		t.Fatal("decrypt of tampered ciphertext succeeded — integrity broken")
	}
}

// TestDecryptShortBlob — input shorter than nonce+mac must be rejected
// without invoking the box primitive.
func TestDecryptShortBlob(t *testing.T) {
	alicePub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, bobPriv, _ := ed25519.GenerateKey(rand.Reader)

	for _, n := range []int{0, 1, NonceSize, MinCipherSize - 1} {
		_, err := DecryptFromPeer(alicePub, bobPriv, make([]byte, n))
		if err == nil {
			t.Errorf("decrypt of %d-byte blob unexpectedly succeeded", n)
		}
	}
}

// TestDeterministicKeyConversion — the Ed25519→X25519 mapping must be stable:
// the same Ed25519 key always converts to the same X25519 key. Otherwise
// every restart would invalidate cached shared secrets.
func TestDeterministicKeyConversion(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)

	pub1, err := Ed25519PublicToCurve25519(pub)
	if err != nil {
		t.Fatalf("pub conv: %v", err)
	}
	pub2, err := Ed25519PublicToCurve25519(pub)
	if err != nil {
		t.Fatalf("pub conv 2: %v", err)
	}
	if *pub1 != *pub2 {
		t.Errorf("public conversion not deterministic")
	}
	priv1, err := Ed25519PrivateToCurve25519(priv)
	if err != nil {
		t.Fatalf("priv conv: %v", err)
	}
	priv2, err := Ed25519PrivateToCurve25519(priv)
	if err != nil {
		t.Fatalf("priv conv 2: %v", err)
	}
	if *priv1 != *priv2 {
		t.Errorf("private conversion not deterministic")
	}
}
