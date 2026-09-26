// Package meshtastic provides a Transport abstraction over Meshtastic radios.
//
// Upper layers send opaque byte payloads; the transport handles serial
// framing, Meshtastic protobuf encoding, and PortNum filtering. Encryption
// and message semantics are the caller's concern — Vox messages travelling
// over LoRa are NOT covered by WebRTC's DTLS, so the chat layer must encrypt
// payloads before handing them to Send().
package meshtastic

import (
	"context"
	"errors"
)

// BroadcastDst is the Meshtastic broadcast destination node number.
const BroadcastDst uint32 = 0xFFFFFFFF

// ErrClosed is returned by Send on a closed Transport.
var ErrClosed = errors.New("mesh: transport closed")

// Transport carries opaque application payloads between nodes via LoRa.
type Transport interface {
	// Send delivers payload to dst. Use BroadcastDst for broadcast.
	// Returns after the packet is handed to firmware (not when delivered
	// over air). Context cancellation aborts the write.
	Send(ctx context.Context, dst uint32, payload []byte) error

	// Recv returns a channel yielding received Envelopes matching the
	// configured PortNum. Closed when Transport is Close()d.
	Recv() <-chan Envelope

	// LocalNode returns this radio's node number, or 0 if not yet known.
	LocalNode() uint32

	// Close shuts down the transport. Idempotent.
	Close() error
}

// Envelope carries a received payload with radio metadata.
type Envelope struct {
	Src     uint32  // source node number
	Dst     uint32  // destination (our node num, or BroadcastDst)
	Payload []byte  // application payload (opaque bytes)
	RxSnr   float32 // LoRa SNR in dB
	RxRssi  int32   // LoRa RSSI in dBm
}
