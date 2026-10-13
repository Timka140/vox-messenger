package logstore

import (
	"encoding/base64"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T, opts Options) (*Store, string) {
	t.Helper()
	base := t.TempDir()
	s := &Store{
		baseDir: base,
		opts:    opts.withDefaults(),
		chunks:  make(map[string]*chunkBuffer),
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return s, base
}

func putFile(t *testing.T, base, source, name string, size int, mod time.Time) string {
	t.Helper()
	dir := filepath.Join(base, source)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(strings.Repeat("x", size)), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
	return p
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func TestPruneMaxAgeAndEmptyDirs(t *testing.T) {
	s, base := testStore(t, Options{MaxAge: 24 * time.Hour})
	now := time.Now()
	old := putFile(t, base, "anon-0000000000000001", "2026-01-01T00-00-00.jsonl", 10, now.Add(-48*time.Hour))
	fresh := putFile(t, base, "anon-0000000000000002", "2026-01-02T00-00-00.jsonl", 10, now)
	s.Prune()
	if exists(old) || exists(filepath.Dir(old)) {
		t.Fatal("старый файл и пустой каталог должны быть удалены")
	}
	if !exists(fresh) {
		t.Fatal("свежий файл удалён")
	}
}

func TestPruneKeepPerSource(t *testing.T) {
	s, base := testStore(t, Options{KeepPerSource: 2})
	now := time.Now()
	var paths []string
	for _, n := range []string{"2026-01-01T00-00-00.jsonl", "2026-01-02T00-00-00.jsonl", "2026-01-03T00-00-00.jsonl"} {
		paths = append(paths, putFile(t, base, "anon-0000000000000001", n, 10, now))
	}
	s.Prune()
	if exists(paths[0]) || !exists(paths[1]) || !exists(paths[2]) {
		t.Fatal("должны остаться два самых новых файла")
	}
}

func TestPruneMaxTotalOldestAcrossSources(t *testing.T) {
	s, base := testStore(t, Options{MaxTotal: 250})
	now := time.Now()
	a1 := putFile(t, base, "anon-000000000000000a", "2026-01-01T00-00-00.jsonl", 100, now.Add(-3*time.Hour))
	b1 := putFile(t, base, "anon-000000000000000b", "2026-01-01T00-00-00.jsonl", 100, now.Add(-2*time.Hour))
	a2 := putFile(t, base, "anon-000000000000000a", "2026-01-02T00-00-00.jsonl", 100, now.Add(-1*time.Hour))
	s.Prune()
	if exists(a1) {
		t.Fatal("самый старый файл во всём хранилище должен уйти первым")
	}
	if !exists(b1) || !exists(a2) {
		t.Fatal("удалено больше, чем нужно для предела")
	}

	s.opts.MaxTotal = 50
	s.Prune()
	if exists(b1) || exists(a2) || exists(filepath.Dir(b1)) {
		t.Fatal("при пределе меньше файла всё удаляется, пустые каталоги тоже")
	}
}

func TestSaveChunkLimits(t *testing.T) {
	s, base := testStore(t, Options{})
	enc := base64.StdEncoding.EncodeToString([]byte(`{"msg":"x"}`))
	src := "anon-0000000000000001"

	if s.SaveChunk(src, 0, maxChunkTotal+1, enc) {
		t.Fatal("chunkTotal сверх предела принят")
	}
	if s.SaveChunk(src, 5, 3, enc) || s.SaveChunk(src, -1, 3, enc) {
		t.Fatal("неверный номер чанка принят")
	}
	big := base64.StdEncoding.EncodeToString(make([]byte, MaxPushBytes+1))
	if s.SaveChunk(src, 0, 1, big) {
		t.Fatal("лог больше предела принят")
	}

	// Сумма частей больше предела — буфер сбрасывается.
	half := base64.StdEncoding.EncodeToString(make([]byte, MaxPushBytes/2+1))
	s.SaveChunk(src, 0, 3, half)
	s.SaveChunk(src, 1, 3, half)
	if _, ok := s.chunks[src]; ok {
		t.Fatal("буфер сверх предела не сброшен")
	}
	if entries, _ := os.ReadDir(base); len(entries) != 0 {
		t.Fatal("отброшенные пуши попали на диск")
	}

	if !s.SaveChunk(src, 0, 1, enc) {
		t.Fatal("нормальный пуш не записан")
	}
}

func TestValidClientSource(t *testing.T) {
	for src, want := range map[string]bool{
		"anon-0123456789abcdef":  true,
		"anon-0123456789ABCDEF":  false,
		"anon-0123":              false,
		"Alice#aa":               false,
		"signal-server":          false,
		"anon-0123456789abcdef/": false,
	} {
		if ValidClientSource(src) != want {
			t.Fatalf("%q: want %v", src, want)
		}
	}
}
