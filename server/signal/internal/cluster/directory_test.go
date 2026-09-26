package cluster

import (
	"strings"
	"testing"
	"time"

	"messenger_signal/internal/hub"
	"messenger_signal/pkg/protocol"
)

func entry(pk, node, sess string, ts int64) protocol.NodeDirEntry {
	return protocol.NodeDirEntry{PubKey: pk, NickTag: pk + "#t", NodeID: node, SessionID: sess, TSms: ts}
}

func TestDirectoryPutUpdateSame(t *testing.T) {
	d := NewDirectory()
	e := entry("k1", "a", "s1", 100)

	r := d.Put(e)
	if r.Event == nil || r.Event.Kind != hub.DirPut {
		t.Fatalf("первый put: ожидался DirPut, получено %+v", r)
	}
	if r := d.Put(e); r.Event != nil || r.Rejected || r.Replaced != nil {
		t.Fatalf("повтор той же записи: ожидалась тишина, получено %+v", r)
	}

	e2 := e
	e2.Private = true
	e2.Watch = []string{"w1"}
	r = d.Put(e2)
	if r.Event == nil || r.Event.Kind != hub.DirUpdate || r.Event.Prev == nil || r.Event.Prev.Private {
		t.Fatalf("смена watch: ожидался DirUpdate с прежней записью, получено %+v", r)
	}
	if r.Replaced != nil {
		t.Fatal("та же сессия не должна считаться вытесненной")
	}
}

func TestDirectoryConflictNewerWins(t *testing.T) {
	d := NewDirectory()
	d.Put(entry("k1", "a", "s1", 100))

	r := d.Put(entry("k1", "b", "s2", 200))
	if r.Replaced == nil || r.Replaced.SessionID != "s1" {
		t.Fatalf("более новая сессия должна вытеснить s1, получено %+v", r)
	}
	if r.Event == nil || r.Event.Kind != hub.DirUpdate {
		t.Fatalf("переезд ключа между узлами — DirUpdate, получено %+v", r.Event)
	}

	r = d.Put(entry("k1", "a", "s3", 150))
	if !r.Rejected || r.Event != nil {
		t.Fatalf("более старая сессия должна проиграть, получено %+v", r)
	}
	if e, _ := d.Lookup("k1"); e.SessionID != "s2" {
		t.Fatalf("в каталоге должна остаться s2, а там %s", e.SessionID)
	}
}

func TestDirectoryConflictTieByNodeID(t *testing.T) {
	d := NewDirectory()
	d.Put(entry("k1", "b", "s1", 100))
	if r := d.Put(entry("k1", "a", "s2", 100)); !r.Rejected {
		t.Fatal("при равном ts побеждает больший node_id (b)")
	}
	if r := d.Put(entry("k1", "c", "s3", 100)); r.Replaced == nil {
		t.Fatal("при равном ts узел c должен вытеснить b")
	}
}

func TestDirectoryDelSessionAndTombstone(t *testing.T) {
	d := NewDirectory()
	now := time.Unix(1000, 0)
	d.now = func() time.Time { return now }

	e := entry("k1", "a", "s1", 100)
	e.Private = true
	e.Watch = []string{"w1"}
	d.Put(e)

	if ev := d.Del("a", "k1", "other"); ev != nil {
		t.Fatal("del с чужой сессией должен игнорироваться")
	}
	if ev := d.Del("b", "k1", "s1"); ev != nil {
		t.Fatal("del от чужого узла должен игнорироваться")
	}
	ev := d.Del("a", "k1", "s1")
	if ev == nil || ev.Kind != hub.DirDel {
		t.Fatalf("ожидался DirDel, получено %+v", ev)
	}
	tb, ok := d.Tombstone("k1")
	if !ok || !tb.Private || len(tb.Watch) != 1 {
		t.Fatalf("надгробие должно хранить политику, получено %+v ok=%v", tb, ok)
	}

	now = now.Add(TombstoneTTL + time.Second)
	if _, ok := d.Tombstone("k1"); ok {
		t.Fatal("надгробие должно истечь через TombstoneTTL")
	}

	// Новый put того же ключа снимает надгробие.
	d.Put(e)
	d.Del("a", "k1", "s1")
	d.Put(entry("k1", "a", "s9", 300))
	if _, ok := d.Tombstone("k1"); ok {
		t.Fatal("клиент вернулся — надгробия быть не должно")
	}
}

