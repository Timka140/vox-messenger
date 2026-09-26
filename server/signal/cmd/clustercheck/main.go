// Живая проверка кластера на двух узлах: объявление delivery_mode между
// узлами, досылка режима при подключении, догон истории по курсорам и запрет
// перезаписи состава не-админом.
//
//	go run ./cmd/clustercheck --a=ws://n1.northwindcdn.com:9741/ws --b=ws://n2.northwindcdn.com:9741/ws
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gorilla/websocket"

	"messenger_signal/pkg/protocol"
)

type client struct {
	name  string
	pub   ed25519.PublicKey
	priv  ed25519.PrivateKey
	conn  *websocket.Conn
	inbox chan protocol.Message
}

func (c *client) hex() string { return hex.EncodeToString(c.pub) }

func dial(name, addr string, pub ed25519.PublicKey, priv ed25519.PrivateKey) *client {
	conn, _, err := websocket.DefaultDialer.Dial(addr, nil)
	if err != nil {
		log.Fatalf("[%s] dial %s: %v", name, addr, err)
	}
	c := &client{name: name, pub: pub, priv: priv, conn: conn, inbox: make(chan protocol.Message, 512)}

	var ch protocol.Message
	if err := conn.ReadJSON(&ch); err != nil || ch.Type != protocol.TypeChallenge {
		log.Fatalf("[%s] challenge: %v %s", name, err, ch.Type)
	}
	var cp protocol.ChallengePayload
	_ = json.Unmarshal(ch.Payload, &cp)
	chBytes, _ := hex.DecodeString(cp.Challenge)
	cc := make([]byte, 32)
	_, _ = rand.Read(cc)
	ap, _ := json.Marshal(protocol.AuthPayload{
		PublicKey:       c.hex(),
		NickTag:         fmt.Sprintf("%s#%s", name, c.hex()[:16]),
		Signature:       hex.EncodeToString(ed25519.Sign(priv, chBytes)),
		ClientChallenge: hex.EncodeToString(cc),
	})
	if err := conn.WriteJSON(protocol.Message{Type: protocol.TypeAuth, Payload: ap}); err != nil {
		log.Fatalf("[%s] auth: %v", name, err)
	}
	var ok protocol.Message
	if err := conn.ReadJSON(&ok); err != nil || ok.Type != protocol.TypeAuthOK {
		log.Fatalf("[%s] auth_ok: %v %s", name, err, ok.Type)
	}
	go func() {
		defer close(c.inbox)
		for {
			var m protocol.Message
			if err := conn.ReadJSON(&m); err != nil {
				return
			}
			c.inbox <- m
		}
	}()
	return c
}

func (c *client) send(typ string, payload any) {
	raw, _ := json.Marshal(payload)
	if err := c.conn.WriteJSON(protocol.Message{Type: typ, Payload: raw}); err != nil {
		log.Fatalf("[%s] send %s: %v", c.name, typ, err)
	}
}

// wait возвращает подходящее сообщение либо (zero, false) по таймауту.
func (c *client) wait(what string, d time.Duration, pred func(protocol.Message) bool) (protocol.Message, bool) {
	deadline := time.After(d)
	for {
		select {
		case m, ok := <-c.inbox:
			if !ok {
				fail("[%s] соединение закрыто, не дождались: %s", c.name, what)
				return protocol.Message{}, false
			}
			if m.Type == protocol.TypeError {
				var e protocol.ErrorPayload
				_ = json.Unmarshal(m.Payload, &e)
				fmt.Printf("  · [%s] ошибка сервера: %s %s\n", c.name, e.Code, e.Message)
			}
			if pred(m) {
				return m, true
			}
		case <-deadline:
			fail("[%s] не дождались: %s", c.name, what)
			return protocol.Message{}, false
		}
	}
}

var failed bool

func fail(format string, args ...any) {
	failed = true
	fmt.Printf("  ✗ "+format+"\n", args...)
}

func okf(format string, args ...any) { fmt.Printf("  ✓ "+format+"\n", args...) }

func isType(t string) func(protocol.Message) bool {
	return func(m protocol.Message) bool { return m.Type == t }
}

