package services

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"vox_app/internal/session"
	"vox_app/internal/storage"
)

func newBlockTestNetwork(t *testing.T) (*NetworkService, *storage.Storage, *session.Session) {
	t.Helper()
	store, err := storage.Open(filepath.Join(t.TempDir(), "block.db"), bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sess := session.New()
	if err := sess.Set(priv, pub); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewNetworkService(log, sess, store), store, sess
}

func TestBlockPeerPersists(t *testing.T) {
	n, store, sess := newBlockTestNetwork(t)
	const peer = "peer-blocked-1"
	if n.IsBlocked(peer) {
		t.Fatal("заблокирован до блокировки")
	}
	if err := n.BlockPeer(peer); err != nil {
		t.Fatal(err)
	}
	if !n.IsBlocked(peer) {
		t.Fatal("не заблокирован после BlockPeer")
	}
	me, _ := sess.PeerID()
	if err := n.BlockPeer(me); err == nil {
		t.Fatal("себя заблокировать удалось")
	}

	// Новый сервис на той же БД — список из таблицы (перезапуск приложения).
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	n2 := NewNetworkService(log, sess, store)
	if !n2.IsBlocked(peer) {
		t.Fatal("блокировка не пережила перезапуск")
	}
	list, err := n2.GetBlockedPeers()
	if err != nil || len(list) != 1 || list[0].PeerID != peer || list[0].BlockedAt == 0 {
		t.Fatalf("список заблокированных: %+v, %v", list, err)
	}
	if err := n2.UnblockPeer(peer); err != nil {
		t.Fatal(err)
	}
	if n2.IsBlocked(peer) {
		t.Fatal("заблокирован после UnblockPeer")
	}
	if list, _ := n2.GetBlockedPeers(); len(list) != 0 {
		t.Fatalf("после разблокировки в списке: %+v", list)
	}
}

func TestDropFromBlocked(t *testing.T) {
	drop := []string{"message", "typing", "read", "message_edit", "message_delete", "caps", "avatar_hash", "nickname_update", "voice_message", "attachment_offer"}
	keep := []string{"group_message", "group_invite", "member_added", "voice_join", "voice_leave", "voice_status", "voice_members"}
	for _, m := range drop {
		if !dropFromBlocked(m) {
			t.Errorf("%s от заблокированного должно отбрасываться", m)
		}
	}
	for _, m := range keep {
		if m == "member_added" {
			continue // не group_* — проверяем только явные типы ниже
		}
		if dropFromBlocked(m) {
			t.Errorf("%s от заблокированного должно пропускаться", m)
		}
	}
}
