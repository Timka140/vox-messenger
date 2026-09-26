package meshtastic

// Fragmentation layer for Vox-encrypted DMs over LoRa.
//
// Why this exists: a single Meshtastic LongFast packet carries at most
// ~200-230 bytes of payload. NaCl box adds 40 bytes of overhead, so a
// plaintext message is capped at ~150 bytes before it stops fitting. Voice
// notes and attachments need much more. This layer splits a single logical
// "message blob" into multiple Meshtastic packets and reassembles them at
// the receiver.
//
// Wire format (per Meshtastic packet):
//
//   | 0x01 | msgID(4 BE) | idx(1) | total(1) | data(N) |
//
// The 0x01 type byte distinguishes encrypted DMs from announcements (0x02)
// and reserves room for future packet types without protocol gymnastics.
//
// Integrity:
//   - The OUTER layer (NaCl box) authenticates the entire reassembled
//     ciphertext. If any fragment is corrupted, missing, or out of order
//     in a way that breaks the byte stream, Poly1305 fails and we drop
//     the message. No need for per-fragment CRC.
//   - msgID is 32-bit random; collision probability negligible for the
//     pending-window we keep open (≤30s) on a slow LoRa channel.

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"
)

// FragTypeEncryptedDM marks a fragment carrying part of an encrypted DM blob.
// Other type bytes are reserved for future Vox-over-mesh packet kinds
// (announcements, ACK envelopes, etc).
const FragTypeEncryptedDM byte = 0x01

// FragHeaderSize is the byte length of the per-fragment header
// (type+msgID+idx+total).
const FragHeaderSize = 7

// MaxFragments is the cap on total fragments for one logical message
// (uint8). 255 fragments at ~220 bytes each ≈ 56 KB max message.
const MaxFragments = 255

// reassemblyTimeout is how long an incomplete message stays in the pending
// table before we give up and drop fragments. 30s is generous for a slow
// LoRa channel — typical 5-fragment voice note completes in under 10s on
// LongFast.
const reassemblyTimeout = 30 * time.Second

// Fragment encodes one chunk for transmission. Returns the wire bytes ready
// to drop into a single Meshtastic Data.payload.
func encodeFragment(msgID uint32, idx, total uint8, data []byte) []byte {
	buf := make([]byte, FragHeaderSize+len(data))
	buf[0] = FragTypeEncryptedDM
	binary.BigEndian.PutUint32(buf[1:5], msgID)
	buf[5] = idx
	buf[6] = total
	copy(buf[7:], data)
	return buf
}

// FragmentBlob splits a complete encrypted blob into one or more fragments
// each fitting under maxPayload (the per-Meshtastic-packet limit). Returns
// a slice of wire-ready packet payloads.
func FragmentBlob(blob []byte, maxPayload int) ([][]byte, error) {
	if maxPayload <= FragHeaderSize {
		return nil, fmt.Errorf("maxPayload %d <= header size %d", maxPayload, FragHeaderSize)
	}
	chunkSize := maxPayload - FragHeaderSize
	total := (len(blob) + chunkSize - 1) / chunkSize
	if total == 0 {
		total = 1 // even empty blob gets one packet so receiver always sees the framing
	}
	if total > MaxFragments {
		return nil, fmt.Errorf("blob %d bytes needs %d fragments (max %d)",
			len(blob), total, MaxFragments)
	}

	var msgIDBytes [4]byte
	if _, err := rand.Read(msgIDBytes[:]); err != nil {
		return nil, fmt.Errorf("msgID rand: %w", err)
	}
	msgID := binary.BigEndian.Uint32(msgIDBytes[:])

	out := make([][]byte, 0, total)
	for i := 0; i < total; i++ {
		start := i * chunkSize
		end := start + chunkSize
		if end > len(blob) {
			end = len(blob)
		}
		out = append(out, encodeFragment(msgID, uint8(i), uint8(total), blob[start:end]))
	}
	return out, nil
}

// reassemblyEntry holds in-progress fragments for one msgID until complete or
// the timeout expires.
type reassemblyEntry struct {
	total     uint8
	received  int
	chunks    [][]byte // index = idx, nil = not yet received
	expiresAt time.Time
}

