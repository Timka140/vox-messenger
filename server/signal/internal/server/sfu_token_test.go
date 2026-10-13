package server

import (
	"encoding/json"
	"testing"
	"time"

	"messenger_signal/internal/config"
	"messenger_signal/pkg/protocol"
)

func findSFUToken(ms []protocol.Message) (protocol.SFUTokenPayload, bool) {
	for _, m := range ms {
		if m.Type == protocol.TypeSFUToken {
			var p protocol.SFUTokenPayload
			if json.Unmarshal(m.Payload, &p) == nil {
				return p, true
			}
		}
	}
	return protocol.SFUTokenPayload{}, false
}

// Токен SFU выдаёт узел: секрета в auth_ok нет, peer в токене — ключ
// соединения, в группу через сервер пускают только участников.
func TestSFUTokenIssuedByNode(t *testing.T) {
	b := newGroupBench(t, 4096, config.SFUConfig{Endpoint: "ws://sfu:9760/ws", Secret: "sfu-secret"})
	connA, chA, me, _ := b.client(t, "a#1")
	connB, chB, other, _ := b.client(t, "b#1")

	ask := func(t *testing.T, ch <-chan protocol.Message, send func(), room string) protocol.SFUTokenPayload {
		t.Helper()
		send()
		res, ok := findSFUToken(collect(ch, 500*time.Millisecond))
		if !ok || res.Room != room {
			t.Fatalf("нет sfu_token для %q: %+v", room, res)
		}
		return res
	}

	// Комната, которой узел не знает (P2P-группа, звонок), — токен на свой ключ.
	room := "5b0f0c4e-1d2a-4c3b-9e8f-0a1b2c3d4e5f"
	res := ask(t, chA, func() {
		sendMsg(t, connA, protocol.TypeSFUTokenReq, nil, protocol.SFUTokenReqPayload{Room: room})
	}, room)
	if res.Error != "" || res.Peer != me {
		t.Fatalf("токен на ключ соединения: %+v", res)
	}
	if want := sfuToken("sfu-secret", me, room, res.TS); res.Token != want {
		t.Fatal("токен не сходится с HMAC SFU")
	}
	if d := time.Now().Unix() - res.TS; d < 0 || d > 5 {
		t.Fatalf("ts токена: %d", res.TS)
	}

	// Группа через сервер: участник получает токен, посторонний — нет.
	sendMsg(t, connA, protocol.TypeGroupRegister, nil, protocol.GroupRegisterPayload{GroupID: "g1", Members: []string{me}, Admins: []string{me}})
	collect(chA, 300*time.Millisecond)
	if res := ask(t, chA, func() {
		sendMsg(t, connA, protocol.TypeSFUTokenReq, nil, protocol.SFUTokenReqPayload{Room: "g1"})
	}, "g1"); res.Error != "" || res.Token == "" {
		t.Fatalf("участник группы: %+v", res)
	}
	if res := ask(t, chB, func() {
		sendMsg(t, connB, protocol.TypeSFUTokenReq, nil, protocol.SFUTokenReqPayload{Room: "g1"})
	}, "g1"); res.Error != protocol.ErrNotGroupMember || res.Token != "" {
		t.Fatalf("посторонний %s получил токен: %+v", other[:8], res)
	}

	// Комната с символами вне [A-Za-z0-9_-] уйдёт в URL SFU — отказ.
	if res := ask(t, chA, func() {
		sendMsg(t, connA, protocol.TypeSFUTokenReq, nil, protocol.SFUTokenReqPayload{Room: "a&peer=x"})
	}, "a&peer=x"); res.Error != "invalid_room" {
		t.Fatalf("неверная комната: %+v", res)
	}

	// Узел без SFU.
	nb := newGroupBench(t, 4096)
	connN, chN, _, _ := nb.client(t, "n#1")
	if res := ask(t, chN, func() {
		sendMsg(t, connN, protocol.TypeSFUTokenReq, nil, protocol.SFUTokenReqPayload{Room: room})
	}, room); res.Error != "sfu_disabled" {
		t.Fatalf("без SFU: %+v", res)
	}
}
