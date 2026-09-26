package meshtastic

// Auto-discovery announcements for Vox-over-mesh.
//
// The friction in M6.2 is that every user must manually type each friend's
// Meshtastic node ID into the friend profile. People don't memorize hex IDs.
// This file defines a tiny signed broadcast packet that lets clients
// announce "I'm Vox-pubkey-X, my LoRa node is Y" so receivers with X in
// their friends list can pre-fill the mapping automatically.
//
// Wire format:
//
//   | 0x02 | pubkey(32) | nodeNum(4 BE) | timestamp(8 BE unix-ms) | sig(64) |
//   = 109 bytes
//
// The signature is over (pubkey || nodeNum || timestamp), allowing the
// receiver to confirm: (a) the announcer actually controls that pubkey
// (signed with its private key), and (b) the announcement is recent (reject
// if timestamp ±1h from now — replay protection).
//
// Privacy: the announcement reveals "user with pubkey X is on the mesh
// using node Y" to anyone able to decrypt the channel AES. On a public
// LongFast channel this is everyone in radio range. For threat models
// where this is acceptable (broad reach > anonymity), enable the feature;
// otherwise stick to manual mapping. Default: opt-in.

import (
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// AnnouncementType marks a discovery announcement packet.
const AnnouncementType byte = 0x02

// AnnouncementSize is the fixed wire byte length of an announcement.
const AnnouncementSize = 1 + ed25519.PublicKeySize + 4 + 8 + ed25519.SignatureSize // 109

// AnnouncementMaxSkewMs is the largest time difference between an
// announcement's claimed timestamp and our local clock that we still
// accept. Anything older or further in the future is treated as a replay.
const AnnouncementMaxSkewMs int64 = 60 * 60 * 1000 // ±1 hour

// Announcement holds the parsed contents of one discovery packet.
type Announcement struct {
	Pubkey    ed25519.PublicKey
	NodeNum   uint32
	Timestamp int64 // unix milliseconds
}

// EncodeAnnouncement signs (pubkey || nodeNum || timestamp) with priv and
// returns the wire bytes ready for a single Meshtastic broadcast packet.
//
// The matching public key is included verbatim so receivers don't need to
// know it in advance — that's exactly the lookup we're trying to avoid.
func EncodeAnnouncement(priv ed25519.PrivateKey, nodeNum uint32, now time.Time) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("priv key wrong size: %d", len(priv))
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("could not derive public key from private key")
	}
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("derived pubkey wrong size: %d", len(pub))
	}
	tsMs := now.UnixMilli()

	// Build the signed payload (without the type byte and signature).
	signed := make([]byte, 0, ed25519.PublicKeySize+4+8)
	signed = append(signed, pub...)
	var nbuf [4]byte
	binary.BigEndian.PutUint32(nbuf[:], nodeNum)
	signed = append(signed, nbuf[:]...)
	var tbuf [8]byte
	binary.BigEndian.PutUint64(tbuf[:], uint64(tsMs))
	signed = append(signed, tbuf[:]...)

	sig := ed25519.Sign(priv, signed)

	out := make([]byte, 0, AnnouncementSize)
	out = append(out, AnnouncementType)
	out = append(out, signed...)
	out = append(out, sig...)
	return out, nil
}

// DecodeAnnouncement parses one wire packet, verifies the embedded
// signature, and rejects stale timestamps. Returns the parsed announcement
// or a specific error.
func DecodeAnnouncement(blob []byte, now time.Time) (Announcement, error) {
	if len(blob) != AnnouncementSize {
		return Announcement{}, fmt.Errorf("wrong size: got %d, want %d", len(blob), AnnouncementSize)
	}
	if blob[0] != AnnouncementType {
		return Announcement{}, fmt.Errorf("wrong type byte: %#x", blob[0])
	}
	pub := ed25519.PublicKey(blob[1 : 1+ed25519.PublicKeySize])
	nodeNum := binary.BigEndian.Uint32(blob[1+ed25519.PublicKeySize : 1+ed25519.PublicKeySize+4])
	tsMs := int64(binary.BigEndian.Uint64(blob[1+ed25519.PublicKeySize+4 : 1+ed25519.PublicKeySize+4+8]))
	signed := blob[1 : 1+ed25519.PublicKeySize+4+8]
	sig := blob[1+ed25519.PublicKeySize+4+8:]

	if !ed25519.Verify(pub, signed, sig) {
		return Announcement{}, errors.New("signature invalid")
	}
	skew := now.UnixMilli() - tsMs
	if skew < 0 {
		skew = -skew
	}
	if skew > AnnouncementMaxSkewMs {
		return Announcement{}, fmt.Errorf("timestamp skew %dms exceeds max %dms (replay or clock drift)",
			skew, AnnouncementMaxSkewMs)
	}
	return Announcement{
		Pubkey:    pub,
		NodeNum:   nodeNum,
		Timestamp: tsMs,
	}, nil
}