// Reassembler collects fragments by (sourceNode, msgID) and emits the
// reassembled blob when complete. Goroutine-safe.
//
// The (source, msgID) keying matters: two different senders could pick the
// same random msgID, and a Reassembler shared across multiple peers would
// blend their fragments into garbage. Per-source keying isolates them.
type Reassembler struct {
	mu      sync.Mutex
	pending map[reassemblyKey]*reassemblyEntry

	// onTimeout, if set, fires for every fragment group that expired
	// before completing. Caller can log these for "message lost" diagnostics
	// — without it there's no signal that LoRa dropped a fragment, just
	// silent absence.
	onTimeout func(src, msgID uint32, received, total int)
}

type reassemblyKey struct {
	src   uint32
	msgID uint32
}

// NewReassembler constructs an empty Reassembler. onTimeout may be nil.
func NewReassembler(onTimeout func(src, msgID uint32, received, total int)) *Reassembler {
	return &Reassembler{
		pending:   make(map[reassemblyKey]*reassemblyEntry),
		onTimeout: onTimeout,
	}
}

// Feed accepts one fragment from the wire. Returns the complete blob if this
// fragment was the last one needed, otherwise (nil, false). Stale entries
// (older than reassemblyTimeout) are evicted on every call — keeps the
// pending table from growing unbounded.
//
// Errors:
//   - ErrShortFragment: input too short to be a valid fragment
//   - ErrWrongType: type byte is not FragTypeEncryptedDM
//   - ErrInvalidFragment: idx >= total or total mismatch with prior fragments
//     of the same msgID
func (r *Reassembler) Feed(src uint32, frag []byte) ([]byte, bool, error) {
	if len(frag) < FragHeaderSize {
		return nil, false, ErrShortFragment
	}
	if frag[0] != FragTypeEncryptedDM {
		return nil, false, ErrWrongType
	}
	msgID := binary.BigEndian.Uint32(frag[1:5])
	idx := frag[5]
	total := frag[6]
	data := frag[7:]

	if total == 0 || idx >= total {
		return nil, false, ErrInvalidFragment
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.evictExpired()

	key := reassemblyKey{src: src, msgID: msgID}
	entry, ok := r.pending[key]
	if !ok {
		entry = &reassemblyEntry{
			total:     total,
			chunks:    make([][]byte, total),
			expiresAt: time.Now().Add(reassemblyTimeout),
		}
		r.pending[key] = entry
	} else if entry.total != total {
		// Two messages from same src reused the msgID — should be rare
		// (random uint32) but if it happens, we can't trust either. Drop.
		delete(r.pending, key)
		return nil, false, ErrInvalidFragment
	}

	if entry.chunks[idx] == nil {
		entry.chunks[idx] = make([]byte, len(data))
		copy(entry.chunks[idx], data)
		entry.received++
	}

	if entry.received < int(entry.total) {
		return nil, false, nil
	}

	// All fragments in — concatenate.
	totalLen := 0
	for _, c := range entry.chunks {
		totalLen += len(c)
	}
	out := make([]byte, 0, totalLen)
	for _, c := range entry.chunks {
		out = append(out, c...)
	}
	delete(r.pending, key)
	return out, true, nil
}

// evictExpired removes pending entries past their deadline. Caller holds
// r.mu. Cheap O(n) scan acceptable for the small expected pending count.
// Fires onTimeout for each evicted entry — this is the only signal we
// have that a multi-fragment message dropped on the floor.
func (r *Reassembler) evictExpired() {
	now := time.Now()
	for k, e := range r.pending {
		if now.After(e.expiresAt) {
			if r.onTimeout != nil {
				r.onTimeout(k.src, k.msgID, e.received, int(e.total))
			}
			delete(r.pending, k)
		}
	}
}

// PendingCount reports how many partial messages are currently in flight.
// Useful for health metrics; should hover near zero on a healthy mesh.
func (r *Reassembler) PendingCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evictExpired()
	return len(r.pending)
}

// Errors surfaced by Feed().
var (
	ErrShortFragment   = errors.New("fragment shorter than header")
	ErrWrongType       = errors.New("fragment type byte not FragTypeEncryptedDM")
	ErrInvalidFragment = errors.New("fragment idx/total inconsistent")
)
