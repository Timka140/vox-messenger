package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"messenger_signal/internal/auth"
	"messenger_signal/internal/config"
	"messenger_signal/internal/hub"
	"messenger_signal/internal/identity"
	"messenger_signal/internal/tlscert"
	"messenger_signal/pkg/protocol"
)

// v2Server — сервер с TLS-сертификатом; tlsOn — отдавать через TLS.
func v2Server(t *testing.T, tlsOn bool) (*httptest.Server, *identity.Identity, []byte) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	id := &identity.Identity{Public: pub, Private: priv}
	srv := New(hub.New(log), config.WSConfig{Path: "/ws"}, config.TURNConfig{},
		config.SFUConfig{Endpoint: "ws://sfu:9760/ws", TLSEndpoint: "wss://sfu:9762/ws", Secret: "s"}, log, id)
	dir := t.TempDir()
	cert, hash, err := tlscert.LoadOrCreate(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), "test")
	if err != nil {
		t.Fatal(err)
	}
	srv.SetTLS(cert, hash)
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", srv.handleWS)
	ts := httptest.NewUnstartedServer(mux)
	if tlsOn {
		ts.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
		ts.StartTLS()
	} else {
		ts.Start()
	}
	t.Cleanup(ts.Close)
	return ts, id, hash
}

type v2Result struct {
	ok      protocol.AuthOKPayload
	errMsg  string
	tlsHash []byte
	cc      []byte
	clientK ed25519.PublicKey
}

// v2Login — вход клиента; version 0 — старый клиент, cc — свой client_challenge.
func v2Login(t *testing.T, ts *httptest.Server, version int, cc []byte) v2Result {
	t.Helper()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	d := *websocket.DefaultDialer
	d.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	conn, _, err := d.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	var res v2Result
	if tc, ok := conn.UnderlyingConn().(*tls.Conn); ok {
		st := tc.ConnectionState()
		res.tlsHash = tlscert.PeerHash(&st)
	}
	var ch protocol.Message
	if err := conn.ReadJSON(&ch); err != nil {
		t.Fatal(err)
	}
	var cp protocol.ChallengePayload
	_ = json.Unmarshal(ch.Payload, &cp)
	chBytes, _ := hex.DecodeString(cp.Challenge)
	srvPub, _ := hex.DecodeString(cp.ServerPubkey)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	var sig []byte
	if version >= auth.AuthVersion {
		sig = ed25519.Sign(priv, auth.Message(auth.LabelClient, srvPub, chBytes, res.tlsHash, pub))
	} else {
		sig = ed25519.Sign(priv, chBytes)
	}
	ap, _ := json.Marshal(protocol.AuthPayload{
		PublicKey: hex.EncodeToString(pub), NickTag: "t#" + hex.EncodeToString(pub)[:16],
		Signature: hex.EncodeToString(sig), ClientChallenge: hex.EncodeToString(cc), AuthVersion: version,
	})
	if err := conn.WriteJSON(protocol.Message{Type: protocol.TypeAuth, Payload: ap}); err != nil {
		t.Fatal(err)
	}
	var m protocol.Message
	if err := conn.ReadJSON(&m); err != nil {
		res.errMsg = err.Error()
		return res
	}
	if m.Type != protocol.TypeAuthOK {
		res.errMsg = string(m.Payload)
		return res
	}
	_ = json.Unmarshal(m.Payload, &res.ok)
	res.cc, res.clientK = cc, pub
	return res
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// Подпись узла в auth_ok v2 привязана к метке, ключу клиента и отпечатку
// TLS — её нельзя выдать ни за node_auth, ни за подпись сырого challenge.
func TestAuthV2TLSBindingAndNoOracle(t *testing.T) {
	ts, id, hash := v2Server(t, true)
	cc := randBytes(32) // «чужой challenge», который атакующий хочет подписать
	res := v2Login(t, ts, auth.AuthVersion, cc)
	if res.errMsg != "" {
		t.Fatalf("вход v2 по TLS: %s", res.errMsg)
	}
	if hex.EncodeToString(res.tlsHash) != hex.EncodeToString(hash) {
		t.Fatal("клиент видит не тот сертификат")
	}
	sig, _ := hex.DecodeString(res.ok.ServerSignature)
	want := auth.Message(auth.LabelServer, res.clientK, cc, hash, id.Public)
	if !ed25519.Verify(id.Public, want, sig) {
		t.Fatal("подпись auth_ok v2 не сходится")
	}
	if ed25519.Verify(id.Public, cc, sig) {
		t.Fatal("узел подписал сырой client_challenge — оракул открыт")
	}
	if ed25519.Verify(id.Public, auth.Message(auth.LabelServer, res.clientK, cc, nil, id.Public), sig) {
		t.Fatal("подпись не привязана к TLS")
	}
	if ed25519.Verify(id.Public, auth.Message(auth.LabelNodeOK, res.clientK, cc, hash, id.Public), sig) {
		t.Fatal("подпись входа годится как node_auth_ok")
	}
	if res.ok.SFUEndpoint != "wss://sfu:9762/ws" {
		t.Fatalf("клиенту v2 нужен wss SFU, получено %q", res.ok.SFUEndpoint)
	}
}

func TestAuthLegacyOnlyWithoutTLS(t *testing.T) {
	tlsTS, _, _ := v2Server(t, true)
	if res := v2Login(t, tlsTS, 0, randBytes(32)); res.errMsg == "" {
		t.Fatal("старый вход на TLS-порту должен отвергаться")
	}
	plain, id, _ := v2Server(t, false)
	cc := randBytes(32)
	res := v2Login(t, plain, 0, cc)
	if res.errMsg != "" {
		t.Fatalf("старый клиент без TLS: %s", res.errMsg)
	}
	sig, _ := hex.DecodeString(res.ok.ServerSignature)
	if !ed25519.Verify(id.Public, cc, sig) {
		t.Fatal("старому клиенту — прежняя подпись")
	}
	if res.ok.SFUEndpoint != "ws://sfu:9760/ws" {
		t.Fatalf("старому клиенту — ws SFU, получено %q", res.ok.SFUEndpoint)
	}
	// v2 без TLS (локальные тесты) — подпись с пустым отпечатком.
	res2 := v2Login(t, plain, auth.AuthVersion, cc)
	sig2, _ := hex.DecodeString(res2.ok.ServerSignature)
	if res2.errMsg != "" || !ed25519.Verify(id.Public, auth.Message(auth.LabelServer, res2.clientK, cc, nil, id.Public), sig2) {
		t.Fatalf("вход v2 без TLS: %s", res2.errMsg)
	}
}
