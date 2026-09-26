package cluster

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"messenger_signal/internal/auth"
	"messenger_signal/internal/hub"
	"messenger_signal/internal/identity"
	"messenger_signal/pkg/protocol"
)

// Тайминги кластера (docs/cluster-contract.md §2, §3.3). Переменные, чтобы
// тесты могли ставить миллисекунды.
var (
	HeartbeatInterval = 5 * time.Second
	DeadAfter         = 15 * time.Second
	GracePeriod       = 15 * time.Second
	DialMinBackoff    = 1 * time.Second
	DialMaxBackoff    = 30 * time.Second
	HandshakeTimeout  = 10 * time.Second
	TombstoneTTL      = 24 * time.Hour
	JanitorInterval   = time.Minute
	// AntiEntropyInterval — как часто узлы сверяют головы журналов групп при
	// живом линке. Без этого досыл идёт только при подъёме линка, и остаток
	// догона ждёт переподключения, которого может не быть сутками.
	AntiEntropyInterval = 5 * time.Minute
)

var (
	errNoLink  = errors.New("нет линка с узлом")
	errStopped = errors.New("кластер остановлен")
)

// grpBatch — сообщений группы в одной пачке node_grp_msgs.
const grpBatch = 200

// GroupHandler — групповой relay узла для репликации журналов (§7).
// Реализует server.Server.
type GroupHandler interface {
	// ApplyGroupMsgs — сохранить сообщения других журналов и донести новые
	// до своих участников; возвращает число новых. live — живая реплика
	// (node_grp_msg), иначе догон (node_grp_msgs).
	ApplyGroupMsgs(msgs []protocol.NodeGrpMsg, live bool) int
	// ApplyGroupMembers — строки состава группы (LWW).
	ApplyGroupMembers(p protocol.NodeGrpMembersPayload)
	// GroupState — все группы узла и головы журналов; составы — только при
	// withMembers (подъём линка), периодическая сверка идёт без них.
	GroupState(withMembers bool) protocol.NodeGrpStatePayload
	// GroupMissingFor — сообщения, которых нет у соседа по его состоянию.
	GroupMissingFor(peer protocol.NodeGrpStatePayload) []protocol.NodeGrpMsg
}

// Manager — узел кластера: линки с другими узлами, каталог, пересылка.
// Реализует hub.Cluster.
type Manager struct {
	cfg    Config
	id     *identity.Identity
	selfID string
	hub    *hub.Hub
	dir    *Directory
	log    *slog.Logger

	peers map[string]PeerNode // nodeID → узел из конфига
	gh    GroupHandler        // групповой relay (nil — выключен); задаётся до Start

	mu     sync.Mutex
	links  map[string]*Link
	grace  map[string]*time.Timer
	downAt map[string]time.Time

	done     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewManager создаёт узел кластера. Start запускает дозвон.
func NewManager(cfg Config, id *identity.Identity, h *hub.Hub, log *slog.Logger) *Manager {
	m := &Manager{
		cfg:    cfg,
		id:     id,
		selfID: strings.ToLower(id.PubKeyHex()),
		hub:    h,
		dir:    NewDirectory(),
		log:    log.With(slog.String("component", "cluster")),
		peers:  make(map[string]PeerNode, len(cfg.Peers)),
		links:  make(map[string]*Link),
		grace:  make(map[string]*time.Timer),
		downAt: make(map[string]time.Time),
		done:   make(chan struct{}),
	}
	for _, p := range cfg.Peers {
		m.peers[p.NodeID] = p
	}
	return m
}

// SetGroupHandler подключает репликацию группового relay. Вызывать до Start.
func (m *Manager) SetGroupHandler(h GroupHandler) { m.gh = h }

// BroadcastGroupMsg — новое сообщение журнала этого узла всем узлам.
func (m *Manager) BroadcastGroupMsg(g protocol.NodeGrpMsg) {
	m.broadcast(protocol.TypeNodeGrpMsg, g)
}

// BroadcastGroupMembers — изменения состава группы всем узлам.
func (m *Manager) BroadcastGroupMembers(p protocol.NodeGrpMembersPayload) {
	m.broadcast(protocol.TypeNodeGrpMembers, p)
}

// sendGroupState — состояние групп соседу: он применит составы (если они
// есть) и дошлёт недостающие сообщения. С составами — при подъёме линка;
// периодическая сверка идёт без них: сосед разбирает кадр прямо в read loop,
// и транзакция на каждую группу раз в несколько минут там ни к чему.
func (m *Manager) sendGroupState(l *Link, withMembers bool) {
	if m.gh == nil {
		return
	}
	st := m.gh.GroupState(withMembers)
	_ = l.SendMsg(protocol.TypeNodeGrpState, st)
	if withMembers {
		m.log.Info("состояние групп отправлено",
			slog.String("node", short(l.NodeID)), slog.Int("groups", len(st.Groups)))
	}
}

// antiEntropy — периодическая сверка голов журналов по всем живым линкам.
func (m *Manager) antiEntropy() {
	defer m.wg.Done()
	t := time.NewTicker(AntiEntropyInterval)
	defer t.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-t.C:
			m.mu.Lock()
			links := make([]*Link, 0, len(m.links))
			for _, l := range m.links {
				links = append(links, l)
			}
			m.mu.Unlock()
			// Вне m.mu: состояние читается из SQLite.
			for _, l := range links {
				m.sendGroupState(l, false)
			}
		}
	}
}

