package meshtastic

// Application-layer encryption for Vox messages travelling over LoRa.
//
// Why this exists: WebRTC's DataChannel transport is wrapped in DTLS-SRTP and
// gives Vox transport-level encryption for free. LoRa via Meshtastic has no
// equivalent — Meshtastic's per-channel AES uses a shared PSK that everyone
// on that channel can decrypt. To deliver a Vox DM privately to one specific
// friend through the public mesh, we encrypt with their public key first.
//
// Crypto choices:
//   - X25519 ECDH for shared secret (peer's public + our private).
//   - XSalsa20-Poly1305 (NaCl box) for authenticated encryption.
//   - Ed25519 → X25519 key conversion: Vox identities are Ed25519 keys, but
//     X25519 ECDH operates on Curve25519 in Montgomery form. Both curves share
//     the same group, so a deterministic mapping exists. Same identity, no
//     extra keypair to manage.
//
// Wire format (the bytes we put inside Meshtastic Data.payload on PRIVATE_APP):
//
//   | nonce (24 bytes) | ciphertext (N bytes) | poly1305 tag (16 bytes) |
//
// Sender identity is NOT carried inline — recipient looks up the source
// Meshtastic nodeNum (from MeshPacket.From) in their friend mapping to find
// the friend's Vox public key. Saves 32 bytes per packet vs. embedding the
// pubkey, at the cost of requiring a per-friend nodeNum mapping (which is
// our explicit M6.2 design — see project_meshtastic.md).

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"

	"golang.org/x/crypto/nacl/box"

	"vox_app/internal/e2e"
)

// Curve25519KeySize is the byte length of an X25519 public or secret key.
const Curve25519KeySize = e2e.KeySize

// NonceSize is the byte length of the random nonce prefix in encrypted blobs.
const NonceSize = 24

// MACSize is the byte length of the poly1305 authentication tag appended by
// nacl/box.
const MACSize = 16

// MinCipherSize is the minimum length of a valid encrypted blob — nonce only,
// with empty plaintext: nonce(24) + mac(16) = 40 bytes.
const MinCipherSize = NonceSize + MACSize

// Ed25519PublicToCurve25519 converts an Ed25519 public key (32 bytes, Edwards
// form) to its X25519 equivalent (32 bytes, Montgomery form). Returns error
// if the input is not a valid curve point.
func Ed25519PublicToCurve25519(pub ed25519.PublicKey) (*[Curve25519KeySize]byte, error) {
	return e2e.PublicToX25519(pub)
}

// Ed25519PrivateToCurve25519 converts an Ed25519 private key (64 bytes:
// seed||pubkey, as produced by crypto/ed25519) to its X25519 equivalent
// (32 bytes, clamped per RFC 7748). Shared with group/SFU encryption.
func Ed25519PrivateToCurve25519(priv ed25519.PrivateKey) (*[Curve25519KeySize]byte, error) {
	return e2e.PrivateToX25519(priv)
}

// EncryptForPeer authenticated-encrypts plaintext with the receiver's Ed25519
// public key and our Ed25519 private key. The result is `nonce || ciphertext+mac`
// suitable for putting directly into MeshPacket.Data.payload.
func EncryptForPeer(peerEdPub ed25519.PublicKey, mySec ed25519.PrivateKey, plaintext []byte) ([]byte, error) {
	peerPub, err := Ed25519PublicToCurve25519(peerEdPub)
	if err != nil {
		return nil, fmt.Errorf("convert peer pubkey: %w", err)
	}
	mySecCurve, err := Ed25519PrivateToCurve25519(mySec)
	if err != nil {
		return nil, fmt.Errorf("convert my privkey: %w", err)
	}
	var nonce [NonceSize]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	// box.Seal prepends `out` to the result, so passing nonce[:] gives us the
	// `nonce || ciphertext+mac` wire format directly.
	return box.Seal(nonce[:], plaintext, &nonce, peerPub, mySecCurve), nil
}

// DecryptFromPeer reverses EncryptForPeer. Returns the recovered plaintext or
// an error if the ciphertext is malformed, the MAC fails, or it was not
// encrypted for us by the named peer.
func DecryptFromPeer(peerEdPub ed25519.PublicKey, mySec ed25519.PrivateKey, blob []byte) ([]byte, error) {
	if len(blob) < MinCipherSize {
		return nil, fmt.Errorf("ciphertext too short: %d bytes (min %d)", len(blob), MinCipherSize)
	}
	peerPub, err := Ed25519PublicToCurve25519(peerEdPub)
	if err != nil {
		return nil, fmt.Errorf("convert peer pubkey: %w", err)
	}
	mySecCurve, err := Ed25519PrivateToCurve25519(mySec)
	if err != nil {
		return nil, fmt.Errorf("convert my privkey: %w", err)
	}
	var nonce [NonceSize]byte
	copy(nonce[:], blob[:NonceSize])
	plaintext, ok := box.Open(nil, blob[NonceSize:], &nonce, peerPub, mySecCurve)
	if !ok {
		return nil, errors.New("decrypt failed: bad MAC, wrong sender, or corrupted ciphertext")
	}
	return plaintext, nil
}
