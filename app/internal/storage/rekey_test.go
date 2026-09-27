package storage

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestApplyPendingRekey(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "vox.db"), "old db")
	writeFile(t, filepath.Join(dir, "vox.db-wal"), "old wal")
	writeFile(t, filepath.Join(dir, "vox.key"), "old key")

	if r, err := ApplyPendingRekey(dir); err != nil || r != SwapNone {
		t.Fatalf("без новых файлов: %v %v", r, err)
	}

	writeFile(t, filepath.Join(dir, PendingDBName), "new db")
	writeFile(t, filepath.Join(dir, PendingKeyName), "new key")
	if r, err := ApplyPendingRekey(dir); err != nil || r != SwapApplied {
		t.Fatalf("подмена: %v %v", r, err)
	}
	if readFile(t, filepath.Join(dir, "vox.db")) != "new db" || readFile(t, filepath.Join(dir, "vox.key")) != "new key" {
		t.Fatal("файлы не подменились")
	}
	for _, p := range []string{"vox.db-wal", PendingDBName, PendingKeyName} {
		if fileExists(filepath.Join(dir, p)) {
			t.Fatalf("%s остался", p)
		}
	}
}

func TestApplyPendingRekeyPartial(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "vox.db"), "old db")
	writeFile(t, filepath.Join(dir, "vox.key"), "old key")
	writeFile(t, filepath.Join(dir, PendingDBName), "new db")

	if r, err := ApplyPendingRekey(dir); err != nil || r != SwapPartial {
		t.Fatalf("половина: %v %v", r, err)
	}
	if readFile(t, filepath.Join(dir, "vox.db")) != "old db" || fileExists(filepath.Join(dir, PendingDBName)) {
		t.Fatal("старые файлы тронуты или обрывок не удалён")
	}
}

// Обрыв между переносом базы и ключа: одинокий vox.key.new, который открывает
// vox.db, ставится на место; чужой — удаляется.
func TestApplyPendingRekeyResume(t *testing.T) {
	dir := t.TempDir()
	newKey := bytes.Repeat([]byte{7}, 32)
	s, err := Open(filepath.Join(dir, "vox.db"), newKey)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	writeFile(t, filepath.Join(dir, "vox.key"), hex.EncodeToString(bytes.Repeat([]byte{1}, 32)))
	writeFile(t, filepath.Join(dir, PendingKeyName), hex.EncodeToString(newKey))

	if r, err := ApplyPendingRekey(dir); err != nil || r != SwapResumed {
		t.Fatalf("довод: %v %v", r, err)
	}
	if readFile(t, filepath.Join(dir, "vox.key")) != hex.EncodeToString(newKey) || fileExists(filepath.Join(dir, PendingKeyName)) {
		t.Fatal("ключ не доставлен")
	}

	writeFile(t, filepath.Join(dir, PendingKeyName), hex.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	if r, err := ApplyPendingRekey(dir); err != nil || r != SwapPartial {
		t.Fatalf("чужой ключ: %v %v", r, err)
	}
	if readFile(t, filepath.Join(dir, "vox.key")) != hex.EncodeToString(newKey) || fileExists(filepath.Join(dir, PendingKeyName)) {
		t.Fatal("чужой ключ тронул vox.key или остался")
	}
}

func TestKeyOpens(t *testing.T) {
	dir := t.TempDir()
	keyA := bytes.Repeat([]byte{1}, 32)
	keyB := bytes.Repeat([]byte{2}, 32)
	p := filepath.Join(dir, "vox.db")
	s, err := Open(p, keyA)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if !KeyOpens(p, keyA) {
		t.Fatal("верный ключ не подошёл")
	}
	if KeyOpens(p, keyB) {
		t.Fatal("неверный ключ подошёл")
	}
	if KeyOpens(filepath.Join(dir, "nope.db"), keyA) {
		t.Fatal("несуществующий файл")
	}
}