func isMode(gid string) func(protocol.Message) bool {
	return func(m protocol.Message) bool {
		if m.Type != protocol.TypeGroupDeliveryMode {
			return false
		}
		var p protocol.GroupDeliveryModePayload
		return json.Unmarshal(m.Payload, &p) == nil && p.GroupID == gid
	}
}

// catchUpDuringOutage — группа регистрируется через B, затем, как только A
// перестаёт отвечать на /health (узел остановлен, например выкладкой),
// через B уходят n сообщений. После подъёма A должен догнать их пачкой.
func catchUpDuringOutage(addrB, healthA string, n int) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	c := dial("catchup", addrB, pub, priv)
	gid := "clustercheck-cu-" + c.hex()[:10]
	c.send(protocol.TypeGroupRegister, protocol.GroupRegisterPayload{
		GroupID: gid, Members: []string{c.hex()}, Admins: []string{c.hex()},
	})
	if _, ok := c.wait("register_ok", 10*time.Second, isType(protocol.TypeGroupRegisterOK)); !ok {
		os.Exit(1)
	}
	fmt.Printf("группа %s зарегистрирована через B, жду остановки A (%s)\n", gid, healthA)

	hc := &http.Client{Timeout: time.Second}
	for deadline := time.Now().Add(5 * time.Minute); ; {
		if time.Now().After(deadline) {
			fmt.Println("A так и не остановился")
			os.Exit(1)
		}
		resp, err := hc.Get(healthA)
		if err != nil {
			break
		}
		resp.Body.Close()
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Printf("A не отвечает — шлю %d сообщений через B\n", n)
	for i := 1; i <= n; i++ {
		c.send(protocol.TypeGroupMessageSend, protocol.GroupMessageSendPayload{
			GroupID: gid, ClientMsgID: fmt.Sprintf("cu%03d", i), Payload: "догон", ClientTsMs: time.Now().UnixMilli(),
		})
	}
	if _, ok := c.wait("своя копия последнего", 10*time.Second, func(m protocol.Message) bool {
		var p protocol.GroupMessageNewPayload
		return m.Type == protocol.TypeGroupMessageNew &&
			json.Unmarshal(m.Payload, &p) == nil && p.ClientMsgID == fmt.Sprintf("cu%03d", n)
	}); ok {
		fmt.Printf("B принял все %d; в логе A после подъёма линка ищите «групповые сообщения догнаны» new=%d\n", n, n)
	}
}

