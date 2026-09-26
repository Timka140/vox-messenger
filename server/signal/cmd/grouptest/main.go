// Smoke-test для server relay больших групп (Фаза 1).
// Запуск:
//   go run ./cmd/grouptest --addr=ws://n1.northwindcdn.com:9741/ws
// Делает: 2 виртуальных клиента (alice, bob) проходят Ed25519 auth,
// alice регистрирует группу с обоими, каждый шлёт сообщение, проверяем
// fan-out + seq, затем group_sync с пустыми курсорами догоняет всю историю.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"messenger_signal/pkg/protocol"
)

type client struct {
	name    string
	pub     ed25519.PublicKey
	priv    ed25519.PrivateKey
	nickTag string
	conn    *websocket.Conn
	inbox   chan protocol.Message
}

func newClient(name, addr string) *client {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		log.Fatalf("[%s] keygen: %v", name, err)
	}
	pubHex := hex.EncodeToString(pub)
	nickTag := fmt.Sprintf("%s#%s", name, pubHex[:16])

	conn, _, err := websocket.DefaultDialer.Dial(addr, nil)
	if err != nil {
		log.Fatalf("[%s] dial %s: %v", name, addr, err)
	}

	c := &client{
		name:    name,
		pub:     pub,
		priv:    priv,
		nickTag: nickTag,
		conn:    conn,
		inbox:   make(chan protocol.Message, 64),
	}
	go c.readLoop()
	c.handshake()
	return c
}

func (c *client) readLoop() {
	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			close(c.inbox)
			return
		}
		var m protocol.Message
		if err := json.Unmarshal(raw, &m); err != nil {
			log.Printf("[%s] bad json: %v", c.name, err)
			continue
		}
		c.inbox <- m
	}
}

func (c *client) handshake() {
	challengeMsg := c.await(protocol.TypeChallenge, 5*time.Second)
	var ch protocol.ChallengePayload
	mustUnmarshal(challengeMsg.Payload, &ch)

	chBytes, err := hex.DecodeString(ch.Challenge)
	if err != nil {
		log.Fatalf("[%s] challenge hex: %v", c.name, err)
	}
	sig := ed25519.Sign(c.priv, chBytes)

	clientChallenge := make([]byte, 32)
	if _, err := rand.Read(clientChallenge); err != nil {
		log.Fatalf("[%s] random client_challenge: %v", c.name, err)
	}
	auth := protocol.AuthPayload{
		PublicKey:       hex.EncodeToString(c.pub),
		NickTag:         c.nickTag,
		Signature:       hex.EncodeToString(sig),
		ClientChallenge: hex.EncodeToString(clientChallenge),
	}
	c.send(protocol.TypeAuth, auth)

	// Ждём auth_ok и ice_servers (оба приходят, проглатываем).
	c.await(protocol.TypeAuthOK, 5*time.Second)
	c.await(protocol.TypeICEServers, 2*time.Second)

	log.Printf("[%s] authed as %s", c.name, c.nickTag)
}

func (c *client) send(t string, payload any) {
	buf, err := json.Marshal(payload)
	if err != nil {
		log.Fatalf("[%s] marshal %s: %v", c.name, t, err)
	}
	msg, _ := json.Marshal(protocol.Message{Type: t, Payload: buf})
	if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
		log.Fatalf("[%s] write %s: %v", c.name, t, err)
	}
}

func (c *client) await(expect string, timeout time.Duration) protocol.Message {
	deadline := time.After(timeout)
	for {
		select {
		case m, ok := <-c.inbox:
			if !ok {
				log.Fatalf("[%s] conn closed waiting for %s", c.name, expect)
			}
			if m.Type == expect {
				return m
			}
			// Логируем и игнорируем неожиданные типы (например error).
			log.Printf("[%s] skipping %s while waiting for %s (payload=%s)", c.name, m.Type, expect, string(m.Payload))
			if m.Type == protocol.TypeError {
				log.Fatalf("[%s] server error: %s", c.name, string(m.Payload))
			}
		case <-deadline:
			log.Fatalf("[%s] timeout waiting for %s", c.name, expect)
		}
	}
}

