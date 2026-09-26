package testkit

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
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestEnv — изолированная среда: in-memory DB, сессия, все сервисы.
type TestEnv struct {
	T         *testing.T
	Store     *storage.Storage
	Sess      *session.Session
	Network   *services.NetworkService
	Messaging *services.MessagingService
	Friends   *services.FriendsService
	Groups    *services.GroupsService
	MyID      string // hex public key текущего "пользователя"
	PeerID    string // hex public key "собеседника"

	mu        sync.Mutex
	sent      []SentMessage
	events    []EmittedEvent
	connected map[string]bool
}

// SentMessage — перехваченное P2P сообщение.
type SentMessage struct {
	To   string
	Data json.RawMessage
	Type string // распарсенный type из P2PMessage
}

// EmittedEvent — перехваченное Wails событие.
type EmittedEvent struct {
	Name string
	Data any
}

// NewTestEnv создаёт изолированную среду с in-memory DB и тестовыми ключами.
func NewTestEnv(t *testing.T, peerConnected bool) *TestEnv {
	t.Helper()

	// In-memory DB
	store, err := storage.OpenInMemory()
	if err != nil {
		t.Fatal("open in-memory DB:", err)
	}

	// Генерируем тестовые ключи
	pubKey, privKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal("generate keys:", err)
	}

	sess := session.New()
	sess.Set(privKey, pubKey)

	myID := hex.EncodeToString(pubKey)

	// Генерируем peerID
	peerPub, _, _ := ed25519.GenerateKey(nil)
	peerID := hex.EncodeToString(peerPub)

	// Добавляем пира в контакты (accepted)
	store.DB().Exec(
		"INSERT INTO contacts (peer_id, nickname, status) VALUES (?, 'TestPeer', 'accepted')",
		peerID)

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	env := &TestEnv{
		T:         t,
		Store:     store,
		Sess:      sess,
		MyID:      myID,
		PeerID:    peerID,
		connected: make(map[string]bool),
	}

	if peerConnected {
		env.connected[peerID] = true
	}

	// Создаём NetworkService с mock хуками
	networkSvc := services.NewNetworkService(log, sess, store)
	networkSvc.SetEmitter(func(name string, data any) {
		env.mu.Lock()
		env.events = append(env.events, EmittedEvent{Name: name, Data: data})
		env.mu.Unlock()
	})
	networkSvc.SetTestConnected(func(pid string) bool {
		env.mu.Lock()
		defer env.mu.Unlock()
		return env.connected[pid]
	})
	networkSvc.SetTestSender(func(pid string, data []byte) error {
		env.mu.Lock()
		isConnected := env.connected[pid]
		env.mu.Unlock()
		if !isConnected {
			return fmt.Errorf("peer %s not connected", pid[:16])
		}
		var msg struct {
			Type string `json:"type"`
		}
		json.Unmarshal(data, &msg)
		env.mu.Lock()
		env.sent = append(env.sent, SentMessage{To: pid, Data: data, Type: msg.Type})
		env.mu.Unlock()
		return nil
	})

	env.Network = networkSvc

	// Создаём сервисы — порядок тот же что в main.go: сначала groups,
	// потом messaging (messaging принимает groups через конструктор).
	// voice/mesh в тестах не используются → nil.
	groupsSvc := services.NewGroupsService(log, sess, store, networkSvc)
	messagingSvc := services.NewMessagingService(log, sess, store, networkSvc, nil, groupsSvc, nil)
	friendsSvc := services.NewFriendsService(log, sess, store, networkSvc)
	networkSvc.Wire(friendsSvc, groupsSvc, nil)

	env.Messaging = messagingSvc
	env.Friends = friendsSvc
	env.Groups = groupsSvc

	return env
}

// Close очищает ресурсы.
func (e *TestEnv) Close() {
	e.Store.Close()
}

// SetPeerConnected меняет статус подключения пира.
func (e *TestEnv) SetPeerConnected(peerID string, connected bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.connected[peerID] = connected
}

// SendFromPeer имитирует входящее DM от собеседника.
func (e *TestEnv) SendFromPeer(content string) string {
	msgID := uuid.New().String()
	now := time.Now().Unix()
	p2pMsg := services.P2PMessage{
		Type:    "message",
		ID:      msgID,
		Content: content,
		Time:    now,
	}
	data, _ := json.Marshal(p2pMsg)
	e.Network.TestTriggerP2PMessage(e.PeerID, data)
	return msgID
}

// SendGroupFromPeer имитирует входящее групповое сообщение от собеседника.
func (e *TestEnv) SendGroupFromPeer(groupID, content string) string {
	msgID := uuid.New().String()
	now := time.Now().Unix()
	p2pMsg := services.P2PMessage{
		Type:    "group_message",
		ID:      msgID,
		GroupID: groupID,
		Content: content,
		Time:    now,
	}
	data, _ := json.Marshal(p2pMsg)
	e.Network.TestTriggerP2PMessage(e.PeerID, data)
	return msgID
}

// GetSent возвращает все перехваченные P2P сообщения.
func (e *TestEnv) GetSent() []SentMessage {
	e.mu.Lock()
	defer e.mu.Unlock()
	cp := make([]SentMessage, len(e.sent))
	copy(cp, e.sent)
	return cp
}

// GetEvents возвращает все перехваченные события.
func (e *TestEnv) GetEvents() []EmittedEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	cp := make([]EmittedEvent, len(e.events))
	copy(cp, e.events)
	return cp
}

// ClearSent очищает перехваченные сообщения.
func (e *TestEnv) ClearSent() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sent = nil
}

// ClearEvents очищает перехваченные события.
func (e *TestEnv) ClearEvents() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = nil
}

// AddPeer создаёт дополнительного пира (контакт accepted) и возвращает его peerID.
func (e *TestEnv) AddPeer(nickname string, connected bool) string {
	peerPub, _, _ := ed25519.GenerateKey(nil)
	peerID := hex.EncodeToString(peerPub)
	e.Store.DB().Exec(
		"INSERT INTO contacts (peer_id, nickname, status) VALUES (?, ?, 'accepted')",
		peerID, nickname)
	e.mu.Lock()
	e.connected[peerID] = connected
	e.mu.Unlock()
	return peerID
}

// SetCustomSender подменяет testSender для failure injection (ошибки, задержки, partial failure).
// Вызвать с nil чтобы вернуть стандартный sender.
func (e *TestEnv) SetCustomSender(fn func(pid string, data []byte) error) {
	if fn == nil {
		// Восстанавливаем стандартный sender
		e.Network.SetTestSender(func(pid string, data []byte) error {
			e.mu.Lock()
			isConnected := e.connected[pid]
			e.mu.Unlock()
			if !isConnected {
				return fmt.Errorf("peer %s not connected", pid[:16])
			}
			var msg struct {
				Type string `json:"type"`
			}
			json.Unmarshal(data, &msg)
			e.mu.Lock()
			e.sent = append(e.sent, SentMessage{To: pid, Data: data, Type: msg.Type})
			e.mu.Unlock()
			return nil
		})
		return
	}
	e.Network.SetTestSender(fn)
}

// SentCountTo возвращает количество P2P сообщений, отправленных конкретному пиру.
func (e *TestEnv) SentCountTo(peerID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	count := 0
	for _, s := range e.sent {
		if s.To == peerID {
			count++
		}
	}
	return count
}
