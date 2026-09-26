package services

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"vox_app/internal/storage"
)

func openPreviewTestDB(t *testing.T) *storage.Storage {
	t.Helper()
	store, err := storage.Open(filepath.Join(t.TempDir(), "preview.db"), bytes.Repeat([]byte{5}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestLastDMMessagePreview(t *testing.T) {
	store := openPreviewTestDB(t)
	db := store.DB()
	const me, peer = "me", "peer1"
	now := time.Now().Unix()
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec("INSERT INTO contacts (peer_id, nickname, status) VALUES (?, 'Алина', 'accepted')", peer)

	// Фото без подписи, альбом из трёх — раньше превью пропадало (content == "").
	mustExec("INSERT INTO messages (id, peer_id, sender_id, content, type, read, created_at) VALUES ('m1', ?, ?, '', 'text', 1, ?)", peer, me, now-10)
	for i := 0; i < 3; i++ {
		mustExec("INSERT INTO message_attachments (message_id, attachment_id, ord, name, kind) VALUES ('m1', ?, ?, ?, 'photo')", "a"+string(rune('0'+i)), i, "p.jpg")
	}
	lm := lastDMMessage(db, peer, me, false)
	if lm == nil || lm.Attachments != 3 || lm.AttachmentKind != "photo" || !lm.Outbound {
		t.Fatalf("фото без подписи: %+v", lm)
	}

	// Голосовое от собеседника — с длительностью, непрочитанное.
	mustExec("INSERT INTO messages (id, peer_id, sender_id, content, type, read, created_at, duration) VALUES ('m2', ?, ?, '🎤', 'voice', 0, ?, 42000)", peer, peer, now-5)
	// И наше сообщение после него.
	mustExec("INSERT INTO messages (id, peer_id, sender_id, content, type, read, created_at) VALUES ('m3', ?, ?, 'ок', 'text', 1, ?)", peer, me, now)

	if lm := lastDMMessage(db, peer, me, false); lm == nil || lm.Content != "ок" || !lm.Outbound {
		t.Fatalf("последнее любое: %+v", lm)
	}
	lm = lastDMMessage(db, peer, me, true)
	if lm == nil || lm.MsgType != "voice" || lm.Duration != 42000 || lm.Outbound {
		t.Fatalf("последнее входящее (голосовое): %+v", lm)
	}
	if lastDMMessage(db, "nobody", me, false) != nil {
		t.Fatal("превью у пустой переписки")
	}
}

func TestLastGroupMessagePreview(t *testing.T) {
	store := openPreviewTestDB(t)
	db := store.DB()
	const me, other, gid = "me", "other1", "g-1"
	now := time.Now().Unix()
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec("INSERT INTO groups (group_id, name, owner_id, type, created_at) VALUES (?, 'Тест', ?, 0, ?)", gid, me, now)
	// Не друг: ник только в peer_profiles.
	mustExec("INSERT INTO peer_profiles (peer_id, nickname) VALUES (?, 'CHORT')", other)

	if lastGroupMessage(db, gid, me) != nil {
		t.Fatal("превью у пустой группы")
	}
	mustExec("INSERT INTO group_messages (id, group_id, sender_id, content, type, created_at) VALUES ('g1', ?, ?, 'Сделай шрифт покрупнее', 'text', ?)", gid, other, now)
	lm := lastGroupMessage(db, gid, me)
	if lm == nil || lm.SenderName != "CHORT" || lm.Outbound || lm.Content != "Сделай шрифт покрупнее" {
		t.Fatalf("групповое от другого: %+v", lm)
	}

	mustExec("INSERT INTO group_messages (id, group_id, sender_id, content, type, created_at) VALUES ('g2', ?, ?, '', 'text', ?)", gid, me, now+1)
	mustExec("INSERT INTO message_attachments (group_msg_id, attachment_id, ord, name, kind) VALUES ('g2', 'f1', 0, 'Референсы.pdf', 'file')")
	lm = lastGroupMessage(db, gid, me)
	if lm == nil || !lm.Outbound || lm.SenderName != "" || lm.Attachments != 1 || lm.AttachmentName != "Референсы.pdf" {
		t.Fatalf("свой файл в группе: %+v", lm)
	}
}

func TestCallRecordRead(t *testing.T) {
	cases := []struct {
		outcome  string
		outbound bool
		want     int
	}{
		{"missed", false, 0},
		{"missed", true, 1},
		{"no_answer", true, 1},
		{"declined", false, 1},
		{"answered", false, 1},
	}
	for _, c := range cases {
		if got := callRecordRead(c.outcome, c.outbound); got != c.want {
			t.Errorf("%s outbound=%v: read=%d, ждали %d", c.outcome, c.outbound, got, c.want)
		}
	}
}
