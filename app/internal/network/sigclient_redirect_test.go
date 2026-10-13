package network

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// redirectServer — фейковый узел: challenge, затем redirect вместо auth_ok.
// signWith — чьим ключом подписать client_challenge (подделка — чужим).
func redirectServer(t *testing.T, srvPriv, signWith ed25519.PrivateKey, target RedirectPayload, tamper bool) *httptest.Server {
	t.Helper()
	up := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		ch := make([]byte, 32)
		_, _ = rand.Read(ch)
		cp, _ := json.Marshal(ChallengePayload{
			Challenge:    hex.EncodeToString(ch),
			ServerPubkey: hex.EncodeToString(srvPriv.Public().(ed25519.PublicKey)),
		})
		_ = conn.WriteJSON(Message{Type: TypeChallenge, Payload: cp})

		var auth Message
		if err := conn.ReadJSON(&auth); err != nil {
			return
		}
		var ap AuthPayload
		_ = json.Unmarshal(auth.Payload, &ap)
		if !ap.RedirectOK {
			t.Error("клиент должен объявлять redirect_ok")
		}
		cc, _ := hex.DecodeString(ap.ClientChallenge)
		rp := target
		rp.ServerSignature = hex.EncodeToString(ed25519.Sign(signWith,
			authMessage(authLabelRedirect, cc, []byte(target.Addr), []byte(strings.ToLower(target.Pubkey)), nil)))
		if tamper {
			rp.Addr = "evil.example:9743" // подпись покрывает addr — подмена не пройдёт
		}
		raw, _ := json.Marshal(rp)
		_ = conn.WriteJSON(Message{Type: TypeRedirect, Payload: raw})
		time.Sleep(100 * time.Millisecond)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func TestConnectRedirect(t *testing.T) {
	srvPub, srvPriv, _ := ed25519.GenerateKey(rand.Reader)
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	cliPub, cliPriv, _ := ed25519.GenerateKey(rand.Reader)
	target := RedirectPayload{Addr: "10.1.2.3:9741", Pubkey: strings.Repeat("A", 64)}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	connect := func(signWith ed25519.PrivateKey, tamper ...bool) error {
		ts := redirectServer(t, srvPriv, signWith, target, len(tamper) > 0)
		c := NewSignalClient(log, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return c.Connect(ctx, "ws://"+strings.TrimPrefix(ts.URL, "http://"), hex.EncodeToString(srvPub), cliPriv, cliPub, "t#1")
	}

	var rd *RedirectError
	if err := connect(srvPriv); !errors.As(err, &rd) {
		t.Fatalf("ожидался RedirectError, получено %v", err)
	}
	if rd.Addr != target.Addr || rd.Pubkey != strings.ToLower(target.Pubkey) {
		t.Fatalf("RedirectError: %+v", rd)
	}

	err := connect(otherPriv)
	if err == nil || errors.As(err, &rd) && rd != nil && err.Error() == rd.Error() {
		t.Fatalf("redirect с чужой подписью должен отвергаться, получено %v", err)
	}
	if !strings.Contains(err.Error(), "verification failed") {
		t.Fatalf("ожидалась ошибка проверки подписи, получено %v", err)
	}
	if err := connect(srvPriv, true); err == nil || !strings.Contains(err.Error(), "verification failed") {
		t.Fatalf("redirect с подменённым addr должен отвергаться, получено %v", err)
	}
}
