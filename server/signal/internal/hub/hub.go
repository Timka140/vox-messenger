package hub

import (
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"messenger_signal/pkg/protocol"
)

const (
	pingInterval = 30 * time.Second
	// pongTimeout — запас сверх pingInterval до признания пира мёртвым.
	// 30s (а не 10s): pong обрабатывается только между входящими сообщениями,
	// занятый readLoop клиента не должен приводить к ложному кику.
	pongTimeout = 30 * time.Second
	// offlinePolicyTTL — сколько помним политику отключившегося пира
	// (надгробие, docs/cluster-contract.md §3.3).
	offlinePolicyTTL = 24 * time.Hour
)

// sessionReplacedCloseDelay — пауза между ошибкой session_replaced и
// закрытием соединения, чтобы write pump успел её отправить.
var sessionReplacedCloseDelay = 300 * time.Millisecond

// offlinePolicy — политика отключившегося приватного пира и срок её жизни.
type offlinePolicy struct {
	pol   *presencePolicy
	until time.Time
}

// Hub — реестр подключённых пиров и маршрутизация сообщений.
//
// Статус «в сети» (peer_online / peer_offline / peer_offline_err) пира видят
// только те, кого он перечислил в watch-списке (presence в auth,
// presence_watch): друзья и участники общих групп. Старые клиенты без списка
// видны всем, как раньше.
//
// С кластером (SetCluster) пиры других узлов видны через каталог: сигналинг
// им пересылается узлу, presence приходит событиями OnDirEvent.
type Hub struct {
	mu    sync.RWMutex
	peers map[string]*Peer // publicKey → Peer
	// lastPolicy — политика отключившихся приватных пиров (24 ч): Route
	// решает по ней, отвечать ли peer_offline_err.
	lastPolicy map[string]offlinePolicy
	cluster    Cluster
	log        *slog.Logger
	done       chan struct{}
}

// New создаёт новый Hub
func New(log *slog.Logger) *Hub {
	return &Hub{
		peers:      make(map[string]*Peer),
		lastPolicy: make(map[string]offlinePolicy),
		log:        log,
		done:       make(chan struct{}),
	}
}

// SetCluster подключает кластер. Вызывать до приёма клиентов.
func (h *Hub) SetCluster(c Cluster) {
	h.mu.Lock()
	h.cluster = c
	h.mu.Unlock()
}

func (h *Hub) getCluster() Cluster {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cluster
}

// Register добавляет аутентифицированного пира
func (h *Hub) Register(peer *Peer) {
	h.mu.Lock()
	// Если пир уже был подключён — закрываем старое соединение
	if old, ok := h.peers[peer.PublicKey]; ok {
		h.log.Warn("пир переподключился, закрываю старое соединение",
			slog.String("public_key", peer.PublicKey))
		old.Close()
	}
	h.peers[peer.PublicKey] = peer
	delete(h.lastPolicy, peer.PublicKey)

	// Кому сообщить, что пир онлайн: только тем, кому виден его статус.
	others := make([]*Peer, 0, len(h.peers))
	for _, p := range h.peers {
		if p.PublicKey != peer.PublicKey && peer.VisibleTo(p.PublicKey) {
			others = append(others, p)
		}
	}
	c := h.cluster
	h.mu.Unlock()

	h.log.Info("пир зарегистрирован",
		slog.String("nick_tag", peer.NickTag()),
		slog.String("public_key", peer.PublicKey),
		slog.Bool("private", peer.Private()),
		slog.Int("notified", len(others)),
		slog.Int("total_peers", h.Count()),
	)

	h.broadcastStatus(peer.PublicKey, protocol.TypePeerOnline, others)
	if c != nil {
		c.Publish(peer.DirEntry(c.NodeID()))
	}
}

