package hub

import (
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"messenger_signal/pkg/protocol"
)

// maxWatchKeys — предел watch-списка (друзья + участники общих групп).
const maxWatchKeys = 5000

// presencePolicy — кому виден статус «в сети» пира. Неизменяема: смена
// списка — замена объекта целиком. nil или private=false — старый клиент без
// списка: статус виден всем, как раньше.
type presencePolicy struct {
	private bool
	watch   map[string]struct{}
}

func newPresencePolicy(self string, keys []string) *presencePolicy {
	pol := &presencePolicy{private: true, watch: make(map[string]struct{}, len(keys))}
	for _, k := range keys {
		if len(pol.watch) >= maxWatchKeys {
			break
		}
		k = strings.ToLower(strings.TrimSpace(k))
		if k == self || len(k) != 64 {
			continue
		}
		if _, err := hex.DecodeString(k); err != nil {
			continue
		}
		pol.watch[k] = struct{}{}
	}
	return pol
}

// visibleTo — виден ли статус владельца политики пиру key.
func (pol *presencePolicy) visibleTo(key string) bool {
	if pol == nil || !pol.private {
		return true
	}
	_, ok := pol.watch[strings.ToLower(key)]
	return ok
}

const (
	// writeWait — дедлайн одной записи в WS. Зависший клиент (заполненный
	// TCP-буфер при half-open соединении) не должен держать write pump дольше.
	writeWait = 10 * time.Second
	// sendQueueSize — буфер исходящих на пира. Переполнение = клиент не
	// вычитывает; рвём соединение, реконнект дешевле зомби.
	sendQueueSize = 256
)

// ErrPeerGone — соединение пира закрыто или его очередь переполнена.
var ErrPeerGone = errors.New("peer connection closed")

// Peer — подключённый и аутентифицированный клиент.
//
// Все записи в Conn идут через единственную write-горутину (writePump):
// gorilla/websocket не допускает конкурентных WriteMessage. Send только
// кладёт данные в буферизованный канал и никогда не блокируется — медленный
// клиент не может застопорить рассылки и ping-loop остальным.
type Peer struct {
	PublicKey string
	nickTag   atomic.Pointer[string]
	Conn      *websocket.Conn
	// SessionID — id этого соединения; ConnectedAtMs — время auth. Нужны
	// каталогу кластера: при одном ключе на двух узлах побеждает более новая
	// сессия.
	SessionID     string
	ConnectedAtMs int64

	mu       sync.Mutex
	lastPong time.Time

	// Кому виден статус «в сети» (presence в auth / presence_watch).
	presMu   sync.RWMutex
	presence *presencePolicy

	sendCh    chan []byte
	done      chan struct{}
	closeOnce sync.Once
}

// NewPeer создаёт нового пира и запускает его write pump
func NewPeer(publicKey, nickTag string, conn *websocket.Conn) *Peer {
	now := time.Now()
	p := &Peer{
		PublicKey:     publicKey,
		Conn:          conn,
		SessionID:     uuid.NewString(),
		ConnectedAtMs: now.UnixMilli(),
		lastPong:      now,
		sendCh:        make(chan []byte, sendQueueSize),
		done:          make(chan struct{}),
	}
	p.nickTag.Store(&nickTag)
	go p.writePump()
	return p
}

// DirEntry — запись каталога кластера для этого пира на узле nodeID.
func (p *Peer) DirEntry(nodeID string) protocol.NodeDirEntry {
	e := protocol.NodeDirEntry{
		PubKey:    p.PublicKey,
		NickTag:   p.NickTag(),
		NodeID:    nodeID,
		SessionID: p.SessionID,
		TSms:      p.ConnectedAtMs,
	}
	if pol := p.policy(); pol != nil && pol.private {
		e.Private = true
		e.Watch = make([]string, 0, len(pol.watch))
		for k := range pol.watch {
			e.Watch = append(e.Watch, k)
		}
		sort.Strings(e.Watch)
	}
	return e
}

// NickTag возвращает текущий nick_tag (безопасно для конкурентного чтения).
func (p *Peer) NickTag() string {
	s := p.nickTag.Load()
	if s == nil {
		return ""
	}
	return *s
}

// SetNickTag атомарно обновляет nick_tag.
func (p *Peer) SetNickTag(nickTag string) {
	p.nickTag.Store(&nickTag)
}

// SetPresence — кому виден статус «в сети» пира: keys — публичные ключи
// (hex) друзей и участников общих групп. После вызова пир «приватный»:
// пустой список скрывает статус от всех.
func (p *Peer) SetPresence(keys []string) {
	pol := newPresencePolicy(strings.ToLower(p.PublicKey), keys)
	p.presMu.Lock()
	p.presence = pol
	p.presMu.Unlock()
}

func (p *Peer) policy() *presencePolicy {
	p.presMu.RLock()
	defer p.presMu.RUnlock()
	return p.presence
}

// VisibleTo — виден ли статус «в сети» пира тому, у кого ключ key.
func (p *Peer) VisibleTo(key string) bool {
	return p.policy().visibleTo(key)
}

// Private — клиент прислал watch-список (новый клиент).
func (p *Peer) Private() bool {
	pol := p.policy()
	return pol != nil && pol.private
}

// Send ставит сообщение в очередь отправки. Не блокируется: при закрытом
// соединении или переполненной очереди возвращает ErrPeerGone (и во втором
// случае рвёт соединение — клиент безнадёжно отстал).
func (p *Peer) Send(data []byte) error {
	select {
	case <-p.done:
		return ErrPeerGone
	default:
	}
	select {
	case p.sendCh <- data:
		// Оба case могли быть готовы одновременно (done закрыт, а в sendCh есть
		// место) — Go выбирает случайно. Перепроверяем done после постановки в
		// очередь, чтобы не отчитаться "доставлено" в уже мёртвый pump.
		select {
		case <-p.done:
			return ErrPeerGone
		default:
			return nil
		}
	case <-p.done:
		return ErrPeerGone
	default:
		p.Close()
		return ErrPeerGone
	}
}

// SendPing отправляет ping для проверки соединения. WriteControl потокобезопасен
// относительно writePump и сам принимает дедлайн.
func (p *Peer) SendPing() error {
	return p.Conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait))
}

// Close закрывает соединение и останавливает write pump. Идемпотентен.
// Закрытие сетевого conn разблокирует readLoop сервера, который сделает
// Unregister — единая точка удаления из реестра сохраняется.
func (p *Peer) Close() {
	p.closeOnce.Do(func() {
		close(p.done)
		p.Conn.Close()
	})
}

func (p *Peer) writePump() {
	for {
		select {
		case data := <-p.sendCh:
			_ = p.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := p.Conn.WriteMessage(websocket.TextMessage, data); err != nil {
				p.Close()
				return
			}
		case <-p.done:
			return
		}
	}
}

// UpdatePong обновляет время последнего pong
func (p *Peer) UpdatePong() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastPong = time.Now()
}

// LastPong возвращает время последнего pong
func (p *Peer) LastPong() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastPong
}