// Start запускает дозвон к узлам с большим node_id (линк инициирует меньший).
func (m *Manager) Start() {
	dialing := 0
	for _, p := range m.cfg.Peers {
		if m.selfID < p.NodeID {
			dialing++
			m.wg.Add(1)
			go m.dialLoop(p)
		}
	}
	m.wg.Add(1)
	go m.janitor()
	if m.gh != nil {
		m.wg.Add(1)
		go m.antiEntropy()
	}
	m.log.Info("кластер запущен",
		slog.String("node_id", m.selfID),
		slog.String("self_addr", m.cfg.SelfAddr),
		slog.Int("peers", len(m.cfg.Peers)),
		slog.Int("dialing", dialing))
}

// Stop закрывает линки и останавливает дозвон.
func (m *Manager) Stop() {
	m.stopOnce.Do(func() {
		close(m.done)
		m.mu.Lock()
		for _, l := range m.links {
			l.Close()
		}
		for _, t := range m.grace {
			t.Stop()
		}
		m.mu.Unlock()
	})
	m.wg.Wait()
}

func (m *Manager) stopped() bool {
	select {
	case <-m.done:
		return true
	default:
		return false
	}
}

// --- hub.Cluster ---

// NodeID — hex pubkey этого узла.
func (m *Manager) NodeID() string { return m.selfID }

// Publish — локальный клиент прошёл auth или сменил ник / watch.
func (m *Manager) Publish(e protocol.NodeDirEntry) {
	e.NodeID = m.selfID
	res := m.dir.Put(e)
	if res.Rejected {
		// На другом узле более новая сессия того же ключа (разъехались часы) —
		// проигрывает наша.
		m.log.Warn("session_replaced: на другом узле более новая сессия",
			slog.String("nick_tag", e.NickTag))
		go m.hub.ReplaceLocal(e.PubKey, e.SessionID)
		return
	}
	if res.Event == nil {
		return
	}
	m.broadcast(protocol.TypeNodeDirPut, e)
}

// Remove — локальная сессия закрыта.
func (m *Manager) Remove(pubKey, sessionID string) {
	if ev := m.dir.Del(m.selfID, pubKey, sessionID); ev == nil {
		return
	}
	m.broadcast(protocol.TypeNodeDirDel, protocol.NodeDirDelPayload{PubKey: pubKey, SessionID: sessionID})
}

// Lookup — запись каталога.
func (m *Manager) Lookup(pubKey string) (protocol.NodeDirEntry, bool) { return m.dir.Lookup(pubKey) }

