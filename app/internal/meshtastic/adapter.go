package meshtastic

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	meshpb "vox_app/internal/meshtastic/meshpb"
)

// AdapterConfig — общие настройки, не зависящие от транспорта.
// SerialConfig и TCPConfig встраивают эти поля + добавляют свою специфику.
type AdapterConfig struct {
	PortNum    meshpb.PortNum // which portnum to surface; default PRIVATE_APP (256)
	Logger     *slog.Logger   // required
	RecvBuffer int            // channel buffer for received envelopes; default 64
	HopLimit   uint32         // mesh hop limit on outgoing packets; default 3
	Tracker    *NodeTracker   // optional — fed every FromRadio frame
}

// Adapter обёртывает байтовый канал к Meshtastic-устройству (USB-CDC serial
// или TCP на порт 4403 при включённом WiFi на прошивке). Транспорт
// абстрагирован через io.ReadWriteCloser — framer/dispatcher/admin-каналы
// одинаковы для обоих вариантов. Конструкторы: NewSerialAdapter / NewTCPAdapter.
type Adapter struct {
	cfg  AdapterConfig
	conn io.ReadWriteCloser
	log  *slog.Logger
	name string // "COM5" / "192.168.1.42:4403" — для логов/Status

	recv  chan Envelope
	text  chan TextEnvelope
	admin chan *meshpb.AdminMessage // parsed AdminMessage responses on ADMIN_APP=6
	done  chan struct{}

	closeOnce sync.Once
	writeMu   sync.Mutex

	localNodeMu    sync.RWMutex
	localNode      uint32
	localNodeReady chan struct{} // closed on first MyInfo (localNode: 0 → non-zero)
	readyOnce      sync.Once
}

// TextEnvelope is a plaintext message received on TEXT_MESSAGE_APP=1, the
// standard Meshtastic portnum any vanilla client (phone app, web client)
// understands. Surfaced separately from Recv() because the use case is
// interop with non-Vox devices, not encrypted Vox traffic.
type TextEnvelope struct {
	MsgID  uint32 // meshtastic packet id (for dedup)
	Src    uint32
	Dst    uint32
	Text   string
	RxSnr  float32
	RxRssi int32
}

// newAdapter создаёт инициализированный *Adapter без какой-либо transport-
// специфики. Вызывается из NewSerialAdapter / NewTCPAdapter после того как
// соединение уже установлено.
func newAdapter(conn io.ReadWriteCloser, cfg AdapterConfig, name string) *Adapter {
	if cfg.PortNum == 0 {
		cfg.PortNum = meshpb.PortNum_PRIVATE_APP
	}
	if cfg.RecvBuffer == 0 {
		cfg.RecvBuffer = 64
	}
	if cfg.HopLimit == 0 {
		cfg.HopLimit = 3
	}
	return &Adapter{
		cfg:            cfg,
		conn:           conn,
		log:            cfg.Logger.With("peer", name),
		name:           name,
		recv:           make(chan Envelope, cfg.RecvBuffer),
		text:           make(chan TextEnvelope, cfg.RecvBuffer),
		admin:          make(chan *meshpb.AdminMessage, 8),
		done:           make(chan struct{}),
		localNodeReady: make(chan struct{}),
	}
}

