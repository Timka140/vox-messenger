package vektor

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"vox_app/internal/services"
	"vox_app/internal/session"
	"vox_app/internal/storage"
	"os"
	"sync"
)

// VirtualPeer — абстракция участника P2P-сети с эмулируемым каналом.
type VirtualPeer struct {
	ID       string // hex-encoded ed25519 public key
	Nickname string

	Store     *storage.Storage
	Sess      *session.Session
	Network   *services.NetworkService
	Messaging *services.MessagingService
	Friends   *services.FriendsService
	Groups    *services.GroupsService

	channel *ChannelModel // эмулируемый канал
	chaos   *ChaosEngine  // инжектор сбоев

	mu        sync.Mutex
	connected map[string]bool
	sent      []CapturedMessage
	events    []CapturedEvent
}

// CapturedMessage — перехваченное P2P сообщение.
type CapturedMessage struct {
	To   string
	Data json.RawMessage
	Type string
}

// CapturedEvent — перехваченное Wails событие.
type CapturedEvent struct {
	Name string
	Data any
}

// PeerConfig — конфигурация VirtualPeer.
type PeerConfig struct {
	Nickname    string
	NetProfile  NetProfile
	ChannelSeed int64
	ChaosSeed   int64
}

// NewVirtualPeer создаёт изолированного пира с in-memory DB и эмулируемым каналом.
func NewVirtualPeer(cfg PeerConfig) (*VirtualPeer, error) {
	store, err := storage.OpenInMemory()
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	pubKey, privKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("keygen: %w", err)
	}

	sess := session.New()
	sess.Set(privKey, pubKey)

	peerID := hex.EncodeToString(pubKey)

	vp := &VirtualPeer{
		ID:        peerID,
		Nickname:  cfg.Nickname,
		Store:     store,
		Sess:      sess,
		channel:   NewChannel(cfg.NetProfile, cfg.ChannelSeed),
		chaos:     NewChaosEngine(cfg.ChaosSeed),
		connected: make(map[string]bool),
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	networkSvc := services.NewNetworkService(log, sess, store)
	networkSvc.SetEmitter(func(name string, data any) {
		vp.mu.Lock()
		vp.events = append(vp.events, CapturedEvent{Name: name, Data: data})
		vp.mu.Unlock()
	})
	networkSvc.SetTestConnected(func(pid string) bool {
		vp.mu.Lock()
		defer vp.mu.Unlock()
		return vp.connected[pid]
	})
	networkSvc.SetTestSender(vp.emulatedSender)

	vp.Network = networkSvc

	groupsSvc := services.NewGroupsService(log, sess, store, networkSvc)
	messagingSvc := services.NewMessagingService(log, sess, store, networkSvc, nil, groupsSvc, nil)
	friendsSvc := services.NewFriendsService(log, sess, store, networkSvc)
	networkSvc.Wire(friendsSvc, groupsSvc, nil)
	// Event bus для доставки pending group messages/actions при готовности P2P.
	// В production это делает main.go; здесь дублируем минимально для тестов,
	// где эта логика нужна.
	bus := services.NewEventBus(log)
	networkSvc.SetEventBus(bus)
	bus.Subscribe(services.TopicP2PReady, func(p any) {
		if pubKey, ok := p.(string); ok {
			groupsSvc.DeliverPendingGroupMessages(pubKey)
			groupsSvc.DeliverPendingGroupActions(pubKey)
		}
	})

	vp.Messaging = messagingSvc
	vp.Friends = friendsSvc
	vp.Groups = groupsSvc

	return vp, nil
}

// emulatedSender — sender с эмуляцией сети + chaos injection.
func (vp *VirtualPeer) emulatedSender(pid string, data []byte) error {
	vp.mu.Lock()
	isConnected := vp.connected[pid]
	vp.mu.Unlock()

	if !isConnected {
		return fmt.Errorf("peer %s not connected", safeShort(pid))
	}

	// Chaos injection
	failType, delay, shouldFail := vp.chaos.Check(pid)
	if shouldFail {
		return fmt.Errorf("chaos: %s for peer %s", failType, safeShort(pid))
	}
	if delay > 0 {
		// SlowResponse — не блокируем тесты, просто записываем задержку
		_ = delay
	}

	// Эмуляция сети
	out, _, ok := vp.channel.Transmit(data)
	if !ok {
		return fmt.Errorf("channel: packet lost to %s", safeShort(pid))
	}

	// MessageCorrupt — подменяем данные
	if failType == FailureMessageCorrupt {
		data = out // уже искажённые данные
	} else {
		data = out
	}

	var msg struct {
		Type string `json:"type"`
	}
	json.Unmarshal(data, &msg)

	vp.mu.Lock()
	vp.sent = append(vp.sent, CapturedMessage{To: pid, Data: data, Type: msg.Type})
	vp.mu.Unlock()

	return nil
}

// Close освобождает ресурсы.
func (vp *VirtualPeer) Close() {
	vp.Store.Close()
}

// ConnectTo устанавливает "связь" с другим пиром.
func (vp *VirtualPeer) ConnectTo(remotePeerID string) {
	vp.mu.Lock()
	defer vp.mu.Unlock()
	vp.connected[remotePeerID] = true
}

// DisconnectFrom разрывает "связь" с пиром.
func (vp *VirtualPeer) DisconnectFrom(remotePeerID string) {
	vp.mu.Lock()
	defer vp.mu.Unlock()
	vp.connected[remotePeerID] = false
}

// AddContact добавляет пира в контакты (БД).
func (vp *VirtualPeer) AddContact(peerID, nickname string) {
	vp.Store.DB().Exec(
		"INSERT OR REPLACE INTO contacts (peer_id, nickname, status) VALUES (?, ?, 'accepted')",
		peerID, nickname)
}

// Channel возвращает канал для настройки профиля.
func (vp *VirtualPeer) Channel() *ChannelModel { return vp.channel }

// Chaos возвращает инжектор сбоев для настройки.
func (vp *VirtualPeer) Chaos() *ChaosEngine { return vp.chaos }

// GetSent возвращает все перехваченные P2P сообщения.
func (vp *VirtualPeer) GetSent() []CapturedMessage {
	vp.mu.Lock()
	defer vp.mu.Unlock()
	cp := make([]CapturedMessage, len(vp.sent))
	copy(cp, vp.sent)
	return cp
}

// SentCountTo возвращает количество отправленных сообщений конкретному пиру.
func (vp *VirtualPeer) SentCountTo(peerID string) int {
	vp.mu.Lock()
	defer vp.mu.Unlock()
	count := 0
	for _, s := range vp.sent {
		if s.To == peerID {
			count++
		}
	}
	return count
}

// GetEvents возвращает все перехваченные Wails события.
func (vp *VirtualPeer) GetEvents() []CapturedEvent {
	vp.mu.Lock()
	defer vp.mu.Unlock()
	cp := make([]CapturedEvent, len(vp.events))
	copy(cp, vp.events)
	return cp
}

// ClearSent очищает перехваченные сообщения.
func (vp *VirtualPeer) ClearSent() {
	vp.mu.Lock()
	defer vp.mu.Unlock()
	vp.sent = nil
}

// ClearEvents очищает перехваченные события.
func (vp *VirtualPeer) ClearEvents() {
	vp.mu.Lock()
	defer vp.mu.Unlock()
	vp.events = nil
}

func safeShort(s string) string {
	if len(s) > 16 {
		return s[:16]
	}
	return s
}
