package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"messenger_signal/internal/config"
	"messenger_signal/internal/hub"
	"messenger_signal/internal/identity"
	"messenger_signal/internal/logstore"
	"messenger_signal/pkg/protocol"
)

// dialVersion — клиент с app_version в auth (пусто — старый клиент без поля).
// Возвращает соединение после ice_servers.
func dialVersion(t *testing.T, wsURL, nickTag, appVersion string) (*websocket.Conn, string) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	var ch struct {
		Payload protocol.ChallengePayload `json:"payload"`
	}
	if err := conn.ReadJSON(&ch); err != nil {
		t.Fatal(err)
	}
	cb, _ := hex.DecodeString(ch.Payload.Challenge)
	ap, _ := json.Marshal(protocol.AuthPayload{
		PublicKey:       hex.EncodeToString(pub),
		NickTag:         nickTag,
		Signature:       hex.EncodeToString(ed25519.Sign(priv, cb)),
		ClientChallenge: hex.EncodeToString([]byte("client-challenge")),
		AppVersion:      appVersion,
	})
	if err := conn.WriteJSON(protocol.Message{Type: protocol.TypeAuth, Payload: ap}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{protocol.TypeAuthOK, protocol.TypeICEServers} {
		var m protocol.Message
		if err := conn.ReadJSON(&m); err != nil || m.Type != want {
			t.Fatalf("want %s, got %s (%v)", want, m.Type, err)
		}
	}
	return conn, hex.EncodeToString(pub)
}

// logRequests — сколько log_request пришло в ch за d.
func logRequests(ch <-chan protocol.Message, d time.Duration) int {
	n := 0
	for _, m := range collect(ch, d) {
		if m.Type == protocol.TypeLogRequest {
			n++
		}
	}
	return n
}