// Unregister удаляет пира из реестра.
// peer — конкретный Peer, чтобы не удалить новое соединение при реконнекте.
func (h *Hub) Unregister(peer *Peer) {
	h.mu.Lock()
	current, ok := h.peers[peer.PublicKey]
	// Удаляем только если в map хранится тот же peer (тот же Conn).
	// При реконнекте Register() уже заменил старый peer на новый —
	// defer старой горутины не должен удалять новое соединение.
	if !ok || current != peer {
		h.mu.Unlock()
		return
	}
	delete(h.peers, peer.PublicKey)
	// Политика офлайн-пира нужна Route (ответ peer_offline_err).
	if pol := peer.policy(); pol != nil && pol.private {
		h.lastPolicy[peer.PublicKey] = offlinePolicy{pol: pol, until: time.Now().Add(offlinePolicyTTL)}
	} else {
		delete(h.lastPolicy, peer.PublicKey)
	}
	c := h.cluster
	h.mu.Unlock()

	// Ключ уже работает на другом узле (сессию заменили) — для наблюдателей
	// он не уходил офлайн.
	movedAway := false
	if c != nil {
		c.Remove(peer.PublicKey, peer.SessionID)
		if e, found := c.Lookup(peer.PublicKey); found && e.SessionID != peer.SessionID {
			movedAway = true
		}
	}

	h.log.Info("пир отключился",
		slog.String("nick_tag", peer.NickTag()),
		slog.String("public_key", peer.PublicKey),
		slog.Bool("moved_to_other_node", movedAway),
		slog.Int("total_peers", h.Count()),
	)
	if movedAway {
		return
	}

	// Уведомляем тех, кому виден статус пира, что он ушёл
	h.mu.RLock()
	others := make([]*Peer, 0, len(h.peers))
	for _, p := range h.peers {
		if peer.VisibleTo(p.PublicKey) {
			others = append(others, p)
		}
	}
	h.mu.RUnlock()
	h.broadcastStatus(peer.PublicKey, protocol.TypePeerOffline, others)
}

// Route маршрутизирует сообщение к пиру по publicKey
func (h *Hub) Route(from string, msg *protocol.Message) error {
	h.mu.RLock()
	target, ok := h.peers[msg.To]
	fromPeer, fromOk := h.peers[from]
	c := h.cluster
	h.mu.RUnlock()

	fromNick := shortKey(from)
	if fromOk {
		fromNick = fromPeer.NickTag()
	}

	if !ok {
		// Адресат на другом узле кластера — пересылаем туда.
		if c != nil {
			if e, found := c.Lookup(msg.To); found && e.NodeID != c.NodeID() {
				msg.From = from
				if err := c.Forward(e.NodeID, from, msg); err == nil {
					h.log.Debug("📨 ROUTE → узел",
						slog.String("type", msg.Type),
						slog.String("from", fromNick),
						slog.String("to", e.NickTag),
						slog.String("node", shortKey(e.NodeID)))
					return nil
				}
				// Линка на узел нет (ждём grace) — для отправителя это «не в сети».
				if !EntryVisibleTo(&e, from) {
					return nil
				}
				return h.sendError(from, protocol.ErrPeerOffline, "пир не в сети", msg.To)
			}
		}

		// «Не в сети» — тоже статус: говорим только тем, кому он виден.
		// Остальным молча — незнакомец не отличит офлайн от «не ответил».
		if !h.offlineVisibleTo(msg.To, from) {
			h.log.Debug("📨 ROUTE: пир не в сети (статус скрыт от отправителя)",
				slog.String("type", msg.Type),
				slog.String("from", fromNick),
				slog.String("to", shortKey(msg.To)))
			return nil
		}
		h.log.Debug("📨 ROUTE: пир не в сети",
			slog.String("type", msg.Type),
			slog.String("from", fromNick),
			slog.String("to", shortKey(msg.To)))
		return h.sendError(from, protocol.ErrPeerOffline, "пир не в сети", msg.To)
	}

	toNick := target.NickTag()

	h.log.Debug("📨 ROUTE",
		slog.String("type", msg.Type),
		slog.String("from", fromNick),
		slog.String("to", toNick))

	// Подставляем отправителя
	msg.From = from

	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	if err := target.Send(data); err != nil {
		// Очередь пира переполнена или соединение уже закрыто — убираем его
		// из реестра сразу, не дожидаясь ping-цикла, и говорим отправителю,
		// что адресат offline (иначе offer/answer молча тонут).
		h.log.Warn("📨 ROUTE: пир не читает, отключаю",
			slog.String("to", toNick),
			slog.String("error", err.Error()))
		target.Close()
		h.Unregister(target)
		if !target.VisibleTo(from) {
			return nil
		}
		return h.sendError(from, protocol.ErrPeerOffline, "пир не в сети", msg.To)
	}
	return nil
}

