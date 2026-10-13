package server

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"messenger_signal/internal/config"
	"messenger_signal/internal/groupstore"
	"messenger_signal/internal/hub"
	"messenger_signal/internal/identity"
	"messenger_signal/pkg/protocol"
)

func findGroupSync(ms []protocol.Message) (protocol.GroupSyncResultPayload, bool) {
	for _, m := range ms {
		if m.Type == protocol.TypeGroupSyncResult {
			var p protocol.GroupSyncResultPayload
			if json.Unmarshal(m.Payload, &p) == nil {
				return p, true
			}
		}
	}
	return protocol.GroupSyncResultPayload{}, false
}

func findError(ms []protocol.Message) (protocol.ErrorPayload, bool) {
	for _, m := range ms {
		if m.Type == protocol.TypeError {
			var p protocol.ErrorPayload
			if json.Unmarshal(m.Payload, &p) == nil {
				return p, true
			}
		}
	}
	return protocol.ErrorPayload{}, false
}

// groupBench — одиночный узел с групповым relay и подключённым клиентом.
type groupBench struct {
	srv       *Server
	groups    *groupstore.Store
	nodeID    string
	wsURL     string
	serverPub []byte
}

func newGroupBench(t *testing.T, maxMsgBytes int, sfu ...config.SFUConfig) *groupBench {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := hub.New(log)
	pub, priv := genKey(t)
	id := &identity.Identity{Public: pub, Private: priv}
	var sfuCfg config.SFUConfig
	if len(sfu) > 0 {
		sfuCfg = sfu[0]
	}
	srv := New(h, config.WSConfig{Path: "/ws"}, config.TURNConfig{}, sfuCfg, log, id)
	gs, err := groupstore.Open(filepath.Join(t.TempDir(), "g.db"), id.PubKeyHex(), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gs.Close() })
	srv.SetGroupStore(gs, config.GroupConfig{MaxMsgBytes: maxMsgBytes, MaxSyncLimit: 500, E2EThreshold: 200, RateBurst: 30, RateRefill: 5})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &groupBench{
		srv: srv, groups: gs, nodeID: id.PubKeyHex(),
		wsURL: "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws", serverPub: id.Public,
	}
}

// client подключает нового клиента; возвращает соединение, поток сообщений и ключ.
func (b *groupBench) client(t *testing.T, nick string) (*websocket.Conn, <-chan protocol.Message, string, []byte) {
	t.Helper()
	pub, priv := genKey(t)
	conn := connectAndAuthKey(t, b.wsURL, nick, b.serverPub, pub, priv, watch())
	t.Cleanup(func() { conn.Close() })
	return conn, pump(conn), hex.EncodeToString(pub), priv
}

