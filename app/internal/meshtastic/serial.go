package meshtastic

import (
	"fmt"
	"log/slog"
	"time"

	"go.bug.st/serial"
	meshpb "vox_app/internal/meshtastic/meshpb"
)

// SerialConfig configures a serial-connected Meshtastic device (USB-CDC).
type SerialConfig struct {
	Port     string         // e.g. "COM5" / "/dev/ttyUSB0"
	BaudRate int            // default 115200 if zero
	PortNum  meshpb.PortNum // which portnum to surface; default PRIVATE_APP (256)
	Logger   *slog.Logger   // required

	// WakeDelay is how long to wait after open before sending want_config_id.
	// ESP32 auto-resets on DTR toggle; we wait for boot to complete. Default 4s.
	WakeDelay time.Duration

	// RecvBuffer is the channel buffer size for received envelopes. Default 64.
	RecvBuffer int

	// HopLimit is the mesh hop limit set on outgoing packets. Default 3.
	HopLimit uint32

	// Tracker, if non-nil, is fed every received FromRadio frame so it can
	// maintain a live snapshot of the mesh. Owned by caller.
	Tracker *NodeTracker
}

// NewSerialAdapter opens the serial port and returns an Adapter reading from it.
// The caller must Close() the returned adapter.
func NewSerialAdapter(cfg SerialConfig) (*Adapter, error) {
	if cfg.Logger == nil {
		return nil, fmt.Errorf("meshtastic.NewSerialAdapter: Logger is required")
	}
	if cfg.BaudRate == 0 {
		cfg.BaudRate = 115200
	}
	if cfg.WakeDelay == 0 {
		cfg.WakeDelay = 4 * time.Second
	}

	mode := &serial.Mode{
		BaudRate: cfg.BaudRate,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	}
	port, err := serial.Open(cfg.Port, mode)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", cfg.Port, err)
	}
	_ = port.SetDTR(false)
	_ = port.SetRTS(false)

	a := newAdapter(port, AdapterConfig{
		PortNum:    cfg.PortNum,
		Logger:     cfg.Logger,
		RecvBuffer: cfg.RecvBuffer,
		HopLimit:   cfg.HopLimit,
		Tracker:    cfg.Tracker,
	}, cfg.Port)

	a.log.Info("opened", "transport", "serial", "baud", cfg.BaudRate, "portnum", a.cfg.PortNum)
	go a.readLoop()

	// ESP32 auto-resets на DTR toggle — нужно подождать пока плата загрузится,
	// только после этого запрашиваем конфиг.
	go func(delay time.Duration) {
		select {
		case <-time.After(delay):
		case <-a.done:
			return
		}
		a.sendWantConfigId()
	}(cfg.WakeDelay)

	return a, nil
}
