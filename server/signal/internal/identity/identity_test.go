package identity

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreate_Generates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vox-signal.key")

	id, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if len(id.Public) != ed25519.PublicKeySize {
		t.Fatalf("pub size = %d, want %d", len(id.Public), ed25519.PublicKeySize)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("key file not created: %v", err)
	}
}

func TestLoadOrCreate_LoadsExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vox-signal.key")

	id1, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("first LoadOrCreate: %v", err)
	}
	id2, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("second LoadOrCreate: %v", err)
	}
	if id1.PubKeyHex() != id2.PubKeyHex() {
		t.Fatalf("pubkey mismatch on reload: %s vs %s", id1.PubKeyHex(), id2.PubKeyHex())
	}
}

func TestLoadOrCreate_RejectsCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vox-signal.key")
	if err := os.WriteFile(path, []byte("not-hex\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(path); err == nil {
		t.Fatal("expected error on corrupt seed, got nil")
	}
}

func TestSignVerifyRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vox-signal.key")
	id, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("client_challenge_bytes")
	sig := ed25519.Sign(id.Private, msg)
	if !ed25519.Verify(id.Public, msg, sig) {
		t.Fatal("signature does not verify")
	}
}