// RemoteEntries — клиенты других узлов.
func (m *Manager) RemoteEntries() []protocol.NodeDirEntry { return m.dir.Except(m.selfID) }

// Tombstone — запись недавно ушедшего клиента.
func (m *Manager) Tombstone(pubKey string) (protocol.NodeDirEntry, bool) {
	return m.dir.Tombstone(pubKey)
}

// Forward — отправить сообщение клиенту на узле nodeID.
func (m *Manager) Forward(nodeID, from string, msg *protocol.Message) error {
	m.mu.Lock()
	l := m.links[nodeID]
	m.mu.Unlock()
	if l == nil {
		return errNoLink
	}
	return l.SendMsg(protocol.TypeNodeFwd, protocol.NodeFwdPayload{From: from, Msg: *msg})
}

// broadcast рассылает сообщение по всем живым линкам. Под m.mu — чтобы
// порядок относительно snapshot в linkUp был согласован.
func (m *Manager) broadcast(typ string, payload any) {
	data, err := encodeMsg(typ, payload)
	if err != nil {
		m.log.Error("encode", slog.String("type", typ), slog.String("error", err.Error()))
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, l := range m.links {
		_ = l.Send(data)
	}
}

// --- линки ---

func (m *Manager) dialLoop(p PeerNode) {
	defer m.wg.Done()
	backoff := DialMinBackoff
	failures := 0
	for !m.stopped() {
		l, err := m.dial(p)
		if err != nil {
			failures++
			if failures == 1 || failures%10 == 0 {
				m.log.Warn("линк: дозвон не удался",
					slog.String("node", short(p.NodeID)),
					slog.String("addr", p.Addr),
					slog.Int("attempt", failures),
					slog.String("error", err.Error()))
			}
			select {
			case <-m.done:
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, DialMaxBackoff)
			continue
		}
		failures = 0
		backoff = DialMinBackoff
		m.runLink(l)
		select {
		case <-m.done:
			return
		case <-time.After(DialMinBackoff):
		}
	}
}

// dial — исходящий линк: challenge от узла → node_auth → node_auth_ok.
func (m *Manager) dial(p PeerNode) (*Link, error) {
	ctx, cancel := context.WithTimeout(context.Background(), HandshakeTimeout)
	defer cancel()
	go func() {
		select {
		case <-m.done:
			cancel()
		case <-ctx.Done():
		}
	}()

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, p.Addr, nil)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			conn.Close()
		}
	}()
	deadline := time.Now().Add(HandshakeTimeout)
	_ = conn.SetReadDeadline(deadline)
	_ = conn.SetWriteDeadline(deadline)

	var chMsg protocol.Message
	if err := conn.ReadJSON(&chMsg); err != nil {
		return nil, fmt.Errorf("read challenge: %w", err)
	}
	if chMsg.Type != protocol.TypeChallenge {
		return nil, fmt.Errorf("ожидался challenge, получено %s", chMsg.Type)
	}
	var ch protocol.ChallengePayload
	if err := json.Unmarshal(chMsg.Payload, &ch); err != nil {
		return nil, fmt.Errorf("challenge payload: %w", err)
	}
	if !strings.EqualFold(ch.ServerPubkey, p.NodeID) {
		return nil, fmt.Errorf("узел предъявил чужой ключ %s", short(ch.ServerPubkey))
	}
	chBytes, err := hex.DecodeString(ch.Challenge)
	if err != nil || len(chBytes) == 0 {
		return nil, fmt.Errorf("invalid challenge")
	}
	myChallenge := make([]byte, 32)
	if _, err := rand.Read(myChallenge); err != nil {
		return nil, err
	}
	authData, _ := encodeMsg(protocol.TypeNodeAuth, protocol.NodeAuthPayload{
		NodeID:          m.selfID,
		Signature:       hex.EncodeToString(ed25519.Sign(m.id.Private, chBytes)),
		ClientChallenge: hex.EncodeToString(myChallenge),
	})
	if err := conn.WriteMessage(websocket.TextMessage, authData); err != nil {
		return nil, fmt.Errorf("send node_auth: %w", err)
	}

	var okMsg protocol.Message
	if err := conn.ReadJSON(&okMsg); err != nil {
		return nil, fmt.Errorf("read node_auth_ok: %w", err)
	}
	if okMsg.Type == protocol.TypeError {
		var e protocol.ErrorPayload
		_ = json.Unmarshal(okMsg.Payload, &e)
		return nil, fmt.Errorf("узел отказал: %s %s", e.Code, e.Message)
	}
	if okMsg.Type != protocol.TypeNodeAuthOK {
		return nil, fmt.Errorf("ожидался node_auth_ok, получено %s", okMsg.Type)
	}
	var okp protocol.NodeAuthOKPayload
	if err := json.Unmarshal(okMsg.Payload, &okp); err != nil {
		return nil, err
	}
	pub, _ := hex.DecodeString(p.NodeID)
	sig, err := hex.DecodeString(okp.ServerSignature)
	if err != nil || !ed25519.Verify(ed25519.PublicKey(pub), myChallenge, sig) {
		return nil, fmt.Errorf("подпись узла не сходится")
	}

	_ = conn.SetReadDeadline(time.Time{})
	_ = conn.SetWriteDeadline(time.Time{})
	ok = true
	return newLink(p.NodeID, p.Addr, conn), nil
}

