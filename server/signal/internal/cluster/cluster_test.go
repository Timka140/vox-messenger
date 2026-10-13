package cluster_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"messenger_signal/internal/cluster"
	"messenger_signal/internal/config"
	"messenger_signal/internal/groupstore"
	"messenger_signal/internal/auth"
	"messenger_signal/internal/hub"
	"messenger_signal/internal/identity"
	"messenger_signal/internal/server"
	"messenger_signal/pkg/protocol"
)

func TestMain(m *testing.M) {
	cluster.HeartbeatInterval = 40 * time.Millisecond
	cluster.DeadAfter = 250 * time.Millisecond
	cluster.GracePeriod = 300 * time.Millisecond
	cluster.DialMinBackoff = 30 * time.Millisecond
	cluster.DialMaxBackoff = 200 * time.Millisecond
	cluster.HandshakeTimeout = 2 * time.Second
	os.Exit(m.Run())
}

// --- узлы ---

type node struct {
	id       *identity.Identity
	hub      *hub.Hub
	mgr      *cluster.Manager
	ts       *httptest.Server
	ws       string
	groups   *groupstore.Store // nil, если nodeGroups выключен
	stopOnce sync.Once
}

func (n *node) key() string { return n.id.PubKeyHex() }

func (n *node) stop() {
	n.stopOnce.Do(func() {
		n.mgr.Stop()
		n.ts.CloseClientConnections()
		n.ts.Close()
		if n.groups != nil {
			n.groups.Close()
		}
	})
}

func newIdentity(t *testing.T) *identity.Identity {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &identity.Identity{Public: pub, Private: priv}
}

// orderedIDs — n идентичностей по возрастанию node_id.
func orderedIDs(t *testing.T, n int) []*identity.Identity {
	ids := make([]*identity.Identity, n)
	for i := range ids {
		ids[i] = newIdentity(t)
	}
	for i := 1; i < n; i++ {
		for j := i; j > 0 && ids[j].PubKeyHex() < ids[j-1].PubKeyHex(); j-- {
			ids[j], ids[j-1] = ids[j-1], ids[j]
		}
	}
	return ids
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// nodeSoftLimit — CLUSTER_SOFT_LIMIT узлов в startNodes.
var nodeSoftLimit = 400

// nodeGroups — узлы в startNodes получают групповой relay (своя БД на узел).
var nodeGroups = false

// startNodes поднимает узлы в процессе. extraPeers — дополнительные записи
// CLUSTER_PEERS (поддельные узлы).
func startNodes(t *testing.T, ids []*identity.Identity, extraPeers ...string) []*node {
	t.Helper()
	nodes := make([]*node, len(ids))
	peers := append([]string{}, extraPeers...)
	for i, id := range ids {
		ts := httptest.NewServer(nil)
		nodes[i] = &node{id: id, ts: ts, ws: "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"}
		peers = append(peers, nodes[i].ws+"="+id.PubKeyHex())
	}
	for _, n := range nodes {
		log := quietLog()
		n.hub = hub.New(log)
		srv := server.New(n.hub, config.WSConfig{Path: "/ws"}, config.TURNConfig{}, config.SFUConfig{}, log, n.id)
		cfg, err := cluster.ParseConfig(n.ws, strings.Join(peers, ";"), nodeSoftLimit, n.key())
		if err != nil {
			t.Fatal(err)
		}
		n.mgr = cluster.NewManager(cfg, n.id, n.hub, log)
		n.hub.SetCluster(n.mgr)
		srv.SetCluster(n.mgr)
		if nodeGroups {
			gs, err := groupstore.Open(filepath.Join(t.TempDir(), n.key()[:8]+".db"), n.key(), log)
			if err != nil {
				t.Fatal(err)
			}
			srv.SetGroupStore(gs, config.GroupConfig{
				MaxMsgBytes: 4096, MaxSyncLimit: 500, E2EThreshold: 200, RateBurst: 30, RateRefill: 5,
			})
			n.mgr.SetGroupHandler(srv)
			n.groups = gs
		}
		n.ts.Config.Handler = srv.Handler()
	}
	for _, n := range nodes {
		n.mgr.Start()
	}
	t.Cleanup(func() {
		for _, n := range nodes {
			n.stop()
		}
	})
	return nodes
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("не дождались: %s", what)
}

func waitLinked(t *testing.T, nodes ...*node) {
	t.Helper()
	waitUntil(t, "линки поднялись", func() bool {
		for _, n := range nodes {
			for _, l := range n.mgr.Health().Links {
				if !l.Alive {
					return false
				}
			}
		}
		return true
	})
}

// --- клиенты ---

type client struct {
	conn *websocket.Conn
	pub  ed25519.PublicKey
	msgs chan protocol.Message
	wmu  sync.Mutex
}

func (c *client) key() string { return hex.EncodeToString(c.pub) }

type keypair struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newKey(t *testing.T) keypair {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return keypair{pub, priv}
}

func (k keypair) hex() string { return hex.EncodeToString(k.pub) }

func watchOf(keys ...keypair) *protocol.PresencePayload {
	p := &protocol.PresencePayload{Watch: []string{}}
	for _, k := range keys {
		p.Watch = append(p.Watch, k.hex())
	}
	return p
}

// connect проходит auth клиента на узле n и запускает чтение в канал.
func connect(t *testing.T, n *node, nick string, k keypair, pres *protocol.PresencePayload) *client {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(n.ws, nil)
	if err != nil {
		t.Fatal("dial:", err)
	}
	var ch protocol.Message
	if err := conn.ReadJSON(&ch); err != nil || ch.Type != protocol.TypeChallenge {
		t.Fatalf("challenge: %v %s", err, ch.Type)
	}
	var cp protocol.ChallengePayload
	_ = json.Unmarshal(ch.Payload, &cp)
	chBytes, _ := hex.DecodeString(cp.Challenge)
	cc := make([]byte, 32)
	_, _ = rand.Read(cc)
	ap, _ := json.Marshal(protocol.AuthPayload{
		PublicKey:       k.hex(),
		NickTag:         nick,
		Signature:       hex.EncodeToString(ed25519.Sign(k.priv, clientAuthMsg(cp, chBytes, k.pub))),
		ClientChallenge: hex.EncodeToString(cc),
		Presence:        pres,
		AuthVersion:     auth.AuthVersion,
	})
	if err := conn.WriteJSON(protocol.Message{Type: protocol.TypeAuth, Payload: ap}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{protocol.TypeAuthOK, protocol.TypeICEServers} {
		var m protocol.Message
		if err := conn.ReadJSON(&m); err != nil || m.Type != want {
			t.Fatalf("ожидался %s: %v %s", want, err, m.Type)
		}
	}
	c := &client{conn: conn, pub: k.pub, msgs: make(chan protocol.Message, 256)}
	go func() {
		defer close(c.msgs)
		for {
			var m protocol.Message
			if err := conn.ReadJSON(&m); err != nil {
				return
			}
			c.msgs <- m
		}
	}()
	t.Cleanup(func() { conn.Close() })
	return c
}

func (c *client) send(t *testing.T, typ, to string, payload any) {
	t.Helper()
	raw, _ := json.Marshal(payload)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.conn.WriteJSON(protocol.Message{Type: typ, To: to, Payload: raw}); err != nil {
		t.Fatal(err)
	}
}

// waitMsg ждёт сообщение, подходящее под pred.
func waitMsg(t *testing.T, c *client, what string, pred func(protocol.Message) bool) protocol.Message {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case m, ok := <-c.msgs:
			if !ok {
				t.Fatalf("соединение закрыто, не дождались: %s", what)
			}
			if pred(m) {
				return m
			}
		case <-timeout:
			t.Fatalf("не дождались: %s", what)
		}
	}
}

