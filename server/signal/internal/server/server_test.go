package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"messenger_signal/internal/config"
	"messenger_signal/internal/hub"
	"messenger_signal/internal/identity"
	"messenger_signal/pkg/protocol"
)

// setupTestServer создаёт тестовый сервер + hub
func setupTestServer(t *testing.T) (*httptest.Server, *hub.Hub, *identity.Identity) {
	t.Helper()
	log := slog.Default()
	h := hub.New(log)

	cfg := config.WSConfig{
		Host: "127.0.0.1",
		Port: 0,
		Path: "/ws",
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal("generate server identity:", err)
	}
	id := &identity.Identity{Public: pub, Private: priv}

	srv := New(h, cfg, config.TURNConfig{}, config.SFUConfig{}, log, id)

	ts := httptest.NewServer(nil)
	// Подменяем handler на наш WebSocket
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", srv.handleWS)
	mux.HandleFunc("/health", srv.handleHealth)
	ts.Config.Handler = mux

	return ts, h, id
}

// connectAndAuth подключается к WS и проходит mutual auth.
// Если expectedServerPubkey != nil — проверяет server_signature над client_challenge.
func connectAndAuth(t *testing.T, wsURL string, nickTag string, expectedServerPubkey ed25519.PublicKey) (*websocket.Conn, ed25519.PublicKey) {
	t.Helper()

	// Генерируем ключи клиента
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal("generate key:", err)
	}
	return connectAndAuthKey(t, wsURL, nickTag, expectedServerPubkey, pub, priv, nil), pub
}

// connectAndAuthKey — то же с заданными ключами и watch-списком (presence);
// pres == nil — старый клиент без списка.
func connectAndAuthKey(t *testing.T, wsURL, nickTag string, expectedServerPubkey ed25519.PublicKey,
	pub ed25519.PublicKey, priv ed25519.PrivateKey, pres *protocol.PresencePayload) *websocket.Conn {
	t.Helper()

	// Подключаемся
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal("dial:", err)
	}

	// Читаем challenge (содержит server_pubkey)
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatal("read challenge:", err)
	}

	var challengeMsg protocol.Message
	if err := json.Unmarshal(raw, &challengeMsg); err != nil {
		t.Fatal("parse challenge:", err)
	}
	if challengeMsg.Type != protocol.TypeChallenge {
		t.Fatalf("expected challenge, got %s", challengeMsg.Type)
	}

	var challengePayload protocol.ChallengePayload
	json.Unmarshal(challengeMsg.Payload, &challengePayload)

	if expectedServerPubkey != nil {
		gotPub, _ := hex.DecodeString(challengePayload.ServerPubkey)
		if !ed25519.PublicKey(gotPub).Equal(expectedServerPubkey) {
			t.Fatalf("server_pubkey mismatch: got %s", challengePayload.ServerPubkey)
		}
	}

	// Подписываем server-side challenge
	challengeBytes, _ := hex.DecodeString(challengePayload.Challenge)
	signature := ed25519.Sign(priv, challengeBytes)

	// Свой client_challenge для mutual auth
	clientChallenge := make([]byte, 32)
	if _, err := rand.Read(clientChallenge); err != nil {
		t.Fatal("random client challenge:", err)
	}

	authPayload, _ := json.Marshal(protocol.AuthPayload{
		PublicKey:       hex.EncodeToString(pub),
		NickTag:         nickTag,
		Signature:       hex.EncodeToString(signature),
		ClientChallenge: hex.EncodeToString(clientChallenge),
		Presence:        pres,
	})
	authMsg, _ := json.Marshal(protocol.Message{
		Type:    protocol.TypeAuth,
		Payload: authPayload,
	})
	if err := conn.WriteMessage(websocket.TextMessage, authMsg); err != nil {
		t.Fatal("send auth:", err)
	}

	// Читаем auth_ok + проверяем server_signature
	_, raw, err = conn.ReadMessage()
	if err != nil {
		t.Fatal("read auth_ok:", err)
	}

	var okMsg protocol.Message
	json.Unmarshal(raw, &okMsg)
	if okMsg.Type != protocol.TypeAuthOK {
		t.Fatalf("expected auth_ok, got %s", okMsg.Type)
	}

	if expectedServerPubkey != nil {
		var okPayload protocol.AuthOKPayload
		if err := json.Unmarshal(okMsg.Payload, &okPayload); err != nil {
			t.Fatal("parse auth_ok payload:", err)
		}
		serverSig, err := hex.DecodeString(okPayload.ServerSignature)
		if err != nil {
			t.Fatal("decode server_signature:", err)
		}
		if !ed25519.Verify(expectedServerPubkey, clientChallenge, serverSig) {
			t.Fatal("server_signature does not verify")
		}
		if !okPayload.Presence {
			t.Fatal("auth_ok.presence = false, сервер должен объявлять watch-списки")
		}
	}

	// Читаем ice_servers (всегда отправляется после auth_ok)
	_, raw, err = conn.ReadMessage()
	if err != nil {
		t.Fatal("read ice_servers:", err)
	}
	var iceMsg protocol.Message
	json.Unmarshal(raw, &iceMsg)
	if iceMsg.Type != protocol.TypeICEServers {
		t.Fatalf("expected ice_servers, got %s", iceMsg.Type)
	}

	return conn
}