// Без курсоров синхронизация отклоняется: клиент до Э3 слал since_seq —
// курсор одного журнала — и при нескольких узлах крутил её бесконечно.
func TestGroupSyncRequiresCursors(t *testing.T) {
	b := newGroupBench(t, 4096)
	conn, ch, me, _ := b.client(t, "a#1")

	sendMsg(t, conn, protocol.TypeGroupRegister, nil, protocol.GroupRegisterPayload{GroupID: "g", Members: []string{me}, Admins: []string{me}})
	sendMsg(t, conn, protocol.TypeGroupMessageSend, nil, protocol.GroupMessageSendPayload{GroupID: "g", ClientMsgID: "m1", Payload: "x"})
	var own protocol.GroupMessageNewPayload
	for _, m := range collect(ch, 500*time.Millisecond) {
		if m.Type == protocol.TypeGroupMessageNew {
			_ = json.Unmarshal(m.Payload, &own)
		}
	}
	if own.ClientMsgID != "m1" || own.OriginNode != b.nodeID || own.Seq != 1 {
		t.Fatalf("fan-out с origin_node узла: %+v", own)
	}

	// Запрос старого клиента — сырой JSON без cursors.
	raw := `{"type":"group_sync","payload":{"group_id":"g","since_seq":0}}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(raw)); err != nil {
		t.Fatal(err)
	}
	got := collect(ch, 500*time.Millisecond)
	if _, ok := findGroupSync(got); ok {
		t.Fatal("без курсоров результата синхронизации быть не должно")
	}
	if e, ok := findError(got); !ok || e.Code != protocol.ErrInvalidMsg {
		t.Fatalf("без курсоров — invalid_message: %+v", e)
	}

	sync := func(cursors map[string]int64) protocol.GroupSyncResultPayload {
		sendMsg(t, conn, protocol.TypeGroupSync, nil, protocol.GroupSyncPayload{GroupID: "g", Cursors: cursors})
		res, ok := findGroupSync(collect(ch, 500*time.Millisecond))
		if !ok {
			t.Fatal("нет group_sync_result")
		}
		return res
	}
	if res := sync(map[string]int64{}); len(res.Messages) != 1 || res.Messages[0].ClientMsgID != "m1" {
		t.Fatalf("пустые курсоры — всё с начала: %+v", res.Messages)
	}
	if res := sync(map[string]int64{b.nodeID: 1}); len(res.Messages) != 0 {
		t.Fatalf("курсор на голове журнала — всё уже получено: %+v", res.Messages)
	}
}

func groupMsgWith(ms []protocol.Message, cmid string) (protocol.GroupMessageNewPayload, bool) {
	for _, m := range ms {
		if m.Type != protocol.TypeGroupMessageNew {
			continue
		}
		var p protocol.GroupMessageNewPayload
		if json.Unmarshal(m.Payload, &p) == nil && p.ClientMsgID == cmid {
			return p, true
		}
	}
	return protocol.GroupMessageNewPayload{}, false
}

// Отказ несёт id сообщения — клиент снимает его с повтора. Размер
// проверяется по телу, а не по кадру с экранированием.
func TestGroupSendRejectCarriesIDs(t *testing.T) {
	b := newGroupBench(t, 4096)
	conn, ch, me, _ := b.client(t, "a#1")
	sendMsg(t, conn, protocol.TypeGroupRegister, nil, protocol.GroupRegisterPayload{GroupID: "g", Members: []string{me}, Admins: []string{me}})
	collect(ch, 300*time.Millisecond)

	sendMsg(t, conn, protocol.TypeGroupMessageSend, nil, protocol.GroupMessageSendPayload{
		GroupID: "g", ClientMsgID: "big", Payload: strings.Repeat("x", 4097),
	})
	e, ok := findError(collect(ch, 500*time.Millisecond))
	if !ok || e.Code != protocol.ErrMessageTooLarge || e.GroupID != "g" || e.ClientMsgID != "big" {
		t.Fatalf("отказ по размеру с id сообщения: %+v", e)
	}

	// 4000 символов «<» в JSON — 24 КБ из-за экранирования, но тело в лимите.
	sendMsg(t, conn, protocol.TypeGroupMessageSend, nil, protocol.GroupMessageSendPayload{
		GroupID: "g", ClientMsgID: "lt", Payload: strings.Repeat("<", 4000),
	})
	got := collect(ch, 500*time.Millisecond)
	if _, ok := groupMsgWith(got, "lt"); !ok {
		e, _ := findError(got)
		t.Fatalf("тело в лимите принимается, сколько бы ни раздул его JSON: %+v", e)
	}
}

// Повтор уже принятого сообщения после удаления автора из группы получает
// сохранённую копию, а не отказ: остальные сообщение уже получили.
func TestGroupSendRetryAfterRemovalReturnsCopy(t *testing.T) {
	b := newGroupBench(t, 4096)
	connA, chA, ka, _ := b.client(t, "a#1")
	connB, chB, kb, _ := b.client(t, "b#1")
	sendMsg(t, connA, protocol.TypeGroupRegister, nil, protocol.GroupRegisterPayload{GroupID: "g", Members: []string{ka, kb}, Admins: []string{ka}})
	collect(chA, 300*time.Millisecond)

	sendMsg(t, connB, protocol.TypeGroupMessageSend, nil, protocol.GroupMessageSendPayload{GroupID: "g", ClientMsgID: "m1", Payload: "привет"})
	if _, ok := groupMsgWith(collect(chB, 500*time.Millisecond), "m1"); !ok {
		t.Fatal("своя копия m1")
	}

	sendMsg(t, connA, protocol.TypeGroupMemberRemove, nil, protocol.GroupMemberChangePayload{GroupID: "g", PublicKey: kb})
	collect(chA, 300*time.Millisecond)

	sendMsg(t, connB, protocol.TypeGroupMessageSend, nil, protocol.GroupMessageSendPayload{GroupID: "g", ClientMsgID: "m1", Payload: "привет"})
	got := collect(chB, 500*time.Millisecond)
	if _, ok := groupMsgWith(got, "m1"); !ok {
		e, _ := findError(got)
		t.Fatalf("повтор принятого — копия, а не отказ: %+v", e)
	}
	if _, isErr := findError(got); isErr {
		t.Fatal("отказа при повторе принятого быть не должно")
	}

	sendMsg(t, connB, protocol.TypeGroupMessageSend, nil, protocol.GroupMessageSendPayload{GroupID: "g", ClientMsgID: "m2", Payload: "ещё"})
	e, ok := findError(collect(chB, 500*time.Millisecond))
	if !ok || e.Code != protocol.ErrNotGroupMember || e.ClientMsgID != "m2" {
		t.Fatalf("новое сообщение удалённого — отказ с id: %+v", e)
	}
}
