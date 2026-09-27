package services

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"vox_app/internal/identity"
	"vox_app/internal/session"
	"vox_app/internal/storage"
)

// Профиль в базе при пустой сессии (экран создания после сбоя LoadIdentity):
// фраза не должна перезаписать его ключ.
func TestApplyMnemonicKeepsExistingProfile(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.Open(filepath.Join(dir, "vox.db"), bytes.Repeat([]byte{5}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := store.DB().Exec(
		"INSERT INTO identity (id, private_key, public_key) VALUES (1, ?, ?)",
		[]byte(priv), []byte(pub),
	); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := NewIdentityService(log, session.New(), store, dir, nil)
	e, err := identity.NewEntropy()
	if err != nil {
		t.Fatal(err)
	}
	phrase, err := identity.EntropyToMnemonic(e, identity.LangEN)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.ApplyMnemonic(phrase, true); err == nil || err.Error() != mnemonicErrHasProfile {
		t.Fatalf("ожидали %s, получили %v", mnemonicErrHasProfile, err)
	}
	var got []byte
	if err := store.DB().QueryRow("SELECT private_key FROM identity WHERE id = 1").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, priv) {
		t.Fatal("ключ профиля перезаписан")
	}
}

// Верная фраза на экране «ключ утерян»: история из .bak доезжает до vox.db
// через подмену при старте, а не через повторы Open с чужим ключом.
func TestApplyMnemonicRestoresBackupViaSwap(t *testing.T) {
	dir := t.TempDir()
	e, err := identity.NewEntropy()
	if err != nil {
		t.Fatal(err)
	}
	phrase, err := identity.EntropyToMnemonic(e, identity.LangRU)
	if err != nil {
		t.Fatal(err)
	}
	priv, pub, dbKey, err := identity.DeriveKeys(e)
	if err != nil {
		t.Fatal(err)
	}

	bakPath := filepath.Join(dir, "vox.db.bak")
	bak, err := storage.Open(bakPath, dbKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bak.DB().Exec(
		"INSERT INTO identity (id, private_key, public_key) VALUES (1, ?, ?)",
		[]byte(priv), []byte(pub),
	); err != nil {
		t.Fatal(err)
	}
	bak.Close()

	store, err := storage.Open(filepath.Join(dir, "vox.db"), bytes.Repeat([]byte{6}, 32))
	if err != nil {
		t.Fatal(err)
	}
	relaunched := false
	orig := relaunchSelf
	relaunchSelf = func(*slog.Logger) error { relaunched = true; return nil }
	t.Cleanup(func() { relaunchSelf = orig })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := NewIdentityService(log, session.New(), store, dir, nil)
	if _, err := s.ApplyMnemonic(phrase, false); err != nil {
		t.Fatal(err)
	}
	// Повтор (двойная отправка) не трогает уже готовые файлы подмены.
	if _, err := s.ApplyMnemonic(phrase, false); err == nil || err.Error() != mnemonicErrBusy {
		t.Fatalf("повтор: ожидали %s, получили %v", mnemonicErrBusy, err)
	}
	store.Close()
	if !relaunched {
		t.Fatal("перезапуска не было")
	}

	res, err := storage.ApplyPendingRekey(dir)
	if err != nil || res != storage.SwapApplied {
		t.Fatalf("подмена: %v, %v", res, err)
	}
	if !storage.KeyOpens(filepath.Join(dir, "vox.db"), dbKey) {
		t.Fatal("vox.db не открывается ключом из фразы")
	}
	key, err := os.ReadFile(filepath.Join(dir, "vox.key"))
	if err != nil || len(key) != 64 {
		t.Fatalf("vox.key: %q, %v", key, err)
	}
	restored, err := storage.Open(filepath.Join(dir, "vox.db"), dbKey)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	var got []byte
	if err := restored.DB().QueryRow("SELECT private_key FROM identity WHERE id = 1").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, priv) {
		t.Fatal("профиль из .bak не восстановлен")
	}
}