// offlineVisibleTo — виден ли статус отключённого пира target отправителю
// from. Политика известна 24 ч после ухода (локального или на другом узле);
// неизвестна — отвечаем как раньше.
func (h *Hub) offlineVisibleTo(target, from string) bool {
	h.mu.RLock()
	lp, ok := h.lastPolicy[target]
	c := h.cluster
	h.mu.RUnlock()
	if ok && time.Now().Before(lp.until) {
		return lp.pol.visibleTo(from)
	}
	if c != nil {
		if e, found := c.Tombstone(target); found {
			return EntryVisibleTo(&e, from)
		}
	}
	return true
}

// DeliverForwarded — доставка сообщения, пришедшего с другого узла
// (node_fwd). Возвращает код для node_fwd_err или "" при успехе.
func (h *Hub) DeliverForwarded(from string, msg *protocol.Message) string {
	h.mu.RLock()
	target, ok := h.peers[msg.To]
	h.mu.RUnlock()
	if !ok {
		return protocol.ErrPeerOffline
	}
	msg.From = from
	data, err := json.Marshal(msg)
	if err != nil {
		return protocol.ErrInvalidMsg
	}
	if err := target.Send(data); err != nil {
		h.log.Warn("📨 ROUTE ← узел: пир не читает, отключаю",
			slog.String("to", target.NickTag()),
			slog.String("error", err.Error()))
		target.Close()
		h.Unregister(target)
		return protocol.ErrPeerOffline
	}
	h.log.Debug("📨 ROUTE ← узел",
		slog.String("type", msg.Type),
		slog.String("from", shortKey(from)),
		slog.String("to", target.NickTag()))
	return ""
}

// ForwardFailed — узел адресата ответил node_fwd_err: отправитель получает
// peer_offline_err, если ему виден статус адресата.
func (h *Hub) ForwardFailed(from, to string) {
	visible := true
	if c := h.getCluster(); c != nil {
		if e, found := c.Lookup(to); found {
			visible = EntryVisibleTo(&e, from)
		} else if e, found := c.Tombstone(to); found {
			visible = EntryVisibleTo(&e, from)
		}
	}
	if visible {
		_ = h.sendError(from, protocol.ErrPeerOffline, "пир не в сети", to)
	}
}

// ReplaceLocal — сессия пира на этом узле проиграла более новой сессии того
// же ключа на другом узле: клиент получает session_replaced, соединение
// закрывается. Unregister наблюдателям офлайн не шлёт — ключ жив там.
func (h *Hub) ReplaceLocal(pubKey, sessionID string) {
	h.mu.RLock()
	p, ok := h.peers[pubKey]
	h.mu.RUnlock()
	if !ok || p.SessionID != sessionID {
		return
	}
	h.log.Warn("сессия заменена сессией на другом узле",
		slog.String("nick_tag", p.NickTag()),
		slog.String("public_key", pubKey))
	payload, _ := json.Marshal(protocol.ErrorPayload{
		Code:    protocol.ErrSessionReplaced,
		Message: "этот ключ подключился к другому узлу",
	})
	data, _ := json.Marshal(protocol.Message{Type: protocol.TypeError, Payload: payload})
	_ = p.Send(data)
	time.AfterFunc(sessionReplacedCloseDelay, p.Close)
}

// OnDirEvent — изменение каталога, пришедшее с другого узла: peer_online /
// peer_offline локальным пирам, которым виден статус.
func (h *Hub) OnDirEvent(ev DirEvent) {
	h.mu.RLock()
	if _, local := h.peers[ev.Entry.PubKey]; local && ev.Kind == DirDel {
		// Ключ подключён здесь — для локальных он в сети.
		h.mu.RUnlock()
		return
	}
	locals := make([]*Peer, 0, len(h.peers))
	for _, p := range h.peers {
		if p.PublicKey != ev.Entry.PubKey {
			locals = append(locals, p)
		}
	}
	h.mu.RUnlock()

	now := entryVis(&ev.Entry)
	var online, offline []*Peer
	switch ev.Kind {
	case DirPut:
		for _, p := range locals {
			if now.has(p.PublicKey) {
				online = append(online, p)
			}
		}
	case DirDel:
		for _, p := range locals {
			if now.has(p.PublicKey) {
				offline = append(offline, p)
			}
		}
	case DirUpdate:
		was := entryVis(ev.Prev)
		for _, p := range locals {
			w, n := was.has(p.PublicKey), now.has(p.PublicKey)
			if n && !w {
				online = append(online, p)
			} else if w && !n {
				offline = append(offline, p)
			}
		}
	}

	h.log.Debug("каталог: событие с другого узла",
		slog.Int("kind", int(ev.Kind)),
		slog.String("nick_tag", ev.Entry.NickTag),
		slog.String("node", shortKey(ev.Entry.NodeID)),
		slog.Int("online", len(online)),
		slog.Int("offline", len(offline)))
	h.broadcastStatus(ev.Entry.PubKey, protocol.TypePeerOnline, online)
	h.broadcastStatus(ev.Entry.PubKey, protocol.TypePeerOffline, offline)
}