// Accept — входящий линк: клиент соединения ответил на challenge сообщением
// node_auth. Проверяет allowlist, направление и подпись, отвечает
// node_auth_ok и держит линк до закрытия (блокирует вызывающую горутину).
// Ошибка — линк отвергнут, соединение закрывает вызывающий.
func (m *Manager) Accept(conn *websocket.Conn, challenge string, p protocol.NodeAuthPayload) error {
	if m.stopped() {
		return errStopped
	}
	nodeID := strings.ToLower(p.NodeID)
	peer, ok := m.peers[nodeID]
	if !ok {
		return fmt.Errorf("узел %s не в CLUSTER_PEERS", short(nodeID))
	}
	if nodeID >= m.selfID {
		return fmt.Errorf("линк инициирует узел с меньшим node_id, а %s больше", short(nodeID))
	}
	if err := auth.VerifySignature(nodeID, challenge, p.Signature); err != nil {
		return fmt.Errorf("подпись узла %s: %w", short(nodeID), err)
	}
	cc, err := hex.DecodeString(p.ClientChallenge)
	if err != nil || len(cc) == 0 {
		return fmt.Errorf("invalid client_challenge")
	}
	okData, _ := encodeMsg(protocol.TypeNodeAuthOK, protocol.NodeAuthOKPayload{
		ServerSignature: hex.EncodeToString(ed25519.Sign(m.id.Private, cc)),
	})
	_ = conn.SetWriteDeadline(time.Now().Add(HandshakeTimeout))
	if err := conn.WriteMessage(websocket.TextMessage, okData); err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Time{})
	m.runLink(newLink(nodeID, peer.Addr, conn))
	return nil
}

// runLink — жизнь линка: регистрация и snapshot, heartbeat, чтение до
// ошибки, снятие с учёта и grace-таймер.
func (m *Manager) runLink(l *Link) {
	if !m.linkUp(l) {
		l.Close()
		return
	}
	hbStop := make(chan struct{})
	go m.heartbeat(l, hbStop)
	err := l.readLoop(func(msg *protocol.Message) { m.handle(l, msg) })
	close(hbStop)
	l.Close()
	m.linkDown(l, err)
}

