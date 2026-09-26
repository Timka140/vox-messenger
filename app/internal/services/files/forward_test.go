package files

import (
	"io"
	"log/slog"
	"testing"

	"vox_app/internal/session"
	"vox_app/internal/storage"
)

// loadFilesBySha — пересылка отдельных файлов из «Медиа» (фаза 7e): имя и тип
// из самой ранней строки message_files, порядок как в запросе, без повторов.
func TestLoadFilesBySha(t *testing.T) {
	store, err := storage.OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	db := store.DB()
	svc := New(slog.New(slog.NewTextHandler(io.Discard, nil)), session.New(), db)

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []struct {
		sha, mime string
		size      int64
	}{{"aaa", "image/png", 100}, {"bbb", "application/pdf", 200}} {
		exec(`INSERT INTO files (sha, size, chunk_size, chunk_count, mime, created_at) VALUES (?, ?, 65536, 1, ?, 0)`, f.sha, f.size, f.mime)
		exec(`INSERT INTO file_chunks (file_sha, chunk_idx, chunk_sha) VALUES (?, 0, ?)`, f.sha, f.sha+"-c0")
	}
	exec(`INSERT INTO message_files (message_id, file_sha, ord, name, kind) VALUES ('m1', 'aaa', 0, 'first.png', 'photo')`)
	exec(`INSERT INTO message_files (message_id, file_sha, ord, name, kind) VALUES ('m1', 'bbb', 1, 'doc.pdf', 'file')`)
	exec(`INSERT INTO message_files (message_id, file_sha, ord, name, kind) VALUES ('m2', 'aaa', 0, 'renamed.png', 'photo')`)

	items, mans, err := svc.loadFilesBySha([]string{"bbb", "aaa", "bbb"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || len(mans) != 2 {
		t.Fatalf("items=%d manifests=%d, want 2 без повтора", len(items), len(mans))
	}
	if mans[0].Sha != "bbb" || mans[0].Name != "doc.pdf" || mans[0].Kind != "file" || mans[0].Size != 200 {
		t.Errorf("первый = %+v", mans[0])
	}
	if mans[1].Sha != "aaa" || mans[1].Name != "first.png" || items[1].Kind != "photo" || mans[1].Mime != "image/png" {
		t.Errorf("второй = %+v (имя — из самой ранней строки)", mans[1])
	}
	if len(mans[1].Chunks) != 1 {
		t.Errorf("chunks = %v", mans[1].Chunks)
	}

	if _, _, err := svc.loadFilesBySha([]string{"zzz"}); err == nil {
		t.Error("неизвестный sha — ожидалась ошибка")
	}
	if _, err := svc.ForwardFileShas([]string{"aaa"}, "", "", ""); err == nil {
		t.Error("без получателя — ожидалась ошибка")
	}
}