func TestAdminLogsCollectOnlineVersions(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	h := hub.New(log)
	srv := New(h, config.WSConfig{Path: "/ws"}, config.TURNConfig{}, config.SFUConfig{}, log,
		&identity.Identity{Public: pub, Private: priv})

	base := t.TempDir()
	for _, src := range []string{"Alice#aa", "Bob#bb", "Gone#cc"} {
		if err := os.MkdirAll(filepath.Join(base, src), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, src, "2026-09-26T11-00-00.jsonl"), []byte(`{"level":"INFO","msg":"x"}`+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	srv.SetLogStore(logstore.New(base, logstore.Options{}, log), "")
	srv.SetAdmin(adminTestToken, nil)
	handler := srv.Handler()
	ts := httptest.NewServer(handler)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	alice, _ := dialVersion(t, wsURL, "Alice#aa", "v0.00.000.0082")
	defer alice.Close()
	bob, bobKey := dialVersion(t, wsURL, "Bob#bb", "")
	defer bob.Close()
	eve, _ := dialVersion(t, wsURL, "Eve#ee", "not-a-version")
	defer eve.Close()
	aliceCh, bobCh, eveCh := pump(alice), pump(bob), pump(eve)
	deadline := time.Now().Add(3 * time.Second)
	for h.Count() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	// status: client_versions по онлайн-клиентам, неверная версия — unknown.
	w := signedReq(handler, "GET", "/admin/v1/status", nil)
	var st struct {
		ClientVersions map[string]int `json:"client_versions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil || w.Code != 200 {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
	if st.ClientVersions["v0.00.000.0082"] != 1 || st.ClientVersions["unknown"] != 2 || len(st.ClientVersions) != 2 {
		t.Fatalf("client_versions %v", st.ClientVersions)
	}

	// sources: online по nick#tag подключённых клиентов.
	w = signedReq(handler, "GET", "/admin/v1/logs/sources", nil)
	var src struct {
		Sources []struct {
			Name   string `json:"name"`
			Online *bool  `json:"online"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &src); err != nil || len(src.Sources) != 3 {
		t.Fatalf("sources %d %s", w.Code, w.Body)
	}
	for _, s := range src.Sources {
		want := s.Name != "Gone#cc"
		if s.Online == nil || *s.Online != want {
			t.Fatalf("%s online %v", s.Name, s.Online)
		}
	}

	// collect адресно: только Alice (по nick#tag) и Bob (по ключу).
	w = signedReq(handler, "POST", "/admin/v1/logs/collect", []byte(`{"sources":["Alice#aa","`+bobKey+`","Nobody#zz"]}`))
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"sent":2}` {
		t.Fatalf("collect targeted %d %s", w.Code, w.Body)
	}
	if a, b, e := logRequests(aliceCh, 300*time.Millisecond), logRequests(bobCh, 300*time.Millisecond), logRequests(eveCh, 300*time.Millisecond); a != 1 || b != 1 || e != 0 {
		t.Fatalf("log_request: alice %d bob %d eve %d", a, b, e)
	}

	// collect без тела — всем.
	w = signedReq(handler, "POST", "/admin/v1/logs/collect", nil)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"sent":3}` {
		t.Fatalf("collect all %d %s", w.Code, w.Body)
	}
	if a, b, e := logRequests(aliceCh, 300*time.Millisecond), logRequests(bobCh, 300*time.Millisecond), logRequests(eveCh, 300*time.Millisecond); a != 1 || b != 1 || e != 1 {
		t.Fatalf("log_request всем: alice %d bob %d eve %d", a, b, e)
	}

	for _, bad := range []string{
		`{"sources":"x"}`,
		`nope`,
		`{"sources":[""]}`,
		`{"sources":["` + strings.Repeat("a", 101) + `"]}`,
		`{"sources":[` + strings.TrimSuffix(strings.Repeat(`"a",`, 201), ",") + `]}`,
	} {
		if w := signedReq(handler, "POST", "/admin/v1/logs/collect", []byte(bad)); w.Code != 400 {
			t.Fatalf("%.40s: code %d", bad, w.Code)
		}
	}
}

func TestValidAppVersion(t *testing.T) {
	for _, v := range []string{"v0.00.000.0082", "v1.23.456.7890", "v12.00.000.0001"} {
		if !protocol.ValidAppVersion(v) {
			t.Fatalf("%q rejected", v)
		}
	}
	for _, v := range []string{"", "0.00.000.0082", "v0.0.000.0082", "v0.00.000.00820", "v0.00.000.0082\n", "v" + strings.Repeat("1", 30) + ".00.000.0001"} {
		if protocol.ValidAppVersion(v) {
			t.Fatalf("%q accepted", v)
		}
	}
}

// TestLogPushAnonSource — log_push принимается только под анонимным id;
// по нему же работают «в сети» и адресный collect.
func TestLogPushAnonSource(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	h := hub.New(log)
	srv := New(h, config.WSConfig{Path: "/ws"}, config.TURNConfig{}, config.SFUConfig{}, log,
		&identity.Identity{Public: pub, Private: priv})
	base := t.TempDir()
	srv.SetLogStore(logstore.New(base, logstore.Options{}, log), "")
	srv.SetAdmin(adminTestToken, nil)
	handler := srv.Handler()
	ts := httptest.NewServer(handler)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	alice, _ := dialVersion(t, wsURL, "Alice#aa", "")
	defer alice.Close()
	aliceCh := pump(alice)

	push := func(source string) {
		p, _ := json.Marshal(protocol.LogPushPayload{Source: source, ChunkIdx: 0, ChunkTotal: 1,
			Data: base64.StdEncoding.EncodeToString([]byte(`{"level":"INFO","msg":"x"}` + "\n"))})
		if err := alice.WriteJSON(protocol.Message{Type: protocol.TypeLogPush, Payload: p}); err != nil {
			t.Fatal(err)
		}
	}
	const id = "anon-0123456789abcdef"
	push("Alice#aa")
	push(id)
	deadline := time.Now().Add(3 * time.Second)
	for !exists(filepath.Join(base, id)) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	entries, _ := os.ReadDir(base)
	if len(entries) != 1 || entries[0].Name() != id {
		t.Fatalf("каталоги логов: %v", entries)
	}

	w := signedReq(handler, "GET", "/admin/v1/logs/sources", nil)
	if !strings.Contains(w.Body.String(), `"name":"`+id+`"`) || !strings.Contains(w.Body.String(), `"online":true`) {
		t.Fatalf("sources %s", w.Body)
	}
	w = signedReq(handler, "POST", "/admin/v1/logs/collect", []byte(`{"sources":["`+id+`"]}`))
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"sent":1}` {
		t.Fatalf("collect по id %d %s", w.Code, w.Body)
	}
	if n := logRequests(aliceCh, 300*time.Millisecond); n != 1 {
		t.Fatalf("log_request %d", n)
	}
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
