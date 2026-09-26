package handler

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/websocket"

	"messenger_sfu/internal/room"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  256,
	WriteBufferSize: 256,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// voxsMagic — магик VOXS пакета (должен совпадать с клиентским voxsMagic в
// app/internal/services/voice_protocol.go). При rename Nexus→Vox (v0.00.000.0038)
// клиент стал писать 'V','O','X','S', а сервер до v0.00.000.0062 ждал старый
// 'N','X','S','F' → всё silently дропалось в readPump.
var voxsMagic = [4]byte{'V', 'O', 'X', 'S'}

const (
	// writeWait — дедлайн одной записи; зависший клиент не держит writePump.
	writeWait = 10 * time.Second
	// pongWait — max тишина от клиента (данные или pong) до разрыва.
	// При mute+DTX клиент может подолгу не слать фреймы — держим с запасом.
	pongWait = 60 * time.Second
	// pingPeriod — период серверного ping (должен быть < pongWait).
	pingPeriod = 25 * time.Second
	// maxFrameSize — лимит входящего фрейма. Opus 20ms на высоком битрейте +
	// VOXS(8) + VOX1(6) заголовки; 4 KiB с запасом под adaptive bitrate,
	// прежний 512 был растяжкой (gorilla рвёт соединение кодом 1009).
	maxFrameSize = 4096
)

type Handler struct {
	mgr    *room.Manager
	secret string
	log    *slog.Logger
}

func New(mgr *room.Manager, secret string, log *slog.Logger) *Handler {
	return &Handler{mgr: mgr, secret: secret, log: log}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	roomID := q.Get("room")
	peerID := q.Get("peer")
	token := q.Get("token")
	ts := q.Get("ts")

	if roomID == "" || peerID == "" {
		http.Error(w, "missing room or peer", http.StatusBadRequest)
		return
	}

	// HMAC auth
	if h.secret != "" {
		if !h.verifyToken(peerID, roomID, ts, token) {
			http.Error(w, "auth failed", http.StatusUnauthorized)
			return
		}
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.log.Warn("ws upgrade failed", slog.String("error", err.Error()))
		return
	}

	senderHash := peerIDHash(peerID)
	member := &room.Member{
		PeerID:     peerID,
		SenderHash: senderHash,
		Conn:       conn,
		WriteCh:    make(chan []byte, 50),
	}
	rm := h.mgr.Join(roomID, member)

	h.log.Info("member joined",
		slog.String("room", roomID),
		slog.String("peer", safeShort(peerID)),
		slog.Int("members", rm.Len()))

	go h.writePump(member)
	h.readPump(conn, rm, senderHash)

	// cleanup: Leave удаляет member только если он всё ещё текущий для этого
	// senderHash (при реконнекте Join уже поставил нового — не трогаем его).
	remaining := h.mgr.Leave(roomID, rm, member)
	member.Stop()
	conn.Close()
	h.log.Info("member left",
		slog.String("room", roomID),
		slog.String("peer", safeShort(peerID)),
		slog.Int("remaining", remaining))
}

func (h *Handler) readPump(conn *websocket.Conn, rm *room.Room, senderHash uint32) {
	conn.SetReadLimit(maxFrameSize)
	// Half-open соединение (Wi-Fi drop, NAT timeout при mute/DTX) без дедлайна
	// висело бы в ReadMessage вечно — зомби-участник + утечка горутин.
	conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		conn.SetReadDeadline(time.Now().Add(pongWait))
		if len(msg) < 8 {
			continue
		}
		// Verify VOXS magic
		if msg[0] != voxsMagic[0] || msg[1] != voxsMagic[1] || msg[2] != voxsMagic[2] || msg[3] != voxsMagic[3] {
			continue
		}
		rm.Forward(senderHash, msg)
	}
}

func (h *Handler) writePump(m *room.Member) {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	for {
		select {
		case frame, ok := <-m.WriteCh:
			if !ok {
				return
			}
			m.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := m.Conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
				// Разрываем conn: readPump этого участника выйдет по ошибке
				// чтения и сделает Leave — единая точка cleanup.
				m.Conn.Close()
				return
			}
		case <-ticker.C:
			if err := m.Conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait)); err != nil {
				m.Conn.Close()
				return
			}
		}
	}
}

func (h *Handler) verifyToken(peerID, roomID, tsStr, token string) bool {
	if token == "" || tsStr == "" {
		return false
	}
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return false
	}
	// Reject tokens older than 60 seconds
	if math.Abs(float64(time.Now().Unix()-ts)) > 60 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(h.secret))
	fmt.Fprintf(mac, "%s:%s:%d", peerID, roomID, ts)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(token))
}

func peerIDHash(peerID string) uint32 {
	h := sha256.Sum256([]byte(peerID))
	return uint32(h[0]) | uint32(h[1])<<8 | uint32(h[2])<<16 | uint32(h[3])<<24
}

func safeShort(s string) string {
	if len(s) > 16 {
		return s[:16]
	}
	return s
}