// Send implements Transport.Send.
func (a *Adapter) Send(ctx context.Context, dst uint32, payload []byte) error {
	select {
	case <-a.done:
		return ErrClosed
	default:
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// Request a routing-layer ACK for unicast (dst != broadcast). Meshtastic
	// firmware will retransmit at the LoRa level if no ACK is heard within
	// its built-in window; on persistent failure we'll see a ROUTING_APP
	// packet with an error reason. For broadcast there's no ACK semantics —
	// every receiver would have to reply, which would flood the channel.
	wantAck := dst != BroadcastDst

	pkt := &meshpb.ToRadio{
		PayloadVariant: &meshpb.ToRadio_Packet{
			Packet: &meshpb.MeshPacket{
				To:       dst,
				Channel:  0,
				HopLimit: a.cfg.HopLimit,
				WantAck:  wantAck,
				PayloadVariant: &meshpb.MeshPacket_Decoded{
					Decoded: &meshpb.Data{
						Portnum: a.cfg.PortNum,
						Payload: payload,
					},
				},
			},
		},
	}
	if err := a.writeToRadio(pkt); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	a.log.Debug("sent",
		"dst", fmt.Sprintf("%#x", dst),
		"bytes", len(payload),
		"wantAck", wantAck)
	return nil
}

// Recv implements Transport.Recv.
func (a *Adapter) Recv() <-chan Envelope { return a.recv }

// LocalNode implements Transport.LocalNode.
func (a *Adapter) LocalNode() uint32 {
	a.localNodeMu.RLock()
	defer a.localNodeMu.RUnlock()
	return a.localNode
}

// Close implements Transport.Close.
func (a *Adapter) Close() error {
	var err error
	a.closeOnce.Do(func() {
		close(a.done)
		err = a.conn.Close()
		close(a.recv)
		close(a.text)
		close(a.admin)
		a.log.Info("closed")
	})
	return err
}

// RecvText returns a channel of plaintext messages received on the standard
// Meshtastic TEXT_MESSAGE_APP=1 portnum, regardless of the adapter's primary
// PortNum filter. Used for vanilla-Meshtastic interop.
func (a *Adapter) RecvText() <-chan TextEnvelope { return a.text }

// Tracker returns the NodeTracker passed in via Config. Nil if none
// was configured. Used by the service layer to retrieve the tracker after
// handing off the adapter to applyAdapter().
func (a *Adapter) Tracker() *NodeTracker { return a.cfg.Tracker }

// LocalNodeReady returns a channel that is closed once the radio has reported
// its own node number via FromRadio.MyInfo. Admin requests that use the own
// node as dst must wait on this channel — sending before LocalNode is known
// produces a packet with dst=0 which the firmware silently drops.
func (a *Adapter) LocalNodeReady() <-chan struct{} { return a.localNodeReady }

// RecvAdmin returns a channel of AdminMessage responses parsed from incoming
// ADMIN_APP=6 packets. Used for read-backs (GetConfig, GetDeviceConnectionStatus).
// Buffer is small (8) — consumers should drain promptly or responses get dropped.
func (a *Adapter) RecvAdmin() <-chan *meshpb.AdminMessage { return a.admin }

// SendText sends a plaintext message on TEXT_MESSAGE_APP=1 to dst (use
// BroadcastDst for broadcast). Anyone on the channel with a Meshtastic
// device — Vox or vanilla — will receive it.
func (a *Adapter) SendText(ctx context.Context, dst uint32, text string) error {
	select {
	case <-a.done:
		return ErrClosed
	default:
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	pkt := &meshpb.ToRadio{
		PayloadVariant: &meshpb.ToRadio_Packet{
			Packet: &meshpb.MeshPacket{
				To:       dst,
				Channel:  0,
				HopLimit: a.cfg.HopLimit,
				WantAck:  false,
				PayloadVariant: &meshpb.MeshPacket_Decoded{
					Decoded: &meshpb.Data{
						Portnum: meshpb.PortNum_TEXT_MESSAGE_APP,
						Payload: []byte(text),
					},
				},
			},
		},
	}
	if err := a.writeToRadio(pkt); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	a.log.Debug("sent text", "dst", fmt.Sprintf("%#x", dst), "bytes", len(text))
	return nil
}

func (a *Adapter) writeToRadio(msg *meshpb.ToRadio) error {
	body, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if len(body) > 0xFFFF {
		return fmt.Errorf("ToRadio too large: %d bytes", len(body))
	}
	frame := make([]byte, 0, 4+len(body))
	frame = append(frame, magic1, magic2, byte(len(body)>>8), byte(len(body)))
	frame = append(frame, body...)

	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	_, err = a.conn.Write(frame)
	return err
}

func (a *Adapter) readLoop() {
	parser := &frameParser{
		onFrame: a.handleFrame,
	}

	buf := make([]byte, 256)
	for {
		select {
		case <-a.done:
			return
		default:
		}
		n, err := a.conn.Read(buf)
		if err != nil {
			select {
			case <-a.done:
			default:
				a.log.Warn("read loop ended", "err", err)
			}
			return
		}
		if n == 0 {
			continue
		}
		parser.feed(buf[:n])
	}
}

func (a *Adapter) handleFrame(payload []byte) {
	var fr meshpb.FromRadio
	if err := proto.Unmarshal(payload, &fr); err != nil {
		a.log.Debug("decode frame failed", "err", err, "len", len(payload))
		return
	}

	switch v := fr.PayloadVariant.(type) {
	case *meshpb.FromRadio_MyInfo:
		a.localNodeMu.Lock()
		a.localNode = v.MyInfo.MyNodeNum
		a.localNodeMu.Unlock()
		if a.cfg.Tracker != nil {
			a.cfg.Tracker.SetLocalNode(v.MyInfo.MyNodeNum)
		}
		a.log.Info("local node", "nodenum", fmt.Sprintf("%#x", v.MyInfo.MyNodeNum))
		// Сигналим, что own nodeNum стал известен — все ожидания admin-запросов
		// разблокируются одновременно. Once — чтобы повторный MyInfo не дёргал.
		if v.MyInfo.MyNodeNum != 0 {
			a.readyOnce.Do(func() { close(a.localNodeReady) })
		}
	case *meshpb.FromRadio_Packet:
		a.dispatchPacket(v.Packet)
	}

	if a.cfg.Tracker != nil {
		a.cfg.Tracker.Apply(&fr)
	}
}

func (a *Adapter) dispatchPacket(p *meshpb.MeshPacket) {
	d := p.GetDecoded()
	if d == nil {
		return
	}
	// TEXT_MESSAGE_APP gets its own channel — interop with vanilla Meshtastic.
	if d.Portnum == meshpb.PortNum_TEXT_MESSAGE_APP {
		text := TextEnvelope{
			MsgID:  p.Id,
			Src:    p.From,
			Dst:    p.To,
			Text:   string(d.Payload),
			RxSnr:  p.RxSnr,
			RxRssi: p.RxRssi,
		}
		select {
		case a.text <- text:
		case <-a.done:
		default:
			a.log.Warn("text buffer full, dropping",
				"src", fmt.Sprintf("%#x", p.From),
				"bytes", len(d.Payload))
		}
		return
	}
	// ADMIN_APP = responses to GetConfig / GetDeviceConnectionStatus / etc.
	// Parsed once here, surfaced via RecvAdmin() for request/response helpers.
	if d.Portnum == meshpb.PortNum_ADMIN_APP {
		var am meshpb.AdminMessage
		if err := proto.Unmarshal(d.Payload, &am); err != nil {
			a.log.Debug("admin unmarshal failed", "err", err, "bytes", len(d.Payload))
			return
		}
		a.log.Debug("admin received",
			"variant", fmt.Sprintf("%T", am.PayloadVariant),
			"from", fmt.Sprintf("%#x", p.From),
			"bytes", len(d.Payload))
		select {
		case a.admin <- &am:
		case <-a.done:
		default:
			a.log.Warn("admin buffer full, dropping",
				"src", fmt.Sprintf("%#x", p.From))
		}
		return
	}
	if d.Portnum != a.cfg.PortNum {
		return
	}
	env := Envelope{
		Src:     p.From,
		Dst:     p.To,
		Payload: d.Payload,
		RxSnr:   p.RxSnr,
		RxRssi:  p.RxRssi,
	}
	select {
	case a.recv <- env:
	case <-a.done:
	default:
		a.log.Warn("recv buffer full, dropping",
			"src", fmt.Sprintf("%#x", p.From),
			"bytes", len(d.Payload))
	}
}

// sendWantConfigId кнопает прошивку стандартным запросом "шли мне текущий
// конфиг". Вызывается при запуске — адаптер получает MyInfo/NodeInfo/Config
// и заполняет localNode.
func (a *Adapter) sendWantConfigId() {
	if err := a.writeToRadio(&meshpb.ToRadio{
		PayloadVariant: &meshpb.ToRadio_WantConfigId{WantConfigId: 1},
	}); err != nil {
		a.log.Warn("want_config_id failed", "err", err)
		return
	}
	a.log.Debug("sent want_config_id")
}

// startHeartbeat шлёт ToRadio.Heartbeat каждый interval, пока адаптер жив.
// Нужен для TCP: прошивка Meshtastic считает TCP-клиента мёртвым если не
// получает heartbeat и закрывает сокет через ~15 секунд. На serial heartbeat
// не требуется — port автоматом чинится DTR/RTS-механикой, прошивка не
// разрывает UART-соединение по таймауту.
func (a *Adapter) startHeartbeat(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-a.done:
			return
		case <-ticker.C:
			err := a.writeToRadio(&meshpb.ToRadio{
				PayloadVariant: &meshpb.ToRadio_Heartbeat{Heartbeat: &meshpb.Heartbeat{}},
			})
			if err != nil {
				// Write-ошибка означает что connection уже мёртв — readLoop
				// тоже заметит это EOF'ом и прекратится, выходим.
				a.log.Debug("heartbeat failed, stopping", "err", err)
				return
			}
		}
	}
}

// ensure Adapter implements Transport at compile time.
var _ Transport = (*Adapter)(nil)
