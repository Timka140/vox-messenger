package room

import (
	"log/slog"
	"sync"

	"github.com/gorilla/websocket"
)

// Member — участник комнаты.
type Member struct {
	PeerID     string
	SenderHash uint32
	Conn       *websocket.Conn
	WriteCh    chan []byte
	stopOnce   sync.Once
}

func (m *Member) Stop() {
	m.stopOnce.Do(func() { close(m.WriteCh) })
}

// Room — одна голосовая комната (соответствует groupID).
type Room struct {
	mu      sync.RWMutex
	members map[uint32]*Member // senderHash → Member
}

// Forward отправляет фрейм всем участникам кроме отправителя.
// Non-blocking: если writeCh заполнен — фрейм дропается (лучше чем backpressure).
func (r *Room) Forward(senderHash uint32, frame []byte) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for hash, m := range r.members {
		if hash == senderHash {
			continue
		}
		select {
		case m.WriteCh <- frame:
		default:
			// drop: получатель не успевает
		}
	}
}

// add ставит участника в комнату. Если по этому senderHash уже есть СТАРЫЙ
// member (реконнект: клиент открыл новое соединение, старое ещё не умерло по
// NAT-timeout) — старого останавливаем и рвём его conn. Без этого cleanup
// старого соединения позже выкидывал бы нового живого участника.
func (r *Room) add(m *Member) {
	r.mu.Lock()
	old, ok := r.members[m.SenderHash]
	r.members[m.SenderHash] = m
	r.mu.Unlock()
	if ok && old != m {
		old.Stop()
		old.Conn.Close()
	}
}

// remove удаляет участника ТОЛЬКО если в map лежит именно он (guard от
// реконнект-гонки: cleanup старого соединения не должен трогать нового
// участника с тем же senderHash). Возвращает (кол-во оставшихся, удалили ли).
func (r *Room) remove(m *Member) (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current, ok := r.members[m.SenderHash]
	if !ok || current != m {
		return len(r.members), false
	}
	m.Stop()
	delete(r.members, m.SenderHash)
	return len(r.members), true
}

func (r *Room) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.members)
}

// Manager управляет всеми комнатами.
type Manager struct {
	mu    sync.Mutex
	rooms map[string]*Room // roomID (groupID) → Room
	log   *slog.Logger
}

func NewManager(log *slog.Logger) *Manager {
	return &Manager{
		rooms: make(map[string]*Room),
		log:   log,
	}
}

// Join атомарно находит/создаёт комнату и добавляет участника. Единая
// критическая секция закрывает гонку GetOrCreate/Add против Leave: раньше
// между получением Room и Add последний участник мог выйти, TryRemove удалял
// комнату из map — и новый участник жил в комнате-сироте, которую Forward
// других пиров уже не находил.
func (mgr *Manager) Join(roomID string, m *Member) *Room {
	mgr.mu.Lock()
	r, ok := mgr.rooms[roomID]
	if !ok {
		r = &Room{members: make(map[uint32]*Member)}
		mgr.rooms[roomID] = r
		mgr.log.Info("room created", slog.String("room", roomID))
	}
	r.add(m)
	mgr.mu.Unlock()
	return r
}

// Leave атомарно удаляет участника (с проверкой идентичности) и, если комната
// опустела, убирает её из реестра. Возвращает число оставшихся участников.
func (mgr *Manager) Leave(roomID string, r *Room, m *Member) int {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	remaining, removed := r.remove(m)
	if removed && remaining == 0 {
		// Удаляем только если в map всё ещё эта комната (может быть пересоздана)
		if cur, ok := mgr.rooms[roomID]; ok && cur == r {
			delete(mgr.rooms, roomID)
			mgr.log.Info("room removed", slog.String("room", roomID))
		}
	}
	return remaining
}

func (mgr *Manager) Stats() (rooms, members int) {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	rooms = len(mgr.rooms)
	for _, r := range mgr.rooms {
		members += r.Len()
	}
	return
}