func TestAuthSuccess(t *testing.T) {
	ts, h, srvID := setupTestServer(t)
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	conn, _ := connectAndAuth(t, wsURL, "alice#1234567890abcdef", srvID.Public)
	defer conn.Close()

	// Даём серверу время зарегистрировать пира
	time.Sleep(50 * time.Millisecond)

	// Проверяем что пир зарегистрирован в hub
	if h.Count() != 1 {
		t.Fatalf("expected 1 peer in hub, got %d", h.Count())
	}
}

func TestAuthBadSignature(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	// Генерируем ключи
	pub, _, _ := ed25519.GenerateKey(rand.Reader)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal("dial:", err)
	}
	defer conn.Close()

	// Читаем challenge
	_, raw, _ := conn.ReadMessage()
	var challengeMsg protocol.Message
	json.Unmarshal(raw, &challengeMsg)

	// Отправляем auth с поддельной подписью (client_challenge валидный, чтобы
	// сервер дошёл именно до проверки подписи клиента, а не до парсинга nonce)
	fakeSignature := make([]byte, ed25519.SignatureSize)
	clientChallenge := make([]byte, 32)
	rand.Read(clientChallenge)
	authPayload, _ := json.Marshal(protocol.AuthPayload{
		PublicKey:       hex.EncodeToString(pub),
		NickTag:         "evil#0000000000000000",
		Signature:       hex.EncodeToString(fakeSignature),
		ClientChallenge: hex.EncodeToString(clientChallenge),
	})
	authMsg, _ := json.Marshal(protocol.Message{
		Type:    protocol.TypeAuth,
		Payload: authPayload,
	})
	conn.WriteMessage(websocket.TextMessage, authMsg)

	// Должны получить ошибку
	_, raw, err = conn.ReadMessage()
	if err != nil {
		// Соединение закрыто — тоже ок
		return
	}

	var errMsg protocol.Message
	json.Unmarshal(raw, &errMsg)
	if errMsg.Type != protocol.TypeError {
		t.Fatalf("expected error, got %s", errMsg.Type)
	}
}

// TestAuthMissingClientChallenge проверяет что сервер отвергает auth без client_challenge.
func TestAuthMissingClientChallenge(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal("dial:", err)
	}
	defer conn.Close()

	_, raw, _ := conn.ReadMessage()
	var challengeMsg protocol.Message
	json.Unmarshal(raw, &challengeMsg)
	var challengePayload protocol.ChallengePayload
	json.Unmarshal(challengeMsg.Payload, &challengePayload)
	challengeBytes, _ := hex.DecodeString(challengePayload.Challenge)
	signature := ed25519.Sign(priv, challengeBytes)

	// Auth без client_challenge — должен быть отвергнут сервером.
	authPayload, _ := json.Marshal(protocol.AuthPayload{
		PublicKey: hex.EncodeToString(pub),
		NickTag:   "alice#1234567890abcdef",
		Signature: hex.EncodeToString(signature),
	})
	authMsg, _ := json.Marshal(protocol.Message{Type: protocol.TypeAuth, Payload: authPayload})
	conn.WriteMessage(websocket.TextMessage, authMsg)

	_, raw, err = conn.ReadMessage()
	if err != nil {
		return // closed = ok
	}
	var errMsg protocol.Message
	json.Unmarshal(raw, &errMsg)
	if errMsg.Type != protocol.TypeError {
		t.Fatalf("expected error for missing client_challenge, got %s", errMsg.Type)
	}
}