func (m *Manager) linkUp(l *Link) bool {
	m.mu.Lock()
	if m.stopped() {
		m.mu.Unlock()
		return false
	}
	if old := m.links[l.NodeID]; old != nil {
		old.Close()
	}
	m.links[l.NodeID] = l
	recovered := false
	if t := m.grace[l.NodeID]; t != nil {
		t.Stop()
		delete(m.grace, l.NodeID)
		recovered = true
	}
	delete(m.downAt, l.NodeID)
	// Snapshot — под тем же m.mu, что и broadcast: put/del своих клиентов
	// уходят в линк строго после snapshot, не раньше.
	snap := m.dir.ByNode(m.selfID)
	_ = l.SendMsg(protocol.TypeNodeDirSnapshot, protocol.NodeDirSnapshotPayload{Entries: snap})
	m.mu.Unlock()

	m.log.Info("линк поднят",
		slog.String("node", short(l.NodeID)),
		slog.String("addr", l.Addr),
		slog.Int("snapshot_sent", len(snap)),
		slog.Bool("within_grace", recovered))
	// Вне m.mu: состояние групп читается из SQLite. Живые node_grp_* уже идут
	// в этот линк; повторы безвредны — приём идемпотентен.
	m.sendGroupState(l, true)
	return true
}

func (m *Manager) linkDown(l *Link, cause error) {
	m.mu.Lock()
	if m.links[l.NodeID] != l {
		m.mu.Unlock()
		return
	}
	delete(m.links, l.NodeID)
	down := time.Now()
	m.downAt[l.NodeID] = down
	if !m.stopped() {
		node := l.NodeID
		m.grace[node] = time.AfterFunc(GracePeriod, func() { m.purgeNode(node, down) })
	}
	m.mu.Unlock()

	errStr := ""
	if cause != nil {
		errStr = cause.Error()
	}
	m.log.Warn("линк упал",
		slog.String("node", short(l.NodeID)),
		slog.String("addr", l.Addr),
		slog.String("error", errStr),
		slog.Duration("grace", GracePeriod))
}

// purgeNode — grace истёк, линк не вернулся: записи узла удаляются.
// Проверка линка и удаление — под m.mu, чтобы не стереть записи, которые
// snapshot вернувшегося узла уже восстановил.
func (m *Manager) purgeNode(node string, down time.Time) {
	m.mu.Lock()
	if m.links[node] != nil || m.stopped() {
		m.mu.Unlock()
		return
	}
	delete(m.grace, node)
	evs := m.dir.RemoveNode(node)
	m.mu.Unlock()

	m.log.Info("записи узла удалены после grace",
		slog.String("node", short(node)),
		slog.Int("entries", len(evs)),
		slog.Duration("since_link_down", time.Since(down)))
	for _, ev := range evs {
		m.hub.OnDirEvent(ev)
	}
}

func (m *Manager) heartbeat(l *Link, stop <-chan struct{}) {
	t := time.NewTicker(HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-l.Done():
			return
		case <-t.C:
			_ = l.SendMsg(protocol.TypeNodePing, protocol.NodePingPayload{
				TSms: time.Now().UnixMilli(), Clients: m.hub.Count(),
			})
		}
	}
}

func (m *Manager) janitor() {
	defer m.wg.Done()
	t := time.NewTicker(JanitorInterval)
	defer t.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-t.C:
			m.dir.PurgeTombs()
		}
	}
}

