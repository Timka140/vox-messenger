package storage

import (
	"bytes"
	"path/filepath"
	"testing"
)

// ExportEncrypted: данные переезжают в новый файл, открываются только новым ключом.
func TestExportEncrypted(t *testing.T) {
	dir := t.TempDir()
	keyA := bytes.Repeat([]byte{0xA1}, 32)
	keyB := bytes.Repeat([]byte{0xB2}, 32)

	src, err := Open(filepath.Join(dir, "vox.db"), keyA)
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	if _, err := src.DB().Exec("INSERT OR REPLACE INTO settings (key, value) VALUES ('probe', 'перешифровано')"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	dst := filepath.Join(dir, "vox.db.new")
	if err := src.ExportEncrypted(dst, keyB); err != nil {
		t.Fatalf("export: %v", err)
	}
	// Повторный экспорт перезаписывает файл.
	if err := src.ExportEncrypted(dst, keyB); err != nil {
		t.Fatalf("export again: %v", err)
	}
	src.Close()

	got, err := Open(dst, keyB)
	if err != nil {
		t.Fatalf("open B: %v", err)
	}
	var v string
	if err := got.DB().QueryRow("SELECT value FROM settings WHERE key = 'probe'").Scan(&v); err != nil || v != "перешифровано" {
		t.Fatalf("read: %q %v", v, err)
	}
	got.Close()
}

func TestExportEncryptedWrongKeyFails(t *testing.T) {
	if testing.Short() {
		t.Skip("Open с неверным ключом ждёт ~15 с повторов")
	}
	dir := t.TempDir()
	keyA := bytes.Repeat([]byte{0xA1}, 32)
	keyB := bytes.Repeat([]byte{0xB2}, 32)
	src, err := Open(filepath.Join(dir, "vox.db"), keyA)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "vox.db.new")
	if err := src.ExportEncrypted(dst, keyB); err != nil {
		t.Fatal(err)
	}
	src.Close()
	if s, err := Open(dst, keyA); err == nil {
		s.Close()
		t.Fatal("новый файл открылся старым ключом")
	}
}