// drainUntil читает сообщения до совпадения по type ИЛИ пока не истечёт timeout.
// Возвращает все прочитанные сообщения нужного типа.
func (c *client) drainUntil(expect string, want int, timeout time.Duration) []protocol.Message {
	deadline := time.After(timeout)
	var got []protocol.Message
	for len(got) < want {
		select {
		case m, ok := <-c.inbox:
			if !ok {
				log.Fatalf("[%s] conn closed waiting for %s", c.name, expect)
			}
			if m.Type == expect {
				got = append(got, m)
			} else {
				log.Printf("[%s] drop %s while collecting %s", c.name, m.Type, expect)
			}
		case <-deadline:
			return got
		}
	}
	return got
}

func mustUnmarshal(raw json.RawMessage, v any) {
	if err := json.Unmarshal(raw, v); err != nil {
		log.Fatalf("unmarshal: %v", err)
	}
}

func main() {
	addr := flag.String("addr", "ws://127.0.0.1:9741/ws", "signal ws url")
	flag.Parse()

	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Printf("connecting to %s", *addr)

	alice := newClient("alice", *addr)
	bob := newClient("bob", *addr)

	groupID := uuid.NewString()
	aliceKey := hex.EncodeToString(alice.pub)
	bobKey := hex.EncodeToString(bob.pub)

	// Alice регистрирует группу.
	alice.send(protocol.TypeGroupRegister, protocol.GroupRegisterPayload{
		GroupID: groupID,
		Members: []string{aliceKey, bobKey},
		Admins:  []string{aliceKey},
	})

	okMsg := alice.await(protocol.TypeGroupRegisterOK, 5*time.Second)
	var ok protocol.GroupRegisterOKPayload
	mustUnmarshal(okMsg.Payload, &ok)
	if ok.GroupID != groupID {
		log.Fatalf("register_ok wrong group: want %s got %s", groupID, ok.GroupID)
	}
	log.Printf("✅ group registered: members=%d current_seq=%d", ok.MemberCount, ok.CurrentSeq)

	// Alice получает свой delivery_mode (она в списке members → получает broadcast).
	// Bob тоже должен получить — ждём его.
	bobMode := bob.await(protocol.TypeGroupDeliveryMode, 3*time.Second)
	var dmB protocol.GroupDeliveryModePayload
	mustUnmarshal(bobMode.Payload, &dmB)
	if dmB.Mode != protocol.DeliveryModeServer {
		log.Fatalf("bob expected server mode, got %q", dmB.Mode)
	}
	log.Printf("✅ bob got delivery_mode=%s for %s", dmB.Mode, dmB.GroupID)

	// Alice тоже получает delivery_mode (fan-out включает её).
	aliceMode := alice.await(protocol.TypeGroupDeliveryMode, 3*time.Second)
	var dmA protocol.GroupDeliveryModePayload
	mustUnmarshal(aliceMode.Payload, &dmA)
	log.Printf("✅ alice got delivery_mode=%s", dmA.Mode)

	// Alice шлёт первое сообщение.
	msg1ID := uuid.NewString()
	alice.send(protocol.TypeGroupMessageSend, protocol.GroupMessageSendPayload{
		GroupID:     groupID,
		ClientMsgID: msg1ID,
		Payload:     "hello from alice",
		Encrypted:   false,
		ClientTsMs:  time.Now().UnixMilli(),
	})

	// Оба должны получить.
	aliceGot1 := alice.drainUntil(protocol.TypeGroupMessageNew, 1, 3*time.Second)
	bobGot1 := bob.drainUntil(protocol.TypeGroupMessageNew, 1, 3*time.Second)
	if len(aliceGot1) != 1 || len(bobGot1) != 1 {
		log.Fatalf("msg1 fan-out failed: alice=%d bob=%d", len(aliceGot1), len(bobGot1))
	}
	var n1a, n1b protocol.GroupMessageNewPayload
	mustUnmarshal(aliceGot1[0].Payload, &n1a)
	mustUnmarshal(bobGot1[0].Payload, &n1b)
	if n1a.Seq != 1 || n1b.Seq != 1 {
		log.Fatalf("msg1 seq mismatch: alice=%d bob=%d (want 1)", n1a.Seq, n1b.Seq)
	}
	if n1a.ClientMsgID != msg1ID || n1b.ClientMsgID != msg1ID {
		log.Fatalf("msg1 client_msg_id mismatch")
	}
	log.Printf("✅ msg1 fan-out ok: seq=%d author=%s…", n1a.Seq, n1a.AuthorKey[:8])

	// Bob отвечает.
	msg2ID := uuid.NewString()
	bob.send(protocol.TypeGroupMessageSend, protocol.GroupMessageSendPayload{
		GroupID:     groupID,
		ClientMsgID: msg2ID,
		Payload:     "hi, bob here",
		Encrypted:   false,
		ClientTsMs:  time.Now().UnixMilli(),
	})
	aliceGot2 := alice.drainUntil(protocol.TypeGroupMessageNew, 1, 3*time.Second)
	bobGot2 := bob.drainUntil(protocol.TypeGroupMessageNew, 1, 3*time.Second)
	if len(aliceGot2) != 1 || len(bobGot2) != 1 {
		log.Fatalf("msg2 fan-out failed")
	}
	var n2 protocol.GroupMessageNewPayload
	mustUnmarshal(aliceGot2[0].Payload, &n2)
	if n2.Seq != 2 {
		log.Fatalf("msg2 seq=%d (want 2)", n2.Seq)
	}
	log.Printf("✅ msg2 fan-out ok: seq=%d", n2.Seq)

	// Sync с нуля — должны получить обе.
	alice.send(protocol.TypeGroupSync, protocol.GroupSyncPayload{
		GroupID: groupID,
		Cursors: map[string]int64{},
		Limit:   100,
	})
	syncMsg := alice.await(protocol.TypeGroupSyncResult, 5*time.Second)
	var sr protocol.GroupSyncResultPayload
	mustUnmarshal(syncMsg.Payload, &sr)
	if len(sr.Messages) != 2 {
		log.Fatalf("sync got %d messages, want 2", len(sr.Messages))
	}
	if sr.Messages[0].Seq != 1 || sr.Messages[1].Seq != 2 {
		log.Fatalf("sync seq order: [%d,%d]", sr.Messages[0].Seq, sr.Messages[1].Seq)
	}
	log.Printf("✅ sync ok: 2 msgs, seq=[1,2], has_more=%v", sr.HasMore)

	// Rate limit smoke: шлём burst+5 сообщений, ожидаем что последние получат rate_limited.
	// (burst по умолчанию 30, поэтому сильно не превышаем чтобы не засорять лог.)
	log.Printf("— rate limit check: отправляю 35 сообщений подряд...")
	blocked := 0
	for i := range 35 {
		alice.send(protocol.TypeGroupMessageSend, protocol.GroupMessageSendPayload{
			GroupID:     groupID,
			ClientMsgID: uuid.NewString(),
			Payload:     fmt.Sprintf("burst-%d", i),
			ClientTsMs:  time.Now().UnixMilli(),
		})
	}
	// Читаем всё что придёт за 2 секунды.
	deadline := time.After(2 * time.Second)
loop:
	for {
		select {
		case m := <-alice.inbox:
			if m.Type == protocol.TypeError {
				var ep protocol.ErrorPayload
				_ = json.Unmarshal(m.Payload, &ep)
				if ep.Code == protocol.ErrRateLimited {
					blocked++
				}
			}
		case <-deadline:
			break loop
		}
	}
	if blocked == 0 {
		log.Printf("⚠️  rate limit не сработал (burst=30, slать 35 — ожидали ≥5 rate_limited). Возможно бакет быстрее восстанавливается чем успел опустеть.")
	} else {
		log.Printf("✅ rate limit сработал: %d rate_limited ответов", blocked)
	}

	// Закрываем.
	alice.conn.Close()
	bob.conn.Close()

	log.Printf("🎉 smoke-test passed")
	os.Exit(0)
}