// handle — входящее сообщение по линку l.
func (m *Manager) handle(l *Link, msg *protocol.Message) {
	switch msg.Type {
	case protocol.TypeNodePing:
		var p protocol.NodePingPayload
		if json.Unmarshal(msg.Payload, &p) == nil {
			l.peerClients.Store(int64(p.Clients))
		}
		_ = l.SendMsg(protocol.TypeNodePong, protocol.NodePingPayload{TSms: p.TSms, Clients: m.hub.Count()})

	case protocol.TypeNodePong:
		var p protocol.NodePingPayload
		if json.Unmarshal(msg.Payload, &p) == nil {
			if rtt := time.Now().UnixMilli() - p.TSms; rtt >= 0 {
				l.rttMs.Store(rtt)
			}
			l.peerClients.Store(int64(p.Clients))
		}

	case protocol.TypeNodeDirPut:
		var e protocol.NodeDirEntry
		if err := json.Unmarshal(msg.Payload, &e); err != nil {
			return
		}
		if e.NodeID != l.NodeID {
			m.log.Warn("node_dir_put с чужим node_id отброшен",
				slog.String("link", short(l.NodeID)), slog.String("entry_node", short(e.NodeID)))
			return
		}
		m.applyRemote(m.dir.Put(e))

	case protocol.TypeNodeDirDel:
		var p protocol.NodeDirDelPayload
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			return
		}
		if ev := m.dir.Del(l.NodeID, p.PubKey, p.SessionID); ev != nil {
			m.hub.OnDirEvent(*ev)
		}

	case protocol.TypeNodeDirSnapshot:
		var p protocol.NodeDirSnapshotPayload
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			return
		}
		results := m.dir.Snapshot(l.NodeID, p.Entries)
		m.log.Info("snapshot получен",
			slog.String("node", short(l.NodeID)),
			slog.Int("entries", len(p.Entries)),
			slog.Int("changes", len(results)))
		for _, r := range results {
			m.applyRemote(r)
		}

	case protocol.TypeNodeFwd:
		var p protocol.NodeFwdPayload
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			return
		}
		if code := m.hub.DeliverForwarded(p.From, &p.Msg); code != "" {
			_ = l.SendMsg(protocol.TypeNodeFwdErr, protocol.NodeFwdErrPayload{
				From: p.From, To: p.Msg.To, Code: code,
			})
		}

	case protocol.TypeNodeFwdErr:
		var p protocol.NodeFwdErrPayload
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			return
		}
		m.hub.ForwardFailed(p.From, p.To)

	case protocol.TypeNodeGrpMsg:
		if m.gh == nil {
			return
		}
		var g protocol.NodeGrpMsg
		if err := json.Unmarshal(msg.Payload, &g); err != nil {
			return
		}
		m.gh.ApplyGroupMsgs([]protocol.NodeGrpMsg{g}, true)

	case protocol.TypeNodeGrpMsgs:
		if m.gh == nil {
			return
		}
		var p protocol.NodeGrpMsgsPayload
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			return
		}
		n := m.gh.ApplyGroupMsgs(p.Messages, false)
		m.log.Info("групповые сообщения догнаны",
			slog.String("node", short(l.NodeID)),
			slog.Int("received", len(p.Messages)),
			slog.Int("new", n))

	case protocol.TypeNodeGrpMembers:
		if m.gh == nil {
			return
		}
		var p protocol.NodeGrpMembersPayload
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			return
		}
		m.gh.ApplyGroupMembers(p)

	case protocol.TypeNodeGrpState:
		if m.gh == nil {
			return
		}
		var st protocol.NodeGrpStatePayload
		if err := json.Unmarshal(msg.Payload, &st); err != nil {
			return
		}
		for _, g := range st.Groups {
			m.gh.ApplyGroupMembers(protocol.NodeGrpMembersPayload{
				GroupID: g.GroupID, CreatedMs: g.CreatedMs, Members: g.Members,
			})
		}
		missing := m.gh.GroupMissingFor(st)
		for i := 0; i < len(missing); i += grpBatch {
			end := min(i+grpBatch, len(missing))
			_ = l.SendMsg(protocol.TypeNodeGrpMsgs, protocol.NodeGrpMsgsPayload{Messages: missing[i:end]})
		}
		// Периодическая сверка без досыла — норма, её не логируем.
		full := len(st.Groups) > 0 && len(st.Groups[0].Members) > 0
		if full || len(missing) > 0 {
			m.log.Info("состояние групп узла получено",
				slog.String("node", short(l.NodeID)),
				slog.Int("groups", len(st.Groups)),
				slog.Bool("members", full),
				slog.Int("catch_up_sent", len(missing)))
		}

	default:
		m.log.Debug("неизвестное сообщение линка",
			slog.String("type", msg.Type), slog.String("node", short(l.NodeID)))
	}
}

