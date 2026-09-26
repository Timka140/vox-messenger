package cluster

import (
	"slices"
	"sync"
	"time"

	"messenger_signal/internal/hub"
	"messenger_signal/pkg/protocol"
)

// Directory — полная копия каталога кластера на узле (реализация fullrepl,
// docs/cluster-contract.md §3). Хранит записи своих и чужих клиентов и
// надгробия ушедших. События возвращаются вызывающему, сам каталог никого
// не зовёт — так нет вложенных блокировок.
type Directory struct {
	mu      sync.Mutex
	entries map[string]protocol.NodeDirEntry // pubkey → запись
	tombs   map[string]tomb                  // pubkey → запись ушедшего клиента
	now     func() time.Time
}

type tomb struct {
	entry protocol.NodeDirEntry
	until time.Time
}

// PutResult — итог применения записи.
type PutResult struct {
	Event    *hub.DirEvent          // nil — ничего не изменилось
	Replaced *protocol.NodeDirEntry // вытесненная сессия того же ключа
	Rejected bool                   // запись проиграла существующей сессии
}

// NewDirectory создаёт пустой каталог.
func NewDirectory() *Directory {
	return &Directory{
		entries: make(map[string]protocol.NodeDirEntry),
		tombs:   make(map[string]tomb),
		now:     time.Now,
	}
}

// newer — побеждает ли сессия a сессию b (§3.3): больший TSms, при равенстве
// больший NodeID.
func newer(a, b protocol.NodeDirEntry) bool {
	if a.TSms != b.TSms {
		return a.TSms > b.TSms
	}
	return a.NodeID > b.NodeID
}

func sameEntry(a, b protocol.NodeDirEntry) bool {
	return a.NickTag == b.NickTag && a.TSms == b.TSms && a.Private == b.Private &&
		slices.Equal(a.Watch, b.Watch)
}

// Put применяет запись: новый ключ — DirPut; та же сессия с другими полями —
// DirUpdate; другая сессия — по правилу newer (победитель — DirUpdate с
// Replaced, проигравший — Rejected).
func (d *Directory) Put(e protocol.NodeDirEntry) PutResult {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.put(e)
}

func (d *Directory) put(e protocol.NodeDirEntry) PutResult {
	old, ok := d.entries[e.PubKey]
	if !ok {
		d.entries[e.PubKey] = e
		delete(d.tombs, e.PubKey)
		return PutResult{Event: &hub.DirEvent{Kind: hub.DirPut, Entry: e}}
	}
	if old.NodeID == e.NodeID && old.SessionID == e.SessionID {
		if sameEntry(old, e) {
			return PutResult{}
		}
		d.entries[e.PubKey] = e
		prev := old
		return PutResult{Event: &hub.DirEvent{Kind: hub.DirUpdate, Entry: e, Prev: &prev}}
	}
	if !newer(e, old) {
		return PutResult{Rejected: true}
	}
	d.entries[e.PubKey] = e
	prev := old
	return PutResult{
		Event:    &hub.DirEvent{Kind: hub.DirUpdate, Entry: e, Prev: &prev},
		Replaced: &prev,
	}
}

// Del удаляет запись, если она принадлежит узлу node и сессии sessionID.
// Чужая сессия игнорируется (гонка «отключился старый, подключился новый»).
func (d *Directory) Del(node, pubKey, sessionID string) *hub.DirEvent {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.del(node, pubKey, sessionID)
}

func (d *Directory) del(node, pubKey, sessionID string) *hub.DirEvent {
	old, ok := d.entries[pubKey]
	if !ok || old.SessionID != sessionID || old.NodeID != node {
		return nil
	}
	delete(d.entries, pubKey)
	d.tombs[pubKey] = tomb{entry: old, until: d.now().Add(TombstoneTTL)}
	return &hub.DirEvent{Kind: hub.DirDel, Entry: old}
}

// RemoveNode удаляет все записи узла node (линк мёртв дольше grace).
func (d *Directory) RemoveNode(node string) []hub.DirEvent {
	d.mu.Lock()
	defer d.mu.Unlock()
	var evs []hub.DirEvent
	for pk, e := range d.entries {
		if e.NodeID == node {
			if ev := d.del(node, pk, e.SessionID); ev != nil {
				evs = append(evs, *ev)
			}
		}
	}
	return evs
}

// Snapshot заменяет все записи узла node его полным списком: отсутствующие
// в списке удаляются, остальные применяются через Put. Записи с чужим
// NodeID игнорируются — узел публикует только своих клиентов.
func (d *Directory) Snapshot(node string, entries []protocol.NodeDirEntry) []PutResult {
	d.mu.Lock()
	defer d.mu.Unlock()

	incoming := make(map[string]string, len(entries)) // pubkey → session
	for _, e := range entries {
		if e.NodeID == node {
			incoming[e.PubKey] = e.SessionID
		}
	}

	var res []PutResult
	for pk, e := range d.entries {
		if e.NodeID != node {
			continue
		}
		if sess, ok := incoming[pk]; ok && sess == e.SessionID {
			continue
		}
		if ev := d.del(node, pk, e.SessionID); ev != nil {
			res = append(res, PutResult{Event: ev})
		}
	}
	for _, e := range entries {
		if e.NodeID != node {
			continue
		}
		if r := d.put(e); r.Event != nil || r.Replaced != nil || r.Rejected {
			res = append(res, r)
		}
	}
	return res
}

// Lookup — запись ключа (любого узла).
func (d *Directory) Lookup(pubKey string) (protocol.NodeDirEntry, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.entries[pubKey]
	return e, ok
}

// Tombstone — запись ушедшего клиента, пока не истёк TombstoneTTL.
func (d *Directory) Tombstone(pubKey string) (protocol.NodeDirEntry, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	t, ok := d.tombs[pubKey]
	if !ok {
		return protocol.NodeDirEntry{}, false
	}
	if d.now().After(t.until) {
		delete(d.tombs, pubKey)
		return protocol.NodeDirEntry{}, false
	}
	return t.entry, true
}

// PurgeTombs удаляет истёкшие надгробия.
func (d *Directory) PurgeTombs() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	n := 0
	for pk, t := range d.tombs {
		if now.After(t.until) {
			delete(d.tombs, pk)
			n++
		}
	}
	return n
}

// ByNode — записи клиентов узла node (для snapshot).
func (d *Directory) ByNode(node string) []protocol.NodeDirEntry {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []protocol.NodeDirEntry
	for _, e := range d.entries {
		if e.NodeID == node {
			out = append(out, e)
		}
	}
	return out
}

// Except — записи всех узлов, кроме node (для поиска).
func (d *Directory) Except(node string) []protocol.NodeDirEntry {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []protocol.NodeDirEntry
	for _, e := range d.entries {
		if e.NodeID != node {
			out = append(out, e)
		}
	}
	return out
}

// Size — число записей.
func (d *Directory) Size() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.entries)
}
