package meshtastic

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"
)

// TestAnnouncementRoundTrip — encode then decode at the same time gives
// back the original pubkey + nodeNum.
func TestAnnouncementRoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	now := time.Now()
	const nodeNum uint32 = 0xacaaba94

	blob, err := EncodeAnnouncement(priv, nodeNum, now)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(blob) != AnnouncementSize {
		t.Errorf("wire size: got %d, want %d", len(blob), AnnouncementSize)
	}
	ann, err := DecodeAnnouncement(blob, now)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ann.NodeNum != nodeNum {
		t.Errorf("nodeNum: got %#x, want %#x", ann.NodeNum, nodeNum)
	}
	if !ann.Pubkey.Equal(pub) {
		t.Error("pubkey mismatch")
	}
	if ann.Timestamp != now.UnixMilli() {
		t.Errorf("timestamp: got %d, want %d", ann.Timestamp, now.UnixMilli())
	}
}

// TestAnnouncementForgedSignature — Mallory cannot send an announcement
// claiming to be Alice unless she has Alice's private key.
func TestAnnouncementForgedSignature(t *testing.T) {
	alicePub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, malloryPriv, _ := ed25519.GenerateKey(rand.Reader)

	// Mallory crafts a packet with Alice's pubkey but signs with her own key.
	now := time.Now()
	blob, _ := EncodeAnnouncement(malloryPriv, 0xdeadbeef, now)
	// Replace Mallory's pubkey with Alice's in the wire bytes.
	copy(blob[1:1+ed25519.PublicKeySize], alicePub)

	if _, err := DecodeAnnouncement(blob, now); err == nil {
		t.Fatal("forged announcement accepted — security broken")
	}
}

// TestAnnouncementStaleTimestamp — old replays are rejected even if the
// signature is valid (still Alice's, just from a week ago).
func TestAnnouncementStaleTimestamp(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)

	weekAgo := time.Now().Add(-7 * 24 * time.Hour)
	blob, _ := EncodeAnnouncement(priv, 0x1234, weekAgo)

	if _, err := DecodeAnnouncement(blob, time.Now()); err == nil {
		t.Fatal("week-old announcement accepted — replay protection broken")
	}
}

// TestAnnouncementFutureTimestamp — clock-drift attack: announcer claims
// future time. Reject too.
func TestAnnouncementFutureTimestamp(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)

	wayFuture := time.Now().Add(2 * time.Hour)
	blob, _ := EncodeAnnouncement(priv, 0x1234, wayFuture)

	if _, err := DecodeAnnouncement(blob, time.Now()); err == nil {
		t.Fatal("far-future announcement accepted")
	}
}

// TestAnnouncementWithinSkew — clock drift up to ±1h is tolerated.
func TestAnnouncementWithinSkew(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)

	thirtyMinAgo := time.Now().Add(-30 * time.Minute)
	blob, _ := EncodeAnnouncement(priv, 0x1234, thirtyMinAgo)

	if _, err := DecodeAnnouncement(blob, time.Now()); err != nil {
		t.Fatalf("30-min-old announcement rejected: %v", err)
	}
}

// TestAnnouncementWrongSize — packets shorter or longer than AnnouncementSize
// are rejected without invoking ed25519.Verify on garbage.
func TestAnnouncementWrongSize(t *testing.T) {
	for _, n := range []int{0, 1, 50, AnnouncementSize - 1, AnnouncementSize + 1} {
		if _, err := DecodeAnnouncement(make([]byte, n), time.Now()); err == nil {
			t.Errorf("%d-byte input accepted", n)
		}
	}
}

// TestAnnouncementWrongTypeByte — first byte must be AnnouncementType (0x02);
// other type bytes belong to other Vox packet kinds (encrypted DMs use 0x01).
func TestAnnouncementWrongTypeByte(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	blob, _ := EncodeAnnouncement(priv, 0x1234, time.Now())
	blob[0] = 0x99
	if _, err := DecodeAnnouncement(blob, time.Now()); err == nil {
		t.Error("wrong type byte accepted")
	}
}
