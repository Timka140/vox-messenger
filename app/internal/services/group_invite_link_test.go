package services

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"vox_app/internal/session"
	"vox_app/internal/storage"
)

func TestInviteLinkRoundTrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	gid := uuid.New().String()
	var nonce [8]byte
	_, _ = rand.Read(nonce[:])
	exp := time.Now().Add(time.Hour).Truncate(time.Second)

	link, err := encodeInviteLink(gid, priv, exp, nonce)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := parseInviteLink(link, time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if tok.GroupID != gid || !bytes.Equal(tok.OwnerPub, pub) || !tok.Expires.Equal(exp) || tok.Nonce != nonce {
		t.Fatalf("токен не совпал: %+v", tok)
	}

	if _, err := parseInviteLink(link, exp.Add(time.Second)); !errors.Is(err, errInviteExpired) {
		t.Fatalf("истёкшая ссылка: %v", err)
	}

	raw, _ := base64.RawURLEncoding.DecodeString(link[len(inviteLinkPrefix):])
	tampered := append([]byte(nil), raw...)
	tampered[5] ^= 1 // чужой groupId
	if _, err := parseInviteLink(inviteLinkPrefix+base64.RawURLEncoding.EncodeToString(tampered), time.Now()); !errors.Is(err, errInviteBadSig) {
		t.Fatalf("подмена groupId прошла: %v", err)
	}
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	swapped := append([]byte(nil), raw...)
	copy(swapped[17:49], otherPub) // подпись от одного ключа, ownerPub — другой
	if _, err := parseInviteLink(inviteLinkPrefix+base64.RawURLEncoding.EncodeToString(swapped), time.Now()); !errors.Is(err, errInviteBadSig) {
		t.Fatalf("подмена владельца прошла: %v", err)
	}

	if _, err := parseInviteLink("vox://abc/def", time.Now()); !errors.Is(err, errInviteMalformed) {
		t.Fatalf("ссылка-контакт принята за приглашение: %v", err)
	}
	if _, err := encodeInviteLink("not-a-uuid", priv, exp, nonce); err == nil {
		t.Fatal("группа без UUID закодирована")
	}
}

// joinOwner — владелец группы с отдельной БД; ответы, ушедшие пирам,
// складываются в sent.
type joinOwner struct {
	svc  *GroupsService
	id   string
	gid  string
	sent []P2PMessage
}

func newJoinOwner(t *testing.T) *joinOwner {
	t.Helper()
	store, err := storage.Open(filepath.Join(t.TempDir(), "owner.db"), bytes.Repeat([]byte{3}, 32))
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
	network := NewNetworkService(log, sess, store)
	o := &joinOwner{svc: NewGroupsService(log, sess, store, network), gid: uuid.New().String()}
	o.id, _ = sess.PeerID()
	network.SetTestConnected(func(string) bool { return true })
	network.SetTestSender(func(_ string, data []byte) error {
		var m P2PMessage
		if err := json.Unmarshal(data, &m); err == nil {
			o.sent = append(o.sent, m)
		}
		return nil
	})
	now := time.Now().Unix()
	store.DB().Exec("INSERT INTO groups (group_id, name, owner_id, type, created_at) VALUES (?, 'Тест', ?, 0, ?)", o.gid, o.id, now)
	store.DB().Exec("INSERT INTO group_members (group_id, peer_id, role, joined_at, last_read_at) VALUES (?, ?, 'admin', ?, ?)", o.gid, o.id, now, now)
	return o
}

// ask — заявка от peer со ссылкой link; возвращает код ответа.
func (o *joinOwner) ask(t *testing.T, peer, link string) string {
	t.Helper()
	o.sent = nil
	o.svc.handleJoinRequest(peer, &P2PMessage{Type: "group_join_request", GroupID: o.gid, Content: link})
	for _, m := range o.sent {
		if m.Type == "group_join_result" {
			return m.Content
		}
	}
	t.Fatalf("нет group_join_result, ушло: %+v", o.sent)
	return ""
}

func randPeerID() string {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	return hex.EncodeToString(pub)
}

func TestJoinRequestOwnerChecks(t *testing.T) {
	o := newJoinOwner(t)
	info, err := o.svc.GetGroupInviteLink(o.gid)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := o.svc.GetGroupInviteLink(o.gid)
	if again.Link != info.Link {
		t.Fatal("живая ссылка выпущена заново")
	}

	alice := randPeerID()
	if code := o.ask(t, alice, info.Link); code != "ok" {
		t.Fatalf("вступление: %s", code)
	}
	gotInvite := false
	for _, m := range o.sent {
		if m.Type == "group_invite" {
			gotInvite = true
		}
	}
	if !gotInvite || o.svc.joinMemberStatus(o.gid, alice) != "active" {
		t.Fatalf("участник не добавлен: invite=%v status=%q", gotInvite, o.svc.joinMemberStatus(o.gid, alice))
	}

	// Вышедший вступает снова — строка 'left' становится 'active'.
	o.svc.store.DB().Exec("UPDATE group_members SET status = 'left' WHERE group_id = ? AND peer_id = ?", o.gid, alice)
	if code := o.ask(t, alice, info.Link); code != "ok" || o.svc.joinMemberStatus(o.gid, alice) != "active" {
		t.Fatalf("повторное вступление: %s / %q", code, o.svc.joinMemberStatus(o.gid, alice))
	}

	// Исключённый не возвращается по ссылке.
	if err := o.svc.KickGroupMember(o.gid, alice); err != nil {
		t.Fatal(err)
	}
	if code := o.ask(t, alice, info.Link); code != "banned" {
		t.Fatalf("исключённый: %s", code)
	}

	// Сброс — старая ссылка больше не работает, новая работает.
	fresh, err := o.svc.ResetGroupInviteLink(o.gid)
	if err != nil || fresh.Link == info.Link {
		t.Fatalf("сброс: %v", err)
	}
	bob := randPeerID()
	if code := o.ask(t, bob, info.Link); code != "revoked" {
		t.Fatalf("старая ссылка после сброса: %s", code)
	}
	if code := o.ask(t, bob, fresh.Link); code != "ok" {
		t.Fatalf("новая ссылка: %s", code)
	}

	// Ссылка чужого владельца на эту группу.
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	var n [8]byte
	forged, _ := encodeInviteLink(o.gid, otherPriv, time.Now().Add(time.Hour), n)
	if code := o.ask(t, randPeerID(), forged); code != "revoked" {
		t.Fatalf("чужая ссылка: %s", code)
	}

	// Владелец вышел из группы — заявки не принимаются.
	o.svc.store.DB().Exec("UPDATE group_members SET status = 'left' WHERE group_id = ? AND peer_id = ?", o.gid, o.id)
	if code := o.ask(t, randPeerID(), fresh.Link); code != "gone" {
		t.Fatalf("владелец вышел: %s", code)
	}
}