func TestSDPRelay(t *testing.T) {
	ts, _, srvID := setupTestServer(t)
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	// Два пира подключаются
	connA, _ := connectAndAuth(t, wsURL, "alice#1234567890abcdef", srvID.Public)
	defer connA.Close()

	connB, pubB := connectAndAuth(t, wsURL, "bob#abcdef1234567890", srvID.Public)
	defer connB.Close()

	// A отправляет offer к B
	sdpPayload := json.RawMessage(`"v=0\r\no=- 123 456 IN IP4 0.0.0.0\r\n"`)
	offerMsg, _ := json.Marshal(protocol.Message{
		Type:    protocol.TypeOffer,
		To:      hex.EncodeToString(pubB),
		Payload: sdpPayload,
	})
	if err := connA.WriteMessage(websocket.TextMessage, offerMsg); err != nil {
		t.Fatal("send offer:", err)
	}

	// B должен получить offer с заполненным From
	connB.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, raw, err := connB.ReadMessage()
	if err != nil {
		t.Fatal("read offer on B:", err)
	}

	var received protocol.Message
	json.Unmarshal(raw, &received)

	if received.Type != protocol.TypeOffer {
		t.Fatalf("expected offer, got %s", received.Type)
	}
	if received.From == "" {
		t.Fatal("offer.From should be set by server")
	}
}

func TestPeerSearch(t *testing.T) {
	ts, _, srvID := setupTestServer(t)
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	conn, _ := connectAndAuth(t, wsURL, "charlie#aabbccddee112233", srvID.Public)
	defer conn.Close()

	// Поиск только точный: подстрока ника не находит (иначе поиск выдавал бы,
	// кто в сети), точный nick#tag и регистр — находят.
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"charlie", 0},
		{"char", 0},
		{"charlie#aabbccddee112233", 1},
		{"CHARLIE#AABBCCDDEE112233", 1},
	} {
		searchPayload, _ := json.Marshal(protocol.PeerSearchPayload{Query: tc.query})
		searchMsg, _ := json.Marshal(protocol.Message{
			Type:    protocol.TypePeerSearch,
			Payload: searchPayload,
		})
		conn.WriteMessage(websocket.TextMessage, searchMsg)

		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatal("read search result:", err)
		}
		var result protocol.Message
		json.Unmarshal(raw, &result)
		if result.Type != protocol.TypePeerResult {
			t.Fatalf("expected peer_result, got %s", result.Type)
		}
		var peers protocol.PeerResultPayload
		json.Unmarshal(result.Payload, &peers)
		if len(peers.Peers) != tc.want {
			t.Fatalf("query %q: expected %d peers, got %d", tc.query, tc.want, len(peers.Peers))
		}
		if tc.want == 1 && peers.Peers[0].NickTag != "charlie#aabbccddee112233" {
			t.Fatalf("unexpected nick_tag: %s", peers.Peers[0].NickTag)
		}
	}
}

// --- статус «в сети» только для watch-списка ---

// pump читает сообщения соединения в канал: у gorilla после таймаута чтения
// соединение негодно, поэтому читаем в горутине и собираем с таймером.
func pump(conn *websocket.Conn) <-chan protocol.Message {
	ch := make(chan protocol.Message, 64)
	go func() {
		defer close(ch)
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var m protocol.Message
			if json.Unmarshal(raw, &m) == nil {
				ch <- m
			}
		}
	}()
	return ch
}

func collect(ch <-chan protocol.Message, d time.Duration) []protocol.Message {
	var out []protocol.Message
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case m, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, m)
		case <-timer.C:
			return out
		}
	}
}

