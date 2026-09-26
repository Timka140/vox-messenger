package hub

import (
	"strings"

	"messenger_signal/pkg/protocol"
)

// Cluster — связь хаба с другими узлами кластера (docs/cluster-contract.md).
// nil — одиночный узел, поведение как до кластера.
//
// Hub вызывает методы Cluster только вне h.mu: реализация может звать обратно
// OnDirEvent / ReplaceLocal / DeliverForwarded / ForwardFailed.
type Cluster interface {
	NodeID() string
	// Publish — локальный пир прошёл auth или сменил ник / watch-список.
	Publish(e protocol.NodeDirEntry)
	// Remove — локальная сессия закрыта.
	Remove(pubKey, sessionID string)
	// Lookup — запись каталога (своего или другого узла).
	Lookup(pubKey string) (protocol.NodeDirEntry, bool)
	// RemoteEntries — записи клиентов других узлов (для поиска).
	RemoteEntries() []protocol.NodeDirEntry
	// Forward — отправить сообщение клиенту на узле nodeID. Ошибка — линка нет.
	Forward(nodeID, from string, msg *protocol.Message) error
	// Tombstone — запись недавно ушедшего клиента (для peer_offline_err).
	Tombstone(pubKey string) (protocol.NodeDirEntry, bool)
}

// DirEventKind — вид изменения каталога.
type DirEventKind int

const (
	DirPut    DirEventKind = iota + 1 // pubkey появился в кластере
	DirUpdate                         // сменились узел, сессия, ник или watch
	DirDel                            // pubkey пропал из кластера
)

// DirEvent — изменение каталога, пришедшее с другого узла.
type DirEvent struct {
	Kind  DirEventKind
	Entry protocol.NodeDirEntry
	Prev  *protocol.NodeDirEntry // для DirUpdate — прежняя запись
}

// EntryVisibleTo — виден ли статус владельца записи пиру key.
func EntryVisibleTo(e *protocol.NodeDirEntry, key string) bool {
	return entryVis(e).has(key)
}

// visSet — кому виден статус записи. Пустой visSet (nil-запись) не виден никому.
type visSet struct {
	public bool
	keys   map[string]struct{}
}

func entryVis(e *protocol.NodeDirEntry) visSet {
	if e == nil {
		return visSet{}
	}
	if !e.Private {
		return visSet{public: true}
	}
	keys := make(map[string]struct{}, len(e.Watch))
	for _, k := range e.Watch {
		keys[strings.ToLower(k)] = struct{}{}
	}
	return visSet{keys: keys}
}

func (v visSet) has(key string) bool {
	if v.public {
		return true
	}
	_, ok := v.keys[strings.ToLower(key)]
	return ok
}
