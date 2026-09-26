package cluster

import (
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"messenger_signal/pkg/protocol"
)

const (
	// linkSendQueue — буфер исходящих на линк. Переполнение = узел не
	// вычитывает; рвём линк, после переподключения snapshot всё выровняет.
	linkSendQueue = 4096
	// linkReadLimit — snapshot с watch-списками может быть большим.
	linkReadLimit = 64 << 20
	linkWriteWait = 10 * time.Second
)

var errLinkGone = errors.New("cluster link closed")

// Link — установленный WS-линк с другим узлом. Все записи идут через одну
// write-горутину (gorilla не допускает конкурентных WriteMessage), Send не
// блокируется — как у hub.Peer.
type Link struct {
	NodeID string
	Addr   string

	conn      *websocket.Conn
	sendCh    chan []byte
	done      chan struct{}
	closeOnce sync.Once

	lastSeen    atomic.Int64 // unix ms последнего входящего сообщения
	rttMs       atomic.Int64
	peerClients atomic.Int64 // клиентов на узле по его heartbeat; -1 — ещё не знаем
}

func newLink(nodeID, addr string, conn *websocket.Conn) *Link {
	l := &Link{
		NodeID: nodeID,
		Addr:   addr,
		conn:   conn,
		sendCh: make(chan []byte, linkSendQueue),
		done:   make(chan struct{}),
	}
	l.lastSeen.Store(time.Now().UnixMilli())
	l.rttMs.Store(-1)
	l.peerClients.Store(-1)
	conn.SetReadLimit(linkReadLimit)
	go l.writePump()
	return l
}

// Send ставит сообщение в очередь. Переполнение очереди закрывает линк.
func (l *Link) Send(data []byte) error {
	select {
	case <-l.done:
		return errLinkGone
	default:
	}
	select {
	case l.sendCh <- data:
		select {
		case <-l.done:
			return errLinkGone
		default:
			return nil
		}
	case <-l.done:
		return errLinkGone
	default:
		l.Close()
		return errLinkGone
	}
}

// SendMsg кодирует сообщение type+payload и ставит в очередь.
func (l *Link) SendMsg(typ string, payload any) error {
	data, err := encodeMsg(typ, payload)
	if err != nil {
		return err
	}
	return l.Send(data)
}

// Close закрывает соединение. Идемпотентен.
func (l *Link) Close() {
	l.closeOnce.Do(func() {
		close(l.done)
		l.conn.Close()
	})
}

// Done закрывается вместе с линком.
func (l *Link) Done() <-chan struct{} { return l.done }

func (l *Link) writePump() {
	for {
		select {
		case data := <-l.sendCh:
			_ = l.conn.SetWriteDeadline(time.Now().Add(linkWriteWait))
			if err := l.conn.WriteMessage(websocket.TextMessage, data); err != nil {
				l.Close()
				return
			}
		case <-l.done:
			return
		}
	}
}

// readLoop читает сообщения до ошибки. Тишина дольше DeadAfter — ошибка
// чтения: узел считается мёртвым (heartbeat шлёт node_ping каждые
// HeartbeatInterval).
func (l *Link) readLoop(handle func(*protocol.Message)) error {
	for {
		_ = l.conn.SetReadDeadline(time.Now().Add(DeadAfter))
		_, raw, err := l.conn.ReadMessage()
		if err != nil {
			return err
		}
		l.lastSeen.Store(time.Now().UnixMilli())
		var msg protocol.Message
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		handle(&msg)
	}
}

func encodeMsg(typ string, payload any) ([]byte, error) {
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	return json.Marshal(protocol.Message{Type: typ, Payload: raw})
}