func hasStatus(ms []protocol.Message, typ string, from ed25519.PublicKey) bool {
	for _, m := range ms {
		if m.Type == typ && m.From == hex.EncodeToString(from) {
			return true
		}
	}
	return false
}

func hasErrCode(ms []protocol.Message, code string) bool {
	for _, m := range ms {
		if m.Type != protocol.TypeError {
			continue
		}
		var p protocol.ErrorPayload
		if json.Unmarshal(m.Payload, &p) == nil && p.Code == code {
			return true
		}
	}
	return false
}

func genKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func watch(keys ...ed25519.PublicKey) *protocol.PresencePayload {
	p := &protocol.PresencePayload{Watch: []string{}}
	for _, k := range keys {
		p.Watch = append(p.Watch, hex.EncodeToString(k))
	}
	return p
}

func sendMsg(t *testing.T, conn *websocket.Conn, typ string, to ed25519.PublicKey, payload any) {
	t.Helper()
	var raw json.RawMessage
	if payload != nil {
		raw, _ = json.Marshal(payload)
	}
	m := protocol.Message{Type: typ, Payload: raw}
	if to != nil {
		m.To = hex.EncodeToString(to)
	}
	data, _ := json.Marshal(m)
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatal("send:", err)
	}
}

// A разрешил статус только B. B узнаёт о входе и выходе A, незнакомец C и
// старый клиент L — нет. Статус старого L по-прежнему виден всем.
// «Не в сети» тоже статус: C на offer к ушедшему A ошибки не получает, B — получает.
func TestPresenceVisibility(t *testing.T) {
	ts, _, srvID := setupTestServer(t)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	pubA, privA := genKey(t)
	pubB, privB := genKey(t)
	pubC, privC := genKey(t)
	pubL, privL := genKey(t)

	connB := connectAndAuthKey(t, wsURL, "bob#b", srvID.Public, pubB, privB, watch(pubA))
	defer connB.Close()
	chB := pump(connB)
	connC := connectAndAuthKey(t, wsURL, "carol#c", srvID.Public, pubC, privC, watch())
	defer connC.Close()
	chC := pump(connC)
	connL := connectAndAuthKey(t, wsURL, "legacy#l", srvID.Public, pubL, privL, nil)
	defer connL.Close()
	chL := pump(connL)

	early := collect(chB, 300*time.Millisecond)
	if !hasStatus(early, protocol.TypePeerOnline, pubL) {
		t.Fatal("статус старого клиента L должен быть виден всем")
	}
	collect(chC, 100*time.Millisecond)

	connA := connectAndAuthKey(t, wsURL, "alice#a", srvID.Public, pubA, privA, watch(pubB))
	chA := pump(connA)

	gotB := collect(chB, 400*time.Millisecond)
	gotC := collect(chC, 100*time.Millisecond)
	gotL := collect(chL, 100*time.Millisecond)
	if !hasStatus(gotB, protocol.TypePeerOnline, pubA) {
		t.Fatal("B (в списке A) не получил peer_online A")
	}
	if hasStatus(gotC, protocol.TypePeerOnline, pubA) || hasStatus(gotL, protocol.TypePeerOnline, pubA) {
		t.Fatal("peer_online A ушёл тем, кого нет в его списке")
	}
	_ = chA

	connA.Close()
	gotB = collect(chB, 500*time.Millisecond)
	gotC = collect(chC, 100*time.Millisecond)
	gotL = collect(chL, 100*time.Millisecond)
	if !hasStatus(gotB, protocol.TypePeerOffline, pubA) {
		t.Fatal("B не получил peer_offline A")
	}
	if hasStatus(gotC, protocol.TypePeerOffline, pubA) || hasStatus(gotL, protocol.TypePeerOffline, pubA) {
		t.Fatal("peer_offline A ушёл тем, кого нет в его списке")
	}

	sendMsg(t, connC, protocol.TypeOffer, pubA, "sdp")
	if hasErrCode(collect(chC, 300*time.Millisecond), protocol.ErrPeerOffline) {
		t.Fatal("незнакомец C узнал через peer_offline_err, что A не в сети")
	}
	sendMsg(t, connB, protocol.TypeOffer, pubA, "sdp")
	if !hasErrCode(collect(chB, 500*time.Millisecond), protocol.ErrPeerOffline) {
		t.Fatal("B (в списке A) должен получить peer_offline_err")
	}

	// Доставка не меняется: заявка незнакомца онлайн-адресату доходит.
	connA2 := connectAndAuthKey(t, wsURL, "alice#a", srvID.Public, pubA, privA, watch(pubB))
	defer connA2.Close()
	chA2 := pump(connA2)
	collect(chC, 200*time.Millisecond)
	sendMsg(t, connC, protocol.TypeFriendRequest, pubA, protocol.FriendRequestPayload{NickTag: "carol#c"})
	gotA := collect(chA2, 500*time.Millisecond)
	found := false
	for _, m := range gotA {
		if m.Type == protocol.TypeFriendRequest && m.From == hex.EncodeToString(pubC) {
			found = true
		}
	}
	if !found {
		t.Fatal("заявка в друзья от незнакомца не доставлена")
	}
}

