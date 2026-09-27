package storage

import (
	"bytes"
	"crypto/rand"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

// makeDB создаёт базу под ключом key; pub != nil — с профилем.
func makeDB(t *testing.T, path string, key, pub []byte) {
	t.Helper()
	s, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if pub != nil {
		if _, err := s.DB().Exec("INSERT INTO identity (id, private_key, public_key) VALUES (1, ?, ?)", []byte("priv"), pub); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}
}

func pubOf(t *testing.T, s *Storage) []byte {
	t.Helper()
	pub, err := identityPub(s.DB())
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func openStartup(t *testing.T, dbPath string, key []byte) (*Storage, bool) {
	t.Helper()
	s, rec, err := OpenAtStartup(quiet, dbPath, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, rec
}

func TestWrongKeyFailsFast(t *testing.T) {
	db := filepath.Join(t.TempDir(), "vox.db")
	makeDB(t, db, testKey(t), nil)
	start := time.Now()
	_, err := Open(db, testKey(t))
	if !IsWrongKey(err) {
		t.Fatalf("ожидали ошибку ключа, получили %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("чужой ключ проверялся %s — повторы не нужны", d)
	}
}

// Ключ утерян, .bak нет: база уезжает в .bak, экран восстановления.
func TestStartupKeyLost(t *testing.T) {
	db := filepath.Join(t.TempDir(), "vox.db")
	makeDB(t, db, testKey(t), []byte("A"))
	s, rec := openStartup(t, db, testKey(t))
	if !rec || pubOf(t, s) != nil || !fileExists(db+".bak") {
		t.Fatalf("rec=%v bak=%v", rec, fileExists(db+".bak"))
	}
}

// Ключ утерян, а .bak уже есть (другим ключом): рабочая база не удаляется.
func TestStartupKeyLostKeepsBoth(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "vox.db")
	makeDB(t, db, testKey(t), []byte("A"))
	makeDB(t, db+".bak", testKey(t), []byte("B"))
	_, rec := openStartup(t, db, testKey(t))
	if !rec || !fileExists(db+".bak") {
		t.Fatalf("rec=%v, .bak на месте=%v", rec, fileExists(db+".bak"))
	}
	m, _ := filepath.Glob(filepath.Join(dir, "vox.db.bak-*"))
	if len(m) != 1 {
		t.Fatalf("отложенная копия рабочей базы: %v", m)
	}
}

// Ключ восстановлен из бэкапа: .bak открывается и становится рабочей базой.
func TestStartupRestoresBakOnKeyMismatch(t *testing.T) {
	db := filepath.Join(t.TempDir(), "vox.db")
	key := testKey(t)
	makeDB(t, db, testKey(t), nil)
	makeDB(t, db+".bak", key, []byte("A"))
	s, rec := openStartup(t, db, key)
	if rec || !bytes.Equal(pubOf(t, s), []byte("A")) || fileExists(db+".bak") {
		t.Fatalf("rec=%v pub=%q", rec, pubOf(t, s))
	}
}

// Пустая база и .bak под текущим ключом (старый сбой не из-за ключа):
// восстанавливаем, а не показываем экран «ключ утерян».
func TestStartupRestoresBakIntoEmptyDB(t *testing.T) {
	db := filepath.Join(t.TempDir(), "vox.db")
	key := testKey(t)
	makeDB(t, db, key, nil)
	makeDB(t, db+".bak", key, []byte("A"))
	s, rec := openStartup(t, db, key)
	if rec || !bytes.Equal(pubOf(t, s), []byte("A")) || fileExists(db+".bak") {
		t.Fatalf("rec=%v pub=%q", rec, pubOf(t, s))
	}
}

// Пустая база и .bak чужим ключом — экран восстановления, .bak цела.
func TestStartupEmptyDBForeignBak(t *testing.T) {
	db := filepath.Join(t.TempDir(), "vox.db")
	key := testKey(t)
	makeDB(t, db, key, nil)
	makeDB(t, db+".bak", testKey(t), []byte("A"))
	_, rec := openStartup(t, db, key)
	if !rec || !fileExists(db+".bak") {
		t.Fatalf("rec=%v", rec)
	}
}

// Тот же профиль в базе и в .bak — копия уже перенесена, удаляем.
func TestStartupRemovesMergedBak(t *testing.T) {
	db := filepath.Join(t.TempDir(), "vox.db")
	key := testKey(t)
	makeDB(t, db, key, []byte("A"))
	makeDB(t, db+".bak", key, []byte("A"))
	_, rec := openStartup(t, db, key)
	if rec || fileExists(db+".bak") {
		t.Fatalf("rec=%v bak=%v", rec, fileExists(db+".bak"))
	}
}

// Другой профиль в .bak (тем же ключом или чужим) — не трогаем.
func TestStartupKeepsOtherProfileBak(t *testing.T) {
	for name, sameKey := range map[string]bool{"тот же ключ": true, "чужой ключ": false} {
		t.Run(name, func(t *testing.T) {
			db := filepath.Join(t.TempDir(), "vox.db")
			key := testKey(t)
			bakKey := testKey(t)
			if sameKey {
				bakKey = key
			}
			makeDB(t, db, key, []byte("A"))
			makeDB(t, db+".bak", bakKey, []byte("B"))
			s, rec := openStartup(t, db, key)
			if rec || !fileExists(db+".bak") || !bytes.Equal(pubOf(t, s), []byte("A")) {
				t.Fatalf("rec=%v bak=%v", rec, fileExists(db+".bak"))
			}
		})
	}
}

// Ошибка не из-за ключа — файлы не трогаем, ошибка наружу.
func TestStartupNonKeyErrorKeepsFiles(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "vox.db")
	// Путь-каталог: Open падает не с «не база».
	if err := os.Mkdir(db, 0700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := OpenAtStartup(quiet, db, testKey(t)); err == nil || IsWrongKey(err) {
		t.Fatalf("ожидали ошибку не ключа, получили %v", err)
	}
	if fileExists(db + ".bak") {
		t.Fatal("база уехала в .bak из-за ошибки не ключа")
	}
}