// noMsg — за d не пришло сообщения, подходящего под pred.
func noMsg(t *testing.T, c *client, d time.Duration, what string, pred func(protocol.Message) bool) {
	t.Helper()
	timeout := time.After(d)
	for {
		select {
		case m, ok := <-c.msgs:
			if !ok {
				return
			}
			if pred(m) {
				t.Fatalf("не должно было прийти: %s (%+v)", what, m)
			}
		case <-timeout:
			return
		}
	}
}

func isStatus(typ, from string) func(protocol.Message) bool {
	return func(m protocol.Message) bool { return m.Type == typ && m.From == from }
}

func isErr(code string) func(protocol.Message) bool {
	return func(m protocol.Message) bool {
		if m.Type != protocol.TypeError {
			return false
		}
		var e protocol.ErrorPayload
		_ = json.Unmarshal(m.Payload, &e)
		return e.Code == code
	}
}

// --- тесты двух настоящих узлов ---

func TestLinkAndHealthz(t *testing.T) {
	nodes := startNodes(t, orderedIDs(t, 2))
	waitLinked(t, nodes...)

	resp, err := http.Get(nodes[0].ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var h struct {
		NodeID    string               `json:"node_id"`
		SoftLimit int                  `json:"soft_limit"`
		Links     []cluster.LinkHealth `json:"links"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatal(err)
	}
	if h.NodeID != nodes[0].key() || h.SoftLimit != 400 {
		t.Fatalf("healthz: %+v", h)
	}
	if len(h.Links) != 1 || !h.Links[0].Alive || h.Links[0].NodeID != nodes[1].key() {
		t.Fatalf("healthz: ожидался живой линк с узлом 2, получено %+v", h.Links)
	}
}

func TestCrossNodePresenceRouteSearch(t *testing.T) {
	nodes := startNodes(t, orderedIDs(t, 2))
	waitLinked(t, nodes...)
	kA, kB := newKey(t), newKey(t)

	a := connect(t, nodes[0], "alice#"+kA.hex()[:16], kA, watchOf(kB))
	b := connect(t, nodes[1], "bob#"+kB.hex()[:16], kB, watchOf(kA))

	waitMsg(t, a, "peer_online B на узле 1", isStatus(protocol.TypePeerOnline, kB.hex()))

	// Поиск на узле 2 находит A по точному тегу.
	b.send(t, protocol.TypePeerSearch, "", protocol.PeerSearchPayload{Query: "alice#" + kA.hex()[:16]})
	res := waitMsg(t, b, "peer_result", func(m protocol.Message) bool { return m.Type == protocol.TypePeerResult })
	var pr protocol.PeerResultPayload
	_ = json.Unmarshal(res.Payload, &pr)
	if len(pr.Peers) != 1 || pr.Peers[0].PublicKey != kA.hex() {
		t.Fatalf("поиск через узлы: %+v", pr.Peers)
	}

	// Offer A → B через линк, from подставлен.
	a.send(t, protocol.TypeOffer, kB.hex(), map[string]string{"sdp": "x"})
	off := waitMsg(t, b, "offer от A", func(m protocol.Message) bool { return m.Type == protocol.TypeOffer })
	if off.From != kA.hex() {
		t.Fatalf("from должен быть A, а %s", off.From)
	}

	// B ушёл → A видит офлайн.
	b.conn.Close()
	waitMsg(t, a, "peer_offline B", isStatus(protocol.TypePeerOffline, kB.hex()))
}

func TestNodeDownGrace(t *testing.T) {
	nodes := startNodes(t, orderedIDs(t, 2))
	waitLinked(t, nodes...)
	kA, kB := newKey(t), newKey(t)

	connect(t, nodes[0], "alice#1", kA, watchOf(kB))
	b := connect(t, nodes[1], "bob#1", kB, watchOf(kA))
	waitUntil(t, "запись A дошла до узла 2", func() bool {
		_, ok := nodes[1].mgr.Lookup(kA.hex())
		return ok
	})

	start := time.Now()
	nodes[0].stop()
	waitMsg(t, b, "peer_offline A после grace", isStatus(protocol.TypePeerOffline, kA.hex()))
	if elapsed := time.Since(start); elapsed < cluster.GracePeriod {
		t.Fatalf("офлайн пришёл раньше grace: %v", elapsed)
	}
	if _, ok := nodes[1].mgr.Lookup(kA.hex()); ok {
		t.Fatal("запись A должна быть удалена")
	}
}

func TestSessionReplacedAcrossNodes(t *testing.T) {
	nodes := startNodes(t, orderedIDs(t, 2))
	waitLinked(t, nodes...)
	kW, kK := newKey(t), newKey(t)

	w := connect(t, nodes[0], "watcher#1", kW, watchOf(kK))
	k1 := connect(t, nodes[0], "k#1", kK, watchOf(kW))
	waitMsg(t, w, "peer_online K", isStatus(protocol.TypePeerOnline, kK.hex()))
	waitUntil(t, "запись K дошла до узла 2", func() bool {
		_, ok := nodes[1].mgr.Lookup(kK.hex())
		return ok
	})

	time.Sleep(20 * time.Millisecond) // гарантированно больший ts у второй сессии
	connect(t, nodes[1], "k#1", kK, watchOf(kW))

	waitMsg(t, k1, "session_replaced у старой сессии", isErr(protocol.ErrSessionReplaced))
	waitUntil(t, "K на узле 2 в каталоге узла 1", func() bool {
		e, ok := nodes[0].mgr.Lookup(kK.hex())
		return ok && e.NodeID == nodes[1].key()
	})
	noMsg(t, w, 500*time.Millisecond, "peer_offline K при переезде", isStatus(protocol.TypePeerOffline, kK.hex()))
}

func TestHiddenOfflineAcrossNodes(t *testing.T) {
	nodes := startNodes(t, orderedIDs(t, 2))
	waitLinked(t, nodes...)
	kA, kC, kT := newKey(t), newKey(t), newKey(t)

	a := connect(t, nodes[0], "alice#1", kA, watchOf(kT))
	c := connect(t, nodes[0], "carol#1", kC, watchOf())
	tt := connect(t, nodes[1], "target#1", kT, watchOf(kA)) // статус виден только A
	waitUntil(t, "запись T дошла до узла 1", func() bool {
		_, ok := nodes[0].mgr.Lookup(kT.hex())
		return ok
	})
	tt.conn.Close()
	waitMsg(t, a, "peer_offline T", isStatus(protocol.TypePeerOffline, kT.hex()))

	// Надгробие T на узле 1: A получает peer_offline_err, C — тишину.
	a.send(t, protocol.TypeOffer, kT.hex(), map[string]string{})
	waitMsg(t, a, "peer_offline_err для A", isErr(protocol.ErrPeerOffline))
	c.send(t, protocol.TypeOffer, kT.hex(), map[string]string{})
	noMsg(t, c, 300*time.Millisecond, "peer_offline_err для скрытого C", isErr(protocol.ErrPeerOffline))
}

// authFirst — auth клиента на узле n; возвращает первое сообщение после auth
// (auth_ok или redirect), свой client_challenge и соединение.
func authFirst(t *testing.T, n *node, k keypair, redirectOK bool) (protocol.Message, []byte, *websocket.Conn) {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(n.ws, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	var ch protocol.Message
	if err := conn.ReadJSON(&ch); err != nil {
		t.Fatal(err)
	}
	var cp protocol.ChallengePayload
	_ = json.Unmarshal(ch.Payload, &cp)
	chBytes, _ := hex.DecodeString(cp.Challenge)
	cc := make([]byte, 32)
	_, _ = rand.Read(cc)
	ap, _ := json.Marshal(protocol.AuthPayload{
		PublicKey:       k.hex(),
		NickTag:         "x#" + k.hex()[:16],
		Signature:       hex.EncodeToString(ed25519.Sign(k.priv, clientAuthMsg(cp, chBytes, k.pub))),
		ClientChallenge: hex.EncodeToString(cc),
		Presence:        watchOf(),
		RedirectOK:      redirectOK,
		AuthVersion:     auth.AuthVersion,
	})
	if err := conn.WriteJSON(protocol.Message{Type: protocol.TypeAuth, Payload: ap}); err != nil {
		t.Fatal(err)
	}
	var first protocol.Message
	if err := conn.ReadJSON(&first); err != nil {
		t.Fatal(err)
	}
	return first, cc, conn
}

func TestClientAddr(t *testing.T) {
	for in, want := range map[string]string{
		"ws://127.0.0.1:9741/ws":   "127.0.0.1:9741",
		"wss://example.org:443/ws": "example.org:443",
		"ws://host:1/custom":       "host:1/custom",
	} {
		if got := cluster.ClientAddr(in); got != want {
			t.Fatalf("ClientAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestServerListAndRedirect(t *testing.T) {
	nodeSoftLimit = 1
	defer func() { nodeSoftLimit = 400 }()
	nodes := startNodes(t, orderedIDs(t, 2))
	waitLinked(t, nodes...)

	// Первый клиент на узле 0 — лимит не достигнут, после auth приходит server_list.
	a := connect(t, nodes[0], "alice#1", newKey(t), watchOf())
	sl := waitMsg(t, a, "server_list", func(m protocol.Message) bool { return m.Type == protocol.TypeServerList })
	var list protocol.ServerListPayload
	if err := json.Unmarshal(sl.Payload, &list); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		cluster.ClientAddr(nodes[0].ws): nodes[0].key(),
		cluster.ClientAddr(nodes[1].ws): nodes[1].key(),
	}
	if len(list.Nodes) != len(want) {
		t.Fatalf("server_list: %+v", list.Nodes)
	}
	for _, n := range list.Nodes {
		if want[n.Addr] != n.Pubkey {
			t.Fatalf("server_list: лишний или неверный узел %+v", n)
		}
	}

	// Узел 0 узнал по heartbeat, что на узле 1 клиентов нет.
	waitUntil(t, "нагрузка соседа по heartbeat", func() bool {
		h := nodes[0].mgr.Health()
		return len(h.Links) == 1 && h.Links[0].Clients == 0
	})

	// Клиент без redirect_ok получает обычный auth_ok, хотя узел перегружен.
	if first, _, _ := authFirst(t, nodes[0], newKey(t), false); first.Type != protocol.TypeAuthOK {
		t.Fatalf("без redirect_ok ожидался auth_ok, получено %s", first.Type)
	}

	// Клиент с redirect_ok получает подписанный redirect на узел 1.
	first, cc, _ := authFirst(t, nodes[0], newKey(t), true)
	if first.Type != protocol.TypeRedirect {
		t.Fatalf("ожидался redirect, получено %s", first.Type)
	}
	var rp protocol.RedirectPayload
	_ = json.Unmarshal(first.Payload, &rp)
	if rp.Addr != cluster.ClientAddr(nodes[1].ws) || rp.Pubkey != nodes[1].key() {
		t.Fatalf("redirect не на узел 1: %+v", rp)
	}
	sig, _ := hex.DecodeString(rp.ServerSignature)
	rmsg := auth.Message(auth.LabelRedirect, cc, []byte(rp.Addr), []byte(rp.Pubkey), nil)
	if !ed25519.Verify(nodes[0].id.Public, rmsg, sig) {
		t.Fatal("подпись redirect не сходится с ключом узла 0")
	}
}

// --- групповой relay: журнал на узел ---

func isGroupMsg(cmid string) func(protocol.Message) bool {
	return func(m protocol.Message) bool {
		if m.Type != protocol.TypeGroupMessageNew {
			return false
		}
		var p protocol.GroupMessageNewPayload
		return json.Unmarshal(m.Payload, &p) == nil && p.ClientMsgID == cmid
	}
}

func groupPayload(t *testing.T, m protocol.Message) protocol.GroupMessageNewPayload {
	t.Helper()
	var p protocol.GroupMessageNewPayload
	if err := json.Unmarshal(m.Payload, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// groupPair — A на узле 0 регистрирует группу с B на узле 1; ждём, пока
// состав дойдёт до узла 1.
func groupPair(t *testing.T, nodes []*node) (a, b *client, gid string) {
	t.Helper()
	kA, kB := newKey(t), newKey(t)
	a = connect(t, nodes[0], "alice#1", kA, watchOf(kB))
	b = connect(t, nodes[1], "bob#1", kB, watchOf(kA))
	gid = "grp-" + kA.hex()[:8]
	a.send(t, protocol.TypeGroupRegister, "", protocol.GroupRegisterPayload{
		GroupID: gid, Members: []string{kA.hex(), kB.hex()}, Admins: []string{kA.hex()},
	})
	waitMsg(t, a, "group_register_ok", func(m protocol.Message) bool { return m.Type == protocol.TypeGroupRegisterOK })
	waitUntil(t, "состав группы дошёл до узла 1", func() bool {
		ok, _ := nodes[1].groups.IsMember(gid, kB.hex())
		return ok
	})
	return a, b, gid
}

func TestGroupAcrossNodes(t *testing.T) {
	nodeGroups = true
	defer func() { nodeGroups = false }()
	nodes := startNodes(t, orderedIDs(t, 2))
	waitLinked(t, nodes...)
	a, b, gid := groupPair(t, nodes)

	// B на узле 1 пишет: своя копия — подтверждение из журнала узла 1.
	b.send(t, protocol.TypeGroupMessageSend, "", protocol.GroupMessageSendPayload{GroupID: gid, ClientMsgID: "b1", Payload: "привет от B"})
	ack := groupPayload(t, waitMsg(t, b, "своя копия b1", isGroupMsg("b1")))
	if ack.OriginNode != nodes[1].key() || ack.Seq != 1 {
		t.Fatalf("подтверждение b1: %+v", ack)
	}
	if got := groupPayload(t, waitMsg(t, a, "b1 у A на узле 0", isGroupMsg("b1"))); got.OriginNode != nodes[1].key() {
		t.Fatalf("b1 у A: %+v", got)
	}

	// A на узле 0 пишет: seq 1 своего журнала, у B оба сообщения с seq 1.
	a.send(t, protocol.TypeGroupMessageSend, "", protocol.GroupMessageSendPayload{GroupID: gid, ClientMsgID: "a1", Payload: "привет от A"})
	if got := groupPayload(t, waitMsg(t, b, "a1 у B на узле 1", isGroupMsg("a1"))); got.OriginNode != nodes[0].key() || got.Seq != 1 {
		t.Fatalf("a1 у B: %+v", got)
	}

	// Синхронизация на узле 1 отдаёт оба журнала.
	b.send(t, protocol.TypeGroupSync, "", protocol.GroupSyncPayload{GroupID: gid, Cursors: map[string]int64{}})
	res := waitMsg(t, b, "group_sync_result", func(m protocol.Message) bool { return m.Type == protocol.TypeGroupSyncResult })
	var sr protocol.GroupSyncResultPayload
	_ = json.Unmarshal(res.Payload, &sr)
	origins := map[string]bool{}
	for _, m := range sr.Messages {
		origins[m.OriginNode] = true
	}
	if len(sr.Messages) != 2 || !origins[nodes[0].key()] || !origins[nodes[1].key()] {
		t.Fatalf("sync на узле 1: %+v", sr.Messages)
	}
}

func TestGroupCatchUpAfterLinkDrop(t *testing.T) {
	nodeGroups = true
	defer func() { nodeGroups = false }()
	// Восстановить после остановки узлов: Cleanup выполняется в обратном порядке.
	old := cluster.DialMinBackoff
	t.Cleanup(func() { cluster.DialMinBackoff = old })
	cluster.DialMinBackoff = 700 * time.Millisecond

	nodes := startNodes(t, orderedIDs(t, 2))
	waitLinked(t, nodes...)
	a, b, gid := groupPair(t, nodes)

	cluster.DropLinks(nodes[0].mgr)
	waitUntil(t, "линк упал", func() bool {
		h := nodes[1].mgr.Health()
		return len(h.Links) == 1 && !h.Links[0].Alive
	})

	// Сообщение во время обрыва остаётся в журнале узла 1.
	b.send(t, protocol.TypeGroupMessageSend, "", protocol.GroupMessageSendPayload{GroupID: gid, ClientMsgID: "during", Payload: "пока линка нет"})
	waitMsg(t, b, "своя копия during", isGroupMsg("during"))

	// Линк вернулся — обмен состоянием досылает сообщение на узел 0. Догон
	// не рассылается поштучно: A получает подсказку и забирает его синком.
	waitMsg(t, a, "подсказка group_sync_hint у A", isSyncHint(gid))
	a.send(t, protocol.TypeGroupSync, "", protocol.GroupSyncPayload{GroupID: gid, Cursors: map[string]int64{}})
	res := waitMsg(t, a, "group_sync_result", func(m protocol.Message) bool { return m.Type == protocol.TypeGroupSyncResult })
	var sr protocol.GroupSyncResultPayload
	_ = json.Unmarshal(res.Payload, &sr)
	found := false
	for _, m := range sr.Messages {
		if m.ClientMsgID == "during" && m.OriginNode == nodes[1].key() {
			found = true
		}
	}
	if !found {
		t.Fatalf("сообщение из обрыва в синке A: %+v", sr.Messages)
	}
}

func isSyncHint(gid string) func(protocol.Message) bool {
	return func(m protocol.Message) bool {
		if m.Type != protocol.TypeGroupSyncHint {
			return false
		}
		var p protocol.GroupSyncHintPayload
		return json.Unmarshal(m.Payload, &p) == nil && p.GroupID == gid
	}
}

// Догон пачками даёт одну подсказку на группу, а не по одной на пачку:
// каждая подсказка запускает у клиента свою цепочку синхронизации.
func TestGroupCatchUpHintCoalesced(t *testing.T) {
	nodeGroups = true
	defer func() { nodeGroups = false }()
	ids := orderedIDs(t, 2)
	fakeID, realID := ids[0], ids[1] // поддельный меньше — он инициатор
	nodes := startNodes(t, []*identity.Identity{realID}, "ws://127.0.0.1:1/ws="+fakeID.PubKeyHex())
	n := nodes[0]

	f, err := fakeDial(t, n, fakeID)
	if err != nil {
		t.Fatal(err)
	}
	f.wait(t, "snapshot от узла", func(m protocol.Message) bool { return m.Type == protocol.TypeNodeDirSnapshot })

	k := newKey(t)
	c := connect(t, n, "alice#1", k, watchOf())
	gid := "grp-" + k.hex()[:8]
	c.send(t, protocol.TypeGroupRegister, "", protocol.GroupRegisterPayload{GroupID: gid, Members: []string{k.hex()}, Admins: []string{k.hex()}})
	waitMsg(t, c, "group_register_ok", func(m protocol.Message) bool { return m.Type == protocol.TypeGroupRegisterOK })

	// Три пачки догона из журнала поддельного узла подряд.
	seq := int64(0)
	for range 3 {
		var batch []protocol.NodeGrpMsg
		for range 5 {
			seq++
			batch = append(batch, protocol.NodeGrpMsg{
				GroupID: gid, OriginNode: fakeID.PubKeyHex(), Seq: seq, AuthorKey: k.hex(),
				ClientMsgID: "c" + strings.Repeat("x", int(seq)), Payload: "p", ServerTsMs: seq,
			})
		}
		f.send(protocol.TypeNodeGrpMsgs, protocol.NodeGrpMsgsPayload{Messages: batch})
	}

	// Собираем всё, что пришло клиенту: waitMsg/noMsg молча выбрасывают
	// неподходящие сообщения, а здесь важно и то, чего быть не должно.
	hints, fanned := 0, 0
	deadline := time.After(2 * time.Second)
collect:
	for {
		select {
		case m, ok := <-c.msgs:
			if !ok {
				break collect
			}
			if isSyncHint(gid)(m) {
				hints++
			}
			if m.Type == protocol.TypeGroupMessageNew {
				fanned++
			}
		case <-deadline:
			break collect
		}
	}
	if hints != 1 || fanned != 0 {
		t.Fatalf("догон: подсказок %d (нужна 1), поштучных сообщений %d (нужно 0)", hints, fanned)
	}
	waitUntil(t, "все 15 сообщений сохранены", func() bool {
		heads, _ := n.groups.Heads()
		return heads[gid][fakeID.PubKeyHex()] == 15
	})
}

func isDeliveryMode(gid string) func(protocol.Message) bool {
	return func(m protocol.Message) bool {
		if m.Type != protocol.TypeGroupDeliveryMode {
			return false
		}
		var p protocol.GroupDeliveryModePayload
		return json.Unmarshal(m.Payload, &p) == nil &&
			p.GroupID == gid && p.Mode == protocol.DeliveryModeServer
	}
}

// Смена режима на server объявляется участникам на всех узлах: у узла
// регистрации в hub'е только его собственные сокеты.
func TestGroupDeliveryModeAcrossNodes(t *testing.T) {
	nodeGroups = true
	defer func() { nodeGroups = false }()
	nodes := startNodes(t, orderedIDs(t, 2))
	waitLinked(t, nodes...)
	_, b, gid := groupPair(t, nodes)

	waitMsg(t, b, "group_delivery_mode у B на узле 1", isDeliveryMode(gid))
}

// Участник, который был офлайн в момент регистрации, узнаёт режим при
// подключении — иначе он остаётся в p2p и не догоняет историю.
func TestGroupDeliveryModePushedOnConnect(t *testing.T) {
	nodeGroups = true
	defer func() { nodeGroups = false }()
	nodes := startNodes(t, orderedIDs(t, 2))
	waitLinked(t, nodes...)

	kA, kB := newKey(t), newKey(t)
	a := connect(t, nodes[0], "alice#1", kA, watchOf())
	gid := "grp-" + kA.hex()[:8]
	a.send(t, protocol.TypeGroupRegister, "", protocol.GroupRegisterPayload{
		GroupID: gid, Members: []string{kA.hex(), kB.hex()}, Admins: []string{kA.hex()},
	})
	waitMsg(t, a, "group_register_ok", func(m protocol.Message) bool { return m.Type == protocol.TypeGroupRegisterOK })
	waitUntil(t, "состав группы дошёл до узла 1", func() bool {
		ok, _ := nodes[1].groups.IsMember(gid, kB.hex())
		return ok
	})

	b := connect(t, nodes[1], "bob#1", kB, watchOf())
	waitMsg(t, b, "group_delivery_mode после подключения", isDeliveryMode(gid))
}

// Рядовой участник перерегистрирует группу при каждой отправке в режиме
// не-server; его взгляд на состав не должен переписывать общий.
func TestNonAdminRegisterKeepsRoster(t *testing.T) {
	nodeGroups = true
	defer func() { nodeGroups = false }()
	nodes := startNodes(t, orderedIDs(t, 2))
	waitLinked(t, nodes...)

	kA, kB, kC := newKey(t), newKey(t), newKey(t)
	a := connect(t, nodes[0], "alice#1", kA, watchOf())
	b := connect(t, nodes[1], "bob#1", kB, watchOf())
	gid := "grp-" + kA.hex()[:8]
	a.send(t, protocol.TypeGroupRegister, "", protocol.GroupRegisterPayload{
		GroupID: gid,
		Members: []string{kA.hex(), kB.hex(), kC.hex()},
		Admins:  []string{kA.hex()},
	})
	waitMsg(t, a, "group_register_ok", func(m protocol.Message) bool { return m.Type == protocol.TypeGroupRegisterOK })
	waitUntil(t, "состав группы дошёл до узла 1", func() bool {
		ok, _ := nodes[1].groups.IsMember(gid, kC.hex())
		return ok
	})

	// B не админ и шлёт урезанный состав без админов.
	b.send(t, protocol.TypeGroupRegister, "", protocol.GroupRegisterPayload{
		GroupID: gid, Members: []string{kB.hex()},
	})
	waitMsg(t, b, "group_register_ok у не-админа", func(m protocol.Message) bool { return m.Type == protocol.TypeGroupRegisterOK })

	// Даём репликации шанс доехать, если бы состав всё-таки переписали.
	waitMsg(t, b, "group_delivery_mode у B", isDeliveryMode(gid))
	for i, n := range nodes {
		if admin, _ := n.groups.IsAdmin(gid, kA.hex()); !admin {
			t.Fatalf("узел %d: админ остаётся админом", i)
		}
		if member, _ := n.groups.IsMember(gid, kC.hex()); !member {
			t.Fatalf("узел %d: участник не выброшен из группы", i)
		}
	}
}

// Пока линк жив, узлы сверяют головы журналов периодически: сообщение,
// которое не дошло живой репликой, доезжает без переподключения.
func TestGroupPeriodicStateCatchUp(t *testing.T) {
	nodeGroups = true
	defer func() { nodeGroups = false }()
	old := cluster.AntiEntropyInterval
	t.Cleanup(func() { cluster.AntiEntropyInterval = old })
	cluster.AntiEntropyInterval = 100 * time.Millisecond

	nodes := startNodes(t, orderedIDs(t, 2))
	waitLinked(t, nodes...)
	_, _, gid := groupPair(t, nodes)

	// Мимо живой рассылки: как если бы node_grp_msg потерялся.
	stored, _, err := nodes[0].groups.AppendMessage(groupstore.Message{
		GroupID: gid, AuthorKey: "x", ClientMsgID: "lost", Payload: "p", SizeBytes: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "сообщение доехало периодической сверкой", func() bool {
		heads, _ := nodes[1].groups.Heads()
		return heads[gid][nodes[0].key()] == stored.Seq
	})
}

// --- поддельный узел ---

type fakeNode struct {
	conn *websocket.Conn
	msgs chan protocol.Message
	wmu  sync.Mutex
	done chan struct{}
}

// fakeDial — рукопожатие поддельного узла с идентичностью id. Ошибка —
// узел отказал.
func fakeDial(t *testing.T, n *node, id *identity.Identity) (*fakeNode, error) {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(n.ws, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var ch protocol.Message
	if err := conn.ReadJSON(&ch); err != nil {
		t.Fatal(err)
	}
	var cp protocol.ChallengePayload
	_ = json.Unmarshal(ch.Payload, &cp)
	chBytes, _ := hex.DecodeString(cp.Challenge)
	cc := make([]byte, 32)
	_, _ = rand.Read(cc)
	ap, _ := json.Marshal(protocol.NodeAuthPayload{
		NodeID:          id.PubKeyHex(),
		Signature:       hex.EncodeToString(ed25519.Sign(id.Private, nodeAuthMsg(cp, chBytes, id.Public))),
		ClientChallenge: hex.EncodeToString(cc),
	})
	if err := conn.WriteJSON(protocol.Message{Type: protocol.TypeNodeAuth, Payload: ap}); err != nil {
		t.Fatal(err)
	}
	var reply protocol.Message
	if err := conn.ReadJSON(&reply); err != nil {
		conn.Close()
		return nil, err
	}
	if reply.Type != protocol.TypeNodeAuthOK {
		conn.Close()
		return nil, &rejectErr{reply}
	}
	var okp protocol.NodeAuthOKPayload
	_ = json.Unmarshal(reply.Payload, &okp)
	sig, _ := hex.DecodeString(okp.ServerSignature)
	if !ed25519.Verify(n.id.Public, auth.Message(auth.LabelNodeOK, id.Public, cc, nil, n.id.Public), sig) {
		t.Fatal("подпись настоящего узла не сходится")
	}
	_ = conn.SetReadDeadline(time.Time{})

	f := &fakeNode{conn: conn, msgs: make(chan protocol.Message, 256), done: make(chan struct{})}
	go func() {
		defer close(f.msgs)
		for {
			var m protocol.Message
			if err := conn.ReadJSON(&m); err != nil {
				return
			}
			f.msgs <- m
		}
	}()
	// Узел рвёт линк после DeadAfter тишины — пингуем.
	go func() {
		tk := time.NewTicker(40 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-f.done:
				return
			case <-tk.C:
				f.send(protocol.TypeNodePing, protocol.NodePingPayload{TSms: time.Now().UnixMilli()})
			}
		}
	}()
	t.Cleanup(func() { close(f.done); conn.Close() })
	return f, nil
}

type rejectErr struct{ m protocol.Message }

func (e *rejectErr) Error() string { return "rejected: " + e.m.Type + " " + string(e.m.Payload) }

func (f *fakeNode) send(typ string, payload any) {
	raw, _ := json.Marshal(payload)
	f.wmu.Lock()
	defer f.wmu.Unlock()
	_ = f.conn.WriteJSON(protocol.Message{Type: typ, Payload: raw})
}

func (f *fakeNode) wait(t *testing.T, what string, pred func(protocol.Message) bool) protocol.Message {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case m, ok := <-f.msgs:
			if !ok {
				t.Fatalf("линк закрыт, не дождались: %s", what)
			}
			if pred(m) {
				return m
			}
		case <-timeout:
			t.Fatalf("не дождались: %s", what)
		}
	}
}

func TestForwardErrVisibility(t *testing.T) {
	ids := orderedIDs(t, 2)
	fakeID, realID := ids[0], ids[1] // поддельный меньше — он инициатор
	nodes := startNodes(t, []*identity.Identity{realID}, "ws://127.0.0.1:1/ws="+fakeID.PubKeyHex())
	n := nodes[0]

	f, err := fakeDial(t, n, fakeID)
	if err != nil {
		t.Fatal(err)
	}
	f.wait(t, "snapshot от узла", func(m protocol.Message) bool { return m.Type == protocol.TypeNodeDirSnapshot })

	kA, kC, kT := newKey(t), newKey(t), newKey(t)
	f.send(protocol.TypeNodeDirPut, protocol.NodeDirEntry{
		PubKey: kT.hex(), NickTag: "target#1", NodeID: fakeID.PubKeyHex(),
		SessionID: "s1", TSms: time.Now().UnixMilli(), Private: true, Watch: []string{kA.hex()},
	})
	waitUntil(t, "запись T в каталоге", func() bool {
		_, ok := n.mgr.Lookup(kT.hex())
		return ok
	})

	a := connect(t, n, "alice#1", kA, watchOf(kT))
	c := connect(t, n, "carol#1", kC, watchOf())

	for _, sender := range []*client{a, c} {
		sender.send(t, protocol.TypeOffer, kT.hex(), map[string]string{})
		fwd := f.wait(t, "node_fwd", func(m protocol.Message) bool { return m.Type == protocol.TypeNodeFwd })
		var p protocol.NodeFwdPayload
		_ = json.Unmarshal(fwd.Payload, &p)
		if p.From != sender.key() || p.Msg.To != kT.hex() || p.Msg.Type != protocol.TypeOffer {
			t.Fatalf("node_fwd: %+v", p)
		}
		f.send(protocol.TypeNodeFwdErr, protocol.NodeFwdErrPayload{From: p.From, To: kT.hex(), Code: protocol.ErrPeerOffline})
	}
	waitMsg(t, a, "peer_offline_err для A", isErr(protocol.ErrPeerOffline))
	noMsg(t, c, 300*time.Millisecond, "peer_offline_err для скрытого C", isErr(protocol.ErrPeerOffline))

	// node_fwd к локальному клиенту доставляется с from отправителя.
	kX := newKey(t)
	f.send(protocol.TypeNodeFwd, protocol.NodeFwdPayload{
		From: kX.hex(),
		Msg:  protocol.Message{Type: protocol.TypeAnswer, To: kA.hex()},
	})
	ans := waitMsg(t, a, "answer через node_fwd", func(m protocol.Message) bool { return m.Type == protocol.TypeAnswer })
	if ans.From != kX.hex() {
		t.Fatalf("from должен быть X, а %s", ans.From)
	}
	// node_fwd к отсутствующему — node_fwd_err обратно.
	f.send(protocol.TypeNodeFwd, protocol.NodeFwdPayload{
		From: kX.hex(),
		Msg:  protocol.Message{Type: protocol.TypeAnswer, To: newKey(t).hex()},
	})
	f.wait(t, "node_fwd_err", func(m protocol.Message) bool { return m.Type == protocol.TypeNodeFwdErr })
}

func TestHandshakeRejects(t *testing.T) {
	ids := orderedIDs(t, 3)
	smaller, realID, larger := ids[0], ids[1], ids[2]
	nodes := startNodes(t, []*identity.Identity{realID},
		"ws://127.0.0.1:1/ws="+smaller.PubKeyHex(),
		"ws://127.0.0.1:1/ws="+larger.PubKeyHex())
	n := nodes[0]

	if _, err := fakeDial(t, n, newIdentity(t)); err == nil {
		t.Fatal("узел не из CLUSTER_PEERS должен получить отказ")
	}
	if _, err := fakeDial(t, n, larger); err == nil {
		t.Fatal("линк от узла с большим node_id должен получить отказ")
	}
	if _, err := fakeDial(t, n, smaller); err != nil {
		t.Fatalf("узел с меньшим node_id из конфига должен пройти: %v", err)
	}
}

// clientAuthMsg — подписываемые байты входа клиента v2 (без TLS).
func clientAuthMsg(cp protocol.ChallengePayload, ch []byte, clientPub ed25519.PublicKey) []byte {
	srv, _ := hex.DecodeString(cp.ServerPubkey)
	return auth.Message(auth.LabelClient, srv, ch, nil, clientPub)
}

// nodeAuthMsg — подписываемые байты node_auth v2 (без TLS).
func nodeAuthMsg(cp protocol.ChallengePayload, ch []byte, initiator ed25519.PublicKey) []byte {
	recv, _ := hex.DecodeString(cp.ServerPubkey)
	return auth.Message(auth.LabelNode, recv, ch, nil, initiator)
}