// presence_watch меняет список на ходу: добавленный получает peer_online,
// убранный — peer_offline.
func TestPresenceWatchUpdate(t *testing.T) {
	ts, _, srvID := setupTestServer(t)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	pubA, privA := genKey(t)
	pubC, privC := genKey(t)

	connC := connectAndAuthKey(t, wsURL, "carol#c", srvID.Public, pubC, privC, nil)
	defer connC.Close()
	chC := pump(connC)
	connA := connectAndAuthKey(t, wsURL, "alice#a", srvID.Public, pubA, privA, watch())
	defer connA.Close()
	pump(connA)

	if hasStatus(collect(chC, 300*time.Millisecond), protocol.TypePeerOnline, pubA) {
		t.Fatal("C узнал о входе A до того, как A его разрешил")
	}

	sendMsg(t, connA, protocol.TypePresenceWatch, nil, watch(pubC))
	if !hasStatus(collect(chC, 500*time.Millisecond), protocol.TypePeerOnline, pubA) {
		t.Fatal("C, добавленный в список, не получил peer_online A")
	}

	sendMsg(t, connA, protocol.TypePresenceWatch, nil, watch())
	if !hasStatus(collect(chC, 500*time.Millisecond), protocol.TypePeerOffline, pubA) {
		t.Fatal("C, убранный из списка, не получил peer_offline A")
	}
}

func TestHealth(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal("health request:", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", body["status"])
	}
}

func TestFriendRequestRelay(t *testing.T) {
	ts, _, srvID := setupTestServer(t)
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	// Два пира подключаются
	connA, _ := connectAndAuth(t, wsURL, "alice#1234567890abcdef", srvID.Public)
	defer connA.Close()

	connB, pubB := connectAndAuth(t, wsURL, "bob#abcdef1234567890", srvID.Public)
	defer connB.Close()

	// A отправляет friend_request к B
	frPayload, _ := json.Marshal(protocol.FriendRequestPayload{NickTag: "alice#1234567890abcdef"})
	frMsg, _ := json.Marshal(protocol.Message{
		Type:    protocol.TypeFriendRequest,
		To:      hex.EncodeToString(pubB),
		Payload: frPayload,
	})
	if err := connA.WriteMessage(websocket.TextMessage, frMsg); err != nil {
		t.Fatal("send friend_request:", err)
	}

	// B должен получить friend_request с заполненным From
	connB.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, raw, err := connB.ReadMessage()
	if err != nil {
		t.Fatal("read friend_request on B:", err)
	}

	var received protocol.Message
	json.Unmarshal(raw, &received)

	if received.Type != protocol.TypeFriendRequest {
		t.Fatalf("expected friend_request, got %s", received.Type)
	}
	if received.From == "" {
		t.Fatal("friend_request.From should be set by server")
	}

	// Проверяем payload
	var frp protocol.FriendRequestPayload
	json.Unmarshal(received.Payload, &frp)
	if frp.NickTag != "alice#1234567890abcdef" {
		t.Fatalf("unexpected nick_tag: %s", frp.NickTag)
	}
}
