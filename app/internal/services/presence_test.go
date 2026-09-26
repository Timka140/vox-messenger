package services

import (
	"reflect"
	"testing"
)

// presenceWatch — друзья и активные участники моих групп; без заявок,
// вышедших, чужих групп, групп, из которых вышел я, заблокированных и себя.
func TestPresenceWatch(t *testing.T) {
	n, store, sess := newBlockTestNetwork(t)
	myID, _ := sess.PeerID()
	db := store.DB()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}

	exec(`INSERT INTO contacts (peer_id, nickname, status) VALUES ('friend1', 'F', 'accepted')`)
	exec(`INSERT INTO contacts (peer_id, nickname, status) VALUES ('pending1', 'P', 'pending')`)
	exec(`INSERT INTO groups (group_id, name, owner_id) VALUES ('g1', 'Mine', 'x'), ('g2', 'Left', 'x'), ('g3', 'Other', 'x')`)
	exec(`INSERT INTO group_members (group_id, peer_id) VALUES ('g1', ?), ('g1', 'member1'), ('g1', 'blocked1'), ('g1', 'friend1')`, myID)
	exec(`INSERT INTO group_members (group_id, peer_id, status) VALUES ('g1', 'gone1', 'left')`)
	exec(`INSERT INTO group_members (group_id, peer_id, status) VALUES ('g2', ?, 'left')`, myID)
	exec(`INSERT INTO group_members (group_id, peer_id) VALUES ('g2', 'stranger1'), ('g3', 'stranger2')`)
	if err := n.BlockPeer("blocked1"); err != nil {
		t.Fatal(err)
	}

	got := n.presenceWatch()
	want := []string{"friend1", "member1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("presenceWatch = %v, want %v", got, want)
	}

	// Список для auth запоминается как отправленный.
	n.presenceForAuth()
	if n.presSent != "friend1,member1" {
		t.Fatalf("presSent = %q", n.presSent)
	}

	// Разблокировка возвращает участника группы в список.
	if err := n.UnblockPeer("blocked1"); err != nil {
		t.Fatal(err)
	}
	if got := n.presenceWatch(); !reflect.DeepEqual(got, []string{"blocked1", "friend1", "member1"}) {
		t.Fatalf("после разблокировки = %v", got)
	}
}