// UpdatePresence — новый watch-список пира (presence_watch). Тем, кому
// статус стал виден, — peer_online; тем, кому скрыт, — peer_offline (иначе
// бывший друг так и видел бы его в сети).
func (h *Hub) UpdatePresence(peer *Peer, keys []string) {
	before := peer.policy()
	peer.SetPresence(keys)

	h.mu.RLock()
	if cur, ok := h.peers[peer.PublicKey]; !ok || cur != peer {
		h.mu.RUnlock()
		return
	}
	var shown, hidden []*Peer
	for _, p := range h.peers {
		if p == peer {
			continue
		}
		was, now := before.visibleTo(p.PublicKey), peer.VisibleTo(p.PublicKey)
		if now && !was {
			shown = append(shown, p)
		} else if was && !now {
			hidden = append(hidden, p)
		}
	}
	c := h.cluster
	h.mu.RUnlock()

	h.log.Info("presence обновлён",
		slog.String("nick_tag", peer.NickTag()),
		slog.Int("watch", len(keys)),
		slog.Int("shown", len(shown)),
		slog.Int("hidden", len(hidden)))
	h.broadcastStatus(peer.PublicKey, protocol.TypePeerOnline, shown)
	h.broadcastStatus(peer.PublicKey, protocol.TypePeerOffline, hidden)
	if c != nil {
		c.Publish(peer.DirEntry(c.NodeID()))
	}
}

// SearchPeers ищет пира по точному nick#tag, по nick#shortID (первые 16 hex
// ключа) или по полному ключу. Подстрокой по нику не ищет: сервер знает
// только тех, кто в сети, и поиск показывал бы, кто сейчас онлайн.
// С кластером ищет и среди клиентов других узлов.
func (h *Hub) SearchPeers(query string, maxResults int) []protocol.PeerInfo {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return nil
	}

	// nick#shortID → shortID
	queryID := ""
	if idx := strings.Index(query, "#"); idx >= 0 {
		queryID = query[idx+1:]
	}
	match := func(nick, pub string) bool {
		nickLower := strings.ToLower(nick)
		pubLower := strings.ToLower(pub)
		return nickLower == query || pubLower == query || (len(queryID) >= 16 && strings.HasPrefix(pubLower, queryID))
	}

	var results []protocol.PeerInfo
	seen := make(map[string]struct{})

	h.mu.RLock()
	for _, peer := range h.peers {
		if len(results) >= maxResults {
			break
		}
		nick := peer.NickTag()
		if match(nick, peer.PublicKey) {
			results = append(results, protocol.PeerInfo{
				PublicKey: peer.PublicKey,
				NickTag:   nick,
				Online:    true,
			})
			seen[peer.PublicKey] = struct{}{}
		}
	}
	c := h.cluster
	h.mu.RUnlock()

	if c != nil {
		for _, e := range c.RemoteEntries() {
			if len(results) >= maxResults {
				break
			}
			if _, dup := seen[e.PubKey]; dup {
				continue
			}
			if match(e.NickTag, e.PubKey) {
				results = append(results, protocol.PeerInfo{
					PublicKey: e.PubKey,
					NickTag:   e.NickTag,
					Online:    true,
				})
				seen[e.PubKey] = struct{}{}
			}
		}
	}

	return results
}

// Count возвращает количество подключённых пиров
func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.peers)
}

// BroadcastToKeys отправляет data списку пиров (fan-out).
// Возвращает количество пиров, которым удалось отправить.
func (h *Hub) BroadcastToKeys(keys []string, data []byte) int {
	delivered := 0
	h.mu.RLock()
	targets := make([]*Peer, 0, len(keys))
	for _, k := range keys {
		if p, ok := h.peers[k]; ok {
			targets = append(targets, p)
		}
	}
	h.mu.RUnlock()

	for _, p := range targets {
		if err := p.Send(data); err == nil {
			delivered++
		}
	}
	return delivered
}