func main() {
	addrA := flag.String("a", "ws://n1.northwindcdn.com:9741/ws", "узел A")
	addrB := flag.String("b", "ws://n2.northwindcdn.com:9741/ws", "узел B")
	count := flag.Int("n", 250, "сколько сообщений слать в офлайн")
	catchUp := flag.String("catchup", "", "режим догона: адрес /health узла A, например http://n1.northwindcdn.com:9741/health")
	flag.Parse()

	if *catchUp != "" {
		catchUpDuringOutage(*addrB, *catchUp, *count)
		return
	}

	pubA, privA, _ := ed25519.GenerateKey(rand.Reader)
	pubB, privB, _ := ed25519.GenerateKey(rand.Reader)
	pubC, _, _ := ed25519.GenerateKey(rand.Reader)
	keyC := hex.EncodeToString(pubC)

	a := dial("alice", *addrA, pubA, privA)
	b := dial("bob", *addrB, pubB, privB)
	gid := "clustercheck-" + a.hex()[:12]
	fmt.Printf("группа %s: alice на A, bob на B, третий участник офлайн\n", gid)

	// 1. Смена режима доходит до участника на другом узле.
	fmt.Println("\n1. delivery_mode между узлами")
	a.send(protocol.TypeGroupRegister, protocol.GroupRegisterPayload{
		GroupID: gid,
		Members: []string{a.hex(), b.hex(), keyC},
		Admins:  []string{a.hex()},
	})
	a.wait("register_ok", 10*time.Second, isType(protocol.TypeGroupRegisterOK))
	if _, got := b.wait("group_delivery_mode у bob на узле B", 10*time.Second, isMode(gid)); got {
		okf("участник на другом узле узнал о режиме server")
	}

	// 2. Офлайн-участник получает режим при подключении и догоняет историю.
	fmt.Printf("\n2. догон %d сообщений после переподключения\n", *count)
	_ = b.conn.Close()
	time.Sleep(1500 * time.Millisecond)

	// Сервер ограничивает частоту: burst 30, пополнение 5/с. Шлём медленнее
	// пополнения, иначе часть сообщений отобьётся с rate_limited.
	for i := 1; i <= *count; i++ {
		a.send(protocol.TypeGroupMessageSend, protocol.GroupMessageSendPayload{
			GroupID:     gid,
			ClientMsgID: fmt.Sprintf("m%04d", i),
			Payload:     fmt.Sprintf("сообщение %d", i),
			ClientTsMs:  time.Now().UnixMilli(),
		})
		time.Sleep(220 * time.Millisecond)
	}
	// Своя копия последнего — сервер принял всю пачку.
	if _, got := a.wait("своя копия последнего сообщения", 30*time.Second, func(m protocol.Message) bool {
		if m.Type != protocol.TypeGroupMessageNew {
			return false
		}
		var p protocol.GroupMessageNewPayload
		return json.Unmarshal(m.Payload, &p) == nil && p.ClientMsgID == fmt.Sprintf("m%04d", *count)
	}); got {
		okf("узел A принял все %d", *count)
	}

	b2 := dial("bob2", *addrB, pubB, privB)
	if _, got := b2.wait("group_delivery_mode при подключении", 10*time.Second, isMode(gid)); got {
		okf("офлайн-участник узнал о режиме сразу после auth")
	}

	// Постраничный догон по курсорам — как это делает клиент.
	cursors := map[string]int64{}
	seen := map[string]bool{}
	pages := 0
	for {
		b2.send(protocol.TypeGroupSync, protocol.GroupSyncPayload{
			GroupID: gid, Limit: 100, Cursors: cursors,
		})
		res, got := b2.wait("group_sync_result", 20*time.Second, isType(protocol.TypeGroupSyncResult))
		if !got {
			break
		}
		var sr protocol.GroupSyncResultPayload
		_ = json.Unmarshal(res.Payload, &sr)
		pages++
		for _, m := range sr.Messages {
			seen[m.ClientMsgID] = true
			if m.Seq > cursors[m.OriginNode] {
				cursors[m.OriginNode] = m.Seq
			}
		}
		if !sr.HasMore || len(sr.Messages) == 0 {
			break
		}
		if pages > 50 {
			fail("догон не сходится: %d страниц", pages)
			break
		}
	}
	missing := 0
	for i := 1; i <= *count; i++ {
		if !seen[fmt.Sprintf("m%04d", i)] {
			missing++
		}
	}
	if missing == 0 {
		okf("догнал все %d за %d страниц, дыр нет", *count, pages)
	} else {
		fail("потеряно сообщений: %d из %d (страниц %d)", missing, *count, pages)
	}

	// 3. Не-админ не переписывает состав.
	fmt.Println("\n3. перерегистрация не-админом")
	b2.send(protocol.TypeGroupRegister, protocol.GroupRegisterPayload{
		GroupID: gid, Members: []string{b2.hex()},
	})
	b2.wait("register_ok у не-админа", 10*time.Second, isType(protocol.TypeGroupRegisterOK))
	time.Sleep(2 * time.Second)

	// Проверяем через права: алиса всё ещё админ — её add не отбивается.
	for len(a.inbox) > 0 {
		<-a.inbox
	}
	a.send(protocol.TypeGroupMemberAdd, protocol.GroupMemberChangePayload{
		GroupID: gid, PublicKey: keyC,
	})
	deadline := time.After(3 * time.Second)
	rejected := false
drain:
	for {
		select {
		case m := <-a.inbox:
			if m.Type == protocol.TypeError {
				var e protocol.ErrorPayload
				_ = json.Unmarshal(m.Payload, &e)
				fail("алиса потеряла права администратора: %s %s", e.Code, e.Message)
				rejected = true
				break drain
			}
		case <-deadline:
			break drain
		}
	}
	if !rejected {
		okf("алиса осталась администратором, состав не переписан")
	}

	fmt.Println()
	if failed {
		fmt.Println("ПРОВЕРКА НЕ ПРОШЛА")
		os.Exit(1)
	}
	fmt.Println("ВСЁ ПРОШЛО")
}