func TestDirectorySnapshotReplacesNode(t *testing.T) {
	d := NewDirectory()
	d.Put(entry("x", "a", "sx", 1))
	d.Put(entry("y", "a", "sy", 1))
	d.Put(entry("q", "b", "sq", 1))

	res := d.Snapshot("a", []protocol.NodeDirEntry{
		entry("y", "a", "sy", 1),
		entry("z", "a", "sz", 1),
		entry("q", "c", "foreign", 5), // чужой node_id — игнорируется
	})

	var dels, puts int
	for _, r := range res {
		if r.Event == nil {
			continue
		}
		switch r.Event.Kind {
		case hub.DirDel:
			dels++
			if r.Event.Entry.PubKey != "x" {
				t.Fatalf("удалиться должен x, а удалён %s", r.Event.Entry.PubKey)
			}
		case hub.DirPut:
			puts++
			if r.Event.Entry.PubKey != "z" {
				t.Fatalf("добавиться должен z, а добавлен %s", r.Event.Entry.PubKey)
			}
		default:
			t.Fatalf("неожиданное событие %+v", r.Event)
		}
	}
	if dels != 1 || puts != 1 {
		t.Fatalf("ожидалось 1 удаление и 1 добавление, получено %d и %d", dels, puts)
	}
	if e, _ := d.Lookup("q"); e.NodeID != "b" {
		t.Fatal("записи узла b snapshot узла a трогать не должен")
	}
}

func TestDirectoryRemoveNode(t *testing.T) {
	d := NewDirectory()
	d.Put(entry("x", "a", "sx", 1))
	d.Put(entry("y", "a", "sy", 1))
	d.Put(entry("q", "b", "sq", 1))

	evs := d.RemoveNode("a")
	if len(evs) != 2 {
		t.Fatalf("ожидалось 2 удаления, получено %d", len(evs))
	}
	if d.Size() != 1 {
		t.Fatalf("должна остаться запись узла b, размер %d", d.Size())
	}
	if len(d.Except("b")) != 0 || len(d.ByNode("b")) != 1 {
		t.Fatal("Except/ByNode считают неверно")
	}
}

func TestParseConfig(t *testing.T) {
	self := strings.Repeat("a", 64)
	other := strings.Repeat("b", 64)

	cfg, err := ParseConfig("ws://self/ws", "ws://s1:9741/ws="+self+"; ws://s2:9741/ws="+strings.ToUpper(other), 400, self)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Peers) != 1 || cfg.Peers[0].NodeID != other || cfg.Peers[0].Addr != "ws://s2:9741/ws" {
		t.Fatalf("себя надо пропустить, ключ привести к нижнему регистру: %+v", cfg.Peers)
	}
	if !cfg.Enabled() || cfg.SoftLimit != 400 {
		t.Fatalf("конфиг разобран неверно: %+v", cfg)
	}

	if cfg, err := ParseConfig("", "", 400, self); err != nil || cfg.Enabled() {
		t.Fatal("пустой CLUSTER_PEERS — кластер выключен без ошибки")
	}
	for _, bad := range []string{
		"http://s2/ws=" + other,
		"ws://s2/ws=zz",
		"ws://s2/ws",
		"ws://s2/ws=" + other + ";ws://s3/ws=" + other,
	} {
		if _, err := ParseConfig("", bad, 400, self); err == nil {
			t.Fatalf("ожидалась ошибка для %q", bad)
		}
	}
}
