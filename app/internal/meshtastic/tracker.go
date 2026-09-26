package meshtastic

import (
	"sort"
	"sync"
	"time"

	meshpb "vox_app/internal/meshtastic/meshpb"
)

// NodeSnapshot is the public view of a single mesh node.
// Marshal-friendly for Wails event payloads.
type NodeSnapshot struct {
	NodeNum   uint32    `json:"nodeNum"`            // Meshtastic node number (e.g. 0xacaaba94)
	NodeID    string    `json:"nodeId"`             // human form "!acaaba94" matching firmware convention
	LongName  string    `json:"longName,omitempty"` // user-set node name
	ShortName string    `json:"shortName,omitempty"`
	HwModel   string    `json:"hwModel,omitempty"`
	LastSeen  time.Time `json:"lastSeen"`
	RxSnr     float32   `json:"rxSnr"`           // last observed SNR (dB)
	RxRssi    int32     `json:"rxRssi"`          // last observed RSSI (dBm)
	HopsAway  uint32    `json:"hopsAway"`        // 0 = direct neighbor
	ViaMqtt   bool      `json:"viaMqtt"`         // last hop was MQTT gateway, not direct LoRa
	IsLocal   bool      `json:"isLocal"`         // this is our own radio
}

// NodeTracker keeps an in-memory map of all nodes the radio has seen since
// startup. It is safe for concurrent use. State is not persisted — meshes
// are ephemeral and the firmware rebuilds its own NodeDB on boot.
type NodeTracker struct {
	mu        sync.RWMutex
	nodes     map[uint32]*NodeSnapshot
	localNode uint32
	onChange  func(snap NodeSnapshot) // invoked outside the lock for any single-node update
}

// NewNodeTracker returns a fresh tracker. onChange may be nil; if set, it is
// called for every insert/update so callers (e.g. the Wails service) can fan
// out events.
func NewNodeTracker(onChange func(NodeSnapshot)) *NodeTracker {
	return &NodeTracker{
		nodes:    make(map[uint32]*NodeSnapshot),
		onChange: onChange,
	}
}

// SetLocalNode marks one node number as our own radio. Subsequent updates
// touching that node will be flagged IsLocal=true. Calling this clears any
// stale "remote" flag if the same node was seen before.
func (t *NodeTracker) SetLocalNode(num uint32) {
	t.mu.Lock()
	t.localNode = num
	if n, ok := t.nodes[num]; ok {
		n.IsLocal = true
	}
	t.mu.Unlock()
}

// Apply updates the tracker from a FromRadio frame. NodeInfo carries name
// and metadata; MeshPacket carries fresh signal metrics.
func (t *NodeTracker) Apply(fr *meshpb.FromRadio) {
	switch v := fr.PayloadVariant.(type) {
	case *meshpb.FromRadio_NodeInfo:
		t.applyNodeInfo(v.NodeInfo)
	case *meshpb.FromRadio_Packet:
		t.applyPacket(v.Packet)
	}
}

func (t *NodeTracker) applyNodeInfo(info *meshpb.NodeInfo) {
	if info == nil || info.Num == 0 {
		return
	}
	snap := t.upsert(info.Num)
	if u := info.User; u != nil {
		snap.LongName = u.LongName
		snap.ShortName = u.ShortName
		snap.HwModel = u.HwModel.String()
	}
	if info.LastHeard != 0 {
		snap.LastSeen = time.Unix(int64(info.LastHeard), 0)
	} else if snap.LastSeen.IsZero() {
		snap.LastSeen = time.Now()
	}
	snap.RxSnr = info.Snr
	snap.HopsAway = info.GetHopsAway()
	t.fireChange(snap)
}

func (t *NodeTracker) applyPacket(p *meshpb.MeshPacket) {
	if p == nil || p.From == 0 {
		return
	}
	snap := t.upsert(p.From)
	snap.LastSeen = time.Now()
	if p.RxSnr != 0 {
		snap.RxSnr = p.RxSnr
	}
	if p.RxRssi != 0 {
		snap.RxRssi = p.RxRssi
	}
	// Some firmware versions surface MQTT-gatewayed packets with hop_start
	// inflated and a "via_mqtt" debug marker we cannot read here. Best-effort:
	// rxRssi exactly 0 paired with non-zero rxSnr typically signals MQTT relay,
	// but we cannot rely on this so we leave ViaMqtt flag for future hooks.
	t.fireChange(snap)
}

// upsert returns the existing snapshot for num or creates a fresh one. Holds
// the lock for the duration; caller must call fireChange (which copies under
// lock and emits outside).
func (t *NodeTracker) upsert(num uint32) *NodeSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	n, ok := t.nodes[num]
	if !ok {
		n = &NodeSnapshot{
			NodeNum: num,
			NodeID:  FormatNodeID(num),
			IsLocal: num == t.localNode,
		}
		t.nodes[num] = n
	}
	return n
}

func (t *NodeTracker) fireChange(snap *NodeSnapshot) {
	if t.onChange == nil {
		return
	}
	t.mu.RLock()
	copy := *snap
	t.mu.RUnlock()
	t.onChange(copy)
}

// List returns a snapshot of all known nodes, sorted by LastSeen descending.
// Local node is always first. Safe for concurrent use.
func (t *NodeTracker) List() []NodeSnapshot {
	t.mu.RLock()
	out := make([]NodeSnapshot, 0, len(t.nodes))
	for _, n := range t.nodes {
		out = append(out, *n)
	}
	t.mu.RUnlock()

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].IsLocal != out[j].IsLocal {
			return out[i].IsLocal
		}
		return out[i].LastSeen.After(out[j].LastSeen)
	})
	return out
}

// Get returns a single node by number, or zero value + false if unknown.
func (t *NodeTracker) Get(num uint32) (NodeSnapshot, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	n, ok := t.nodes[num]
	if !ok {
		return NodeSnapshot{}, false
	}
	return *n, true
}

// Reset clears all tracked nodes. Used when adapter is closed and reopened.
func (t *NodeTracker) Reset() {
	t.mu.Lock()
	t.nodes = make(map[uint32]*NodeSnapshot)
	t.localNode = 0
	t.mu.Unlock()
}

// FormatNodeID renders 0xacaaba94 as "!acaaba94" matching Meshtastic firmware
// convention. Exported so callers in other packages can format node numbers
// the same way without reimplementing the helper.
func FormatNodeID(num uint32) string {
	const hex = "0123456789abcdef"
	b := []byte{'!', 0, 0, 0, 0, 0, 0, 0, 0}
	for i := range 8 {
		b[8-i] = hex[(num>>(i*4))&0xF]
	}
	return string(b)
}