// applyRemote — итог записи с другого узла: вытесненная локальная сессия
// закрывается, событие уходит в hub.
func (m *Manager) applyRemote(r PutResult) {
	if r.Replaced != nil && r.Replaced.NodeID == m.selfID {
		m.log.Warn("session_replaced: ключ подключился к другому узлу позже",
			slog.String("nick_tag", r.Replaced.NickTag))
		m.hub.ReplaceLocal(r.Replaced.PubKey, r.Replaced.SessionID)
	}
	if r.Event != nil {
		m.hub.OnDirEvent(*r.Event)
	}
}

// --- балансировка (§6) ---

// ClientAddr — адрес узла в формате клиента: "host:port" без схемы и без
// пути /ws (клиент сам добавит ws:// и /ws).
func ClientAddr(wsURL string) string {
	a := strings.TrimPrefix(strings.TrimPrefix(wsURL, "wss://"), "ws://")
	return strings.TrimSuffix(a, "/ws")
}

// ServerList — все узлы кластера (себя, если задан CLUSTER_SELF_ADDR, и
// соседей из конфига) для server_list.
func (m *Manager) ServerList() []protocol.ServerNode {
	nodes := make([]protocol.ServerNode, 0, len(m.cfg.Peers)+1)
	if m.cfg.SelfAddr != "" {
		nodes = append(nodes, protocol.ServerNode{Addr: ClientAddr(m.cfg.SelfAddr), Pubkey: m.selfID})
	}
	for _, p := range m.cfg.Peers {
		nodes = append(nodes, protocol.ServerNode{Addr: ClientAddr(p.Addr), Pubkey: p.NodeID})
	}
	return nodes
}

// RedirectTarget — куда перенаправить нового клиента: узел перегружен
// (own >= soft_limit), а у живого соседа клиентов меньше. Из таких —
// наименее загруженный.
func (m *Manager) RedirectTarget(own int) (PeerNode, bool) {
	if m.cfg.SoftLimit <= 0 || own < m.cfg.SoftLimit {
		return PeerNode{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var best PeerNode
	bestLoad := int64(own)
	found := false
	for id, l := range m.links {
		load := l.peerClients.Load()
		if load < 0 || load >= bestLoad {
			continue
		}
		if p, ok := m.peers[id]; ok {
			best, bestLoad, found = p, load, true
		}
	}
	return best, found
}

// --- наблюдаемость ---

// LinkHealth — состояние линка с узлом для /healthz.
type LinkHealth struct {
	NodeID     string `json:"node_id"`
	Addr       string `json:"addr"`
	Alive      bool   `json:"alive"`
	RTTms      int64  `json:"rtt_ms"`       // -1 — ещё не измерен
	LastSeenMs int64  `json:"last_seen_ms"` // мс с последнего сообщения; -1 — линка нет
	Clients    int64  `json:"clients"`      // клиентов на узле по heartbeat; -1 — неизвестно
}

// Health — состояние кластера узла.
type Health struct {
	NodeID    string       `json:"node_id"`
	SoftLimit int          `json:"soft_limit"`
	DirSize   int          `json:"dir_size"`
	Links     []LinkHealth `json:"links"`
}

// Health — снимок для /healthz.
func (m *Manager) Health() Health {
	h := Health{NodeID: m.selfID, SoftLimit: m.cfg.SoftLimit, DirSize: m.dir.Size()}
	now := time.Now().UnixMilli()
	m.mu.Lock()
	for _, p := range m.cfg.Peers {
		lh := LinkHealth{NodeID: p.NodeID, Addr: p.Addr, RTTms: -1, LastSeenMs: -1, Clients: -1}
		if l := m.links[p.NodeID]; l != nil {
			lh.Alive = true
			lh.RTTms = l.rttMs.Load()
			lh.LastSeenMs = now - l.lastSeen.Load()
			lh.Clients = l.peerClients.Load()
		}
		h.Links = append(h.Links, lh)
	}
	m.mu.Unlock()
	sort.Slice(h.Links, func(i, j int) bool { return h.Links[i].NodeID < h.Links[j].NodeID })
	return h
}

func short(s string) string {
	if len(s) > 16 {
		return s[:16]
	}
	return s
}