// SendToAll отправляет сообщение всем подключённым пирам
func (h *Hub) SendToAll(data []byte) {
	h.mu.RLock()
	peers := make([]*Peer, 0, len(h.peers))
	for _, p := range h.peers {
		peers = append(peers, p)
	}
	h.mu.RUnlock()

	for _, p := range peers {
		p.Send(data)
	}
}

// UpdateNickTag обновляет nick_tag уже зарегистрированного пира.
// Используется при смене ника без переподключения WS (чтобы не рвать P2P).
// Возвращает старый ник и true, если пир найден.
func (h *Hub) UpdateNickTag(publicKey, newNickTag string) (oldNickTag string, ok bool) {
	h.mu.RLock()
	peer, exists := h.peers[publicKey]
	c := h.cluster
	h.mu.RUnlock()
	if !exists {
		return "", false
	}
	oldNickTag = peer.NickTag()
	peer.SetNickTag(newNickTag)
	if c != nil {
		c.Publish(peer.DirEntry(c.NodeID()))
	}
	return oldNickTag, true
}

// PeerList возвращает список подключённых пиров
func (h *Hub) PeerList() []protocol.PeerInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()

	list := make([]protocol.PeerInfo, 0, len(h.peers))
	for _, p := range h.peers {
		list = append(list, protocol.PeerInfo{
			PublicKey: p.PublicKey,
			NickTag:   p.NickTag(),
			Online:    true,
		})
	}
	return list
}

// StartPingLoop запускает горутину для проверки живости соединений
func (h *Hub) StartPingLoop() {
	go func() {
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				h.pingAll()
				h.purgeOfflinePolicies()
			case <-h.done:
				return
			}
		}
	}()
}

// Stop останавливает Hub и закрывает все соединения
func (h *Hub) Stop() {
	close(h.done)

	h.mu.Lock()
	defer h.mu.Unlock()

	for key, peer := range h.peers {
		peer.Close()
		delete(h.peers, key)
	}

	h.log.Info("hub остановлен")
}

func (h *Hub) pingAll() {
	h.mu.RLock()
	peers := make([]*Peer, 0, len(h.peers))
	for _, p := range h.peers {
		peers = append(peers, p)
	}
	h.mu.RUnlock()

	deadline := time.Now().Add(-pingInterval - pongTimeout)
	for _, peer := range peers {
		if peer.LastPong().Before(deadline) {
			h.log.Warn("пир не ответил на ping, отключаю",
				slog.String("public_key", peer.PublicKey))
			peer.Close()
			h.Unregister(peer)
			continue
		}
		if err := peer.SendPing(); err != nil {
			h.log.Warn("ошибка отправки ping",
				slog.String("public_key", peer.PublicKey),
				slog.String("error", err.Error()))
			peer.Close()
			h.Unregister(peer)
		}
	}
}

// purgeOfflinePolicies — удаляет надгробия старше offlinePolicyTTL.
func (h *Hub) purgeOfflinePolicies() {
	now := time.Now()
	h.mu.Lock()
	for k, lp := range h.lastPolicy {
		if now.After(lp.until) {
			delete(h.lastPolicy, k)
		}
	}
	h.mu.Unlock()
}

func (h *Hub) broadcastStatus(pubKey, msgType string, targets []*Peer) {
	if len(targets) == 0 {
		return
	}
	msg, _ := json.Marshal(protocol.Message{
		Type: msgType,
		From: pubKey,
	})
	for _, p := range targets {
		p.Send(msg)
	}
}

func shortKey(s string) string {
	if len(s) > 16 {
		return s[:16]
	}
	return s
}

func (h *Hub) sendError(toPublicKey, code, message, peerID string) error {
	h.mu.RLock()
	peer, ok := h.peers[toPublicKey]
	h.mu.RUnlock()

	if !ok {
		return nil
	}

	payload, _ := json.Marshal(protocol.ErrorPayload{
		Code:    code,
		Message: message,
		PeerID:  peerID,
	})

	data, _ := json.Marshal(protocol.Message{
		Type:    protocol.TypeError,
		Payload: payload,
	})

	return peer.Send(data)
}
