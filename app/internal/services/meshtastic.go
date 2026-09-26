package services

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"vox_app/internal/meshtastic"
	meshpb "vox_app/internal/meshtastic/meshpb"
	"vox_app/internal/session"
	"vox_app/internal/storage"
)

const (
	settingMeshEnabled      = "meshtastic_enabled"
	settingMeshTransport    = "meshtastic_transport" // "serial" | "tcp"
	settingMeshPort         = "meshtastic_port"      // COM5 / 192.168.1.42[:4403]
	settingMeshPortnum      = "meshtastic_portnum"
	settingMeshWiFiNetworks = "meshtastic_wifi_networks"
)

const (
	MeshTransportSerial = "serial"
	MeshTransportTCP    = "tcp"
)

// Сколько WiFi-сетей устройства хранить в рамках одной Vox-базы.
// Лимит защищает settings от распухания, если пользователь прошёлся
// по десяткам разных точек, но нормальные use-case'ы (дом/работа/телефон)
// с запасом укладываются.
const maxSavedMeshWiFi = 10

// MeshtasticService is the Wails-bound surface over a single LoRa radio.
//
// Only one radio is supported per app instance. Enable() opens the serial
// port; Disable() closes it. While enabled, every received frame updates an
// internal NodeTracker and is fanned out to the frontend via the
// "meshtastic:node-update" event.
//
// Send() of arbitrary Vox payloads over LoRa is intentionally NOT exposed
// from this service yet — chat-over-mesh requires app-layer encryption and
// is the M6 design problem.
type MeshtasticService struct {
	log   *slog.Logger
	store *storage.Storage
	sess  *session.Session

	mu          sync.RWMutex
	adapter     *meshtastic.Adapter
	tracker     *meshtastic.NodeTracker
	reassembler *meshtastic.Reassembler // collects multi-fragment encrypted DMs
	transport   string                  // "serial" | "tcp"; "" если disabled
	port        string                  // currently-open addr: COM5 / host[:port]; "" если disabled
	portnum     int                     // configured Meshtastic portnum (default 256)
	cancel      context.CancelFunc

	// Reconnect-state: когда после admin-команды (SetWiFi и т.п.) устройство
	// программно ребутится, мы закрываем адаптер и пытаемся автоматически
	// переоткрыть порт. На это время adapter == nil, но UI показывает
	// "reconnecting" — пользователь не должен включать руками.
	reconnecting     bool
	reconnectAttempt int
	reconnectMax     int
	reconnectCancel  context.CancelFunc

	// Последний снимок WiFi-статуса устройства — показывается в settings под Apply.
	// Обновляется после Enable и после успешного reconnect.
	wifiStatus *MeshWiFiStatus
	// Тот же паттерн для BT-статуса. Приходит в одном и том же DeviceConnectionStatus.
	btStatus *MeshBtStatus
	// LED+Display-настройки, читаются через QueryConfig(DEVICE_CONFIG / DISPLAY_CONFIG).
	deviceStatus *MeshDeviceStatus

	emit func(eventName string, data any)

	// onDecrypted, if set, is invoked when an encrypted DM addressed to us
	// is successfully decrypted. Wired by MessagingService to its existing
	// P2P-handler so mesh-DMs join the same chat history with no special
	// case in the chat layer.
	onDecrypted func(fromPeerID string, plaintext []byte)
}

// MeshStatus is the public snapshot of the radio link state.
type MeshStatus struct {
	Enabled bool `json:"enabled"`

	// Reconnecting=true значит adapter сейчас закрыт и идёт попытка
	// переоткрыть порт после программно вызванного reboot устройства
	// (например, SetWiFi). UI должен считать это "вроде включён" —
	// nav-таб и network-секция не скрываются.
	Reconnecting     bool `json:"reconnecting,omitempty"`
	ReconnectAttempt int  `json:"reconnectAttempt,omitempty"`
	ReconnectMax     int  `json:"reconnectMax,omitempty"`

	Transport string            `json:"transport,omitempty"` // "serial" | "tcp"
	Port      string            `json:"port,omitempty"`      // COM5 / host[:port]
	NodeNum   uint32            `json:"localNodeNum,omitempty"`
	NodeID    string            `json:"localNodeId,omitempty"`
	HwModel   string            `json:"hwModel,omitempty"`
	NumNodes  int               `json:"nodesSeen"`
	WiFi      *MeshWiFiStatus   `json:"wifi,omitempty"`
	Bluetooth *MeshBtStatus     `json:"bluetooth,omitempty"`
	Device    *MeshDeviceStatus `json:"device,omitempty"`
}

// MeshDeviceStatus — снимок LED/дисплей-настроек устройства, прочитанных
// через AdminMessage.GetConfigResponse(DEVICE_CONFIG / DISPLAY_CONFIG).
// LedHeartbeat — удобная инверсия: true = мигает (LedHeartbeatDisabled=false).
type MeshDeviceStatus struct {
	LedHeartbeat bool   `json:"ledHeartbeat"`
	ScreenOnSecs uint32 `json:"screenOnSecs"`
	WakeOnTap    bool   `json:"wakeOnTap"`
}

// MeshBtStatus — последний снимок BT-состояния устройства.
//   - Enabled / Mode берутся из прочитанного Config_BluetoothConfig (source of
//     truth для "включён/выключен" и "какой режим спаривания").
//   - Connected — из BluetoothConnectionStatus (runtime: есть ли активная
//     BLE-сессия с клиентом).
type MeshBtStatus struct {
	Enabled   bool   `json:"enabled"`
	Mode      string `json:"mode,omitempty"`
	Connected bool   `json:"connected"`
}

// MeshWiFiStatus — последний снимок WiFi-состояния устройства.
//   - Enabled / SSID читаются из Config.Network (source of truth);
//   - IP / Rssi / Connected — runtime из DeviceConnectionStatus.
type MeshWiFiStatus struct {
	Enabled   bool   `json:"enabled"`
	SSID      string `json:"ssid,omitempty"`
	IP        string `json:"ip,omitempty"`
	Rssi      int32  `json:"rssi,omitempty"`
	Connected bool   `json:"connected"`
}

// MeshTestResult is returned by TestConnection.
type MeshTestResult struct {
	OK       bool   `json:"ok"`
	NodeNum  uint32 `json:"nodeNum,omitempty"`
	NodeID   string `json:"nodeId,omitempty"`
	HwModel  string `json:"hwModel,omitempty"`
	Firmware string `json:"firmware,omitempty"`
	Error    string `json:"error,omitempty"`
}

// NewMeshtasticService constructs the service. The radio is closed on start;
// caller must invoke Enable() (or AutoStart() to use stored config).
func NewMeshtasticService(log *slog.Logger, store *storage.Storage, sess *session.Session) *MeshtasticService {
	return &MeshtasticService{
		log:     log.With("service", "meshtastic"),
		store:   store,
		sess:    sess,
		portnum: int(meshpb.PortNum_PRIVATE_APP),
	}
}

// SetOnDecryptedMessage wires the callback for successfully-decrypted DMs.
// MessagingService passes its handleIncomingP2P here at boot, so mesh-DM
// receipts feed into the same code path as P2P receipts — same dedup, same
// storage, same UI events. Caller should set this once.
func (s *MeshtasticService) SetOnDecryptedMessage(fn func(fromPeerID string, plaintext []byte)) {
	s.mu.Lock()
	s.onDecrypted = fn
	s.mu.Unlock()
}

// SendEncryptedDM encrypts plaintext for the given Vox peer and sends it as a
// unicast packet on PRIVATE_APP=256 to the friend's mapped Meshtastic node.
//
// Errors:
//   - ErrMeshDisabled if the radio is closed
//   - "no mesh node mapped for peer" if SetPeerMeshNode hasn't been called
//   - "ciphertext too large" if encrypted blob exceeds single-packet limit
//     (fragmentation lands in M6.3)
func (s *MeshtasticService) SendEncryptedDM(peerID string, plaintext []byte) error {
	if peerID == "" {
		return errors.New("peerID is empty")
	}
	mapping, _ := s.GetPeerMeshNode(peerID)
	if mapping.NodeNum == 0 {
		return fmt.Errorf("no mesh node mapped for peer %s — set it in their profile first",
			safeShort(peerID))
	}

	peerPub, err := hex.DecodeString(peerID)
	if err != nil || len(peerPub) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid peerID (expected hex Ed25519 pubkey): %w", err)
	}

	mySec, err := s.sess.PrivateKey()
	if err != nil || len(mySec) == 0 {
		return fmt.Errorf("identity not loaded: %w", err)
	}

	ciphertext, err := meshtastic.EncryptForPeer(ed25519.PublicKey(peerPub), mySec, plaintext)
	if err != nil {
		return fmt.Errorf("encrypt: %w", err)
	}

	// Fragment the ciphertext for transmission. Each fragment fits in one
	// LoRa packet; receiver reassembles before decrypt. Per-fragment
	// payload limit accounts for Data.payload max minus the fragmentation
	// header (7 bytes).
	const maxFragmentPayload = 220 // conservative LongFast Data.payload limit
	fragments, err := meshtastic.FragmentBlob(ciphertext, maxFragmentPayload)
	if err != nil {
		return fmt.Errorf("fragment: %w", err)
	}

	s.mu.RLock()
	adapter := s.adapter
	s.mu.RUnlock()
	if adapter == nil {
		return ErrMeshDisabled
	}
	for i, frag := range fragments {
		if err := adapter.Send(context.Background(), mapping.NodeNum, frag); err != nil {
			return fmt.Errorf("transport send fragment %d/%d: %w", i+1, len(fragments), err)
		}
	}
	s.log.Info("encrypted DM sent",
		"peer", safeShort(peerID),
		"nodenum", fmt.Sprintf("%#x", mapping.NodeNum),
		"plaintext_bytes", len(plaintext),
		"ciphertext_bytes", len(ciphertext),
		"fragments", len(fragments))
	return nil
}

// SetWiFi применяет WiFi-конфиг на подключённом устройстве через Meshtastic
// AdminMessage.SetConfig.Network. После успешной отправки устройство
// автоматически перезагружается (так ведёт себя прошивка при смене
// NetworkConfig), поэтому адаптер закрывается — пользователь увидит
// mesh-toggle в off и вручную включит mesh заново через ~10 секунд.
//
// При enabled=false SSID/PSK игнорируются и могут быть пустыми — это штатный
// способ отключить WiFi на ноде и оставить только COM/BT-доступ.
//
// Ограничения (проверяются здесь, фронт дублирует для UX):
//   - SSID ≤ 32 символа (ограничение 802.11)
//   - PSK ≥ 8 и ≤ 63 символа (WPA2)
func (s *MeshtasticService) SetWiFi(enabled bool, ssid, psk string) error {
	if enabled {
		if ssid == "" {
			return errors.New("SSID is required when WiFi enabled")
		}
		if len(ssid) > 32 {
			return fmt.Errorf("SSID too long (%d chars, max 32)", len(ssid))
		}
		if len(psk) < 8 || len(psk) > 63 {
			return fmt.Errorf("PSK must be 8..63 chars (WPA2), got %d", len(psk))
		}
	}

	s.mu.RLock()
	adapter := s.adapter
	s.mu.RUnlock()
	if adapter == nil {
		return ErrMeshDisabled
	}

	// Между reboot→reconnect и приходом MyInfo у нас ~5-секундное окно:
	// adapter живой, но dst=0. Если user быстро кликает Apply — вместо ошибки
	// ждём MyInfo до 8 секунд. На практике это ~1 секунда.
	if adapter.LocalNode() == 0 {
		select {
		case <-adapter.LocalNodeReady():
		case <-time.After(8 * time.Second):
			return errors.New("local node info not received yet — wait a few seconds after enable")
		}
	}
	dst := adapter.LocalNode()

	// Батч-транзакция. При enable WiFi **всегда** первым SetConfig тушим BT —
	// на ESP32 радио одно, оба одновременно работать не могут. Делаем это
	// безусловно (не проверяя текущий s.btStatus), потому что тот мог быть
	// устаревшим после свежего reboot (race с queryDeviceConnectionStatus).
	// Лишний disable-false idempotent — прошивка просто подтвердит off.
	changes := make([]*meshpb.AdminMessage, 0, 2)
	if enabled {
		changes = append(changes, &meshpb.AdminMessage{
			PayloadVariant: &meshpb.AdminMessage_SetConfig{
				SetConfig: &meshpb.Config{
					PayloadVariant: &meshpb.Config_Bluetooth{
						Bluetooth: &meshpb.Config_BluetoothConfig{Enabled: false},
					},
				},
			},
		})
	}
	changes = append(changes, &meshpb.AdminMessage{
		PayloadVariant: &meshpb.AdminMessage_SetConfig{
			SetConfig: &meshpb.Config{
				PayloadVariant: &meshpb.Config_Network{
					Network: &meshpb.Config_NetworkConfig{
						WifiEnabled: enabled,
						WifiSsid:    ssid,
						WifiPsk:     psk,
					},
				},
			},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := adapter.SendAdminBatch(ctx, dst, changes); err != nil {
		return fmt.Errorf("send admin batch: %w", err)
	}

	// Не логируем PSK.
	s.log.Info("wifi configured, device will reboot",
		"enabled", enabled,
		"ssid", ssid,
		"bt_auto_off", enabled, // при enable мы всегда шлём disable BT в том же batch
		"dst", fmt.Sprintf("%#x", dst))

	// Автосохраняем сеть в журнал, чтобы пользователь мог выбрать её
	// из списка в следующий раз вместо повторного ввода PSK. Делаем это
	// только для включённых сетей (enabled=false — это просто "выключи WiFi",
	// сохранять нечего).
	if enabled {
		if err := s.SaveMeshWiFiNetwork(ssid, psk); err != nil {
			s.log.Warn("save wifi network failed", "ssid", ssid, "err", err)
		}
	}

	// Устройство сейчас перезагрузится — serial дропнется. Вместо того чтобы
	// закрывать и просить пользователя включить вручную, ставим задачу
	// "scheduleReconnect": закрываем текущий адаптер, помечаем reconnecting,
	// фоновая горутина будет переоткрывать порт пока устройство не поднимется
	// или пока не закончатся попытки.
	s.mu.RLock()
	transport := s.transport
	port := s.port
	s.mu.RUnlock()
	if port == "" {
		// Чрезвычайный случай: не знаем адрес — нечего переподключать.
		// Закрываем адаптер штатно.
		_ = s.closeAdapter("config applied, device rebooting")
		return nil
	}
	s.scheduleReconnect(transport, port)
	return nil
}

// SetBluetooth применяет Bluetooth-конфиг на устройстве через AdminMessage.
// После успешной отправки устройство автоматически перезагружается — запускается
// тот же scheduleReconnect flow, что и для SetWiFi (reboot → re-open порта →
// queryDeviceConnectionStatus обновит UI с актуальным BT-статусом).
//
// mode ∈ {"random", "fixed", "no_pin"}:
//   - random  — прошивка генерирует PIN при каждом запросе спаривания и показывает
//     его на OLED (дефолт Meshtastic).
//   - fixed   — используется заранее заданный fixedPin (для киосков/headless-нод).
//   - no_pin  — открытое спаривание; небезопасно, но простое.
//
// При enabled=false режим/PIN передаются прошивке, но фактически BT выключен.
// PIN не логируем даже в DEBUG.
func (s *MeshtasticService) SetBluetooth(enabled bool, mode string, fixedPin uint32) error {
	var pairingMode meshpb.Config_BluetoothConfig_PairingMode
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "random":
		pairingMode = meshpb.Config_BluetoothConfig_RANDOM_PIN
	case "fixed":
		pairingMode = meshpb.Config_BluetoothConfig_FIXED_PIN
	case "no_pin", "nopin", "none":
		pairingMode = meshpb.Config_BluetoothConfig_NO_PIN
	default:
		return fmt.Errorf("unknown pairing mode %q (expected random/fixed/no_pin)", mode)
	}

	// fixedPin имеет смысл только для FIXED_PIN. Границы: Meshtastic использует
	// 6-значный PIN, но прошивка принимает uint32; ограничиваем до 999999
	// чтобы не сбивать отображение на OLED.
	if pairingMode == meshpb.Config_BluetoothConfig_FIXED_PIN {
		if !enabled {
			return errors.New("fixed PIN requires BT to be enabled")
		}
		if fixedPin > 999999 {
			return fmt.Errorf("fixed PIN out of range (0..999999), got %d", fixedPin)
		}
	} else {
		// Для других режимов fixedPin не используется — обнуляем, чтобы не
		// залипал в EEPROM.
		fixedPin = 0
	}

	s.mu.RLock()
	adapter := s.adapter
	s.mu.RUnlock()
	if adapter == nil {
		return ErrMeshDisabled
	}
	// Между reboot→reconnect и приходом MyInfo у нас ~5-секундное окно:
	// adapter живой, но dst=0. Если user быстро кликает Apply — вместо ошибки
	// ждём MyInfo до 8 секунд. На практике это ~1 секунда.
	if adapter.LocalNode() == 0 {
		select {
		case <-adapter.LocalNodeReady():
		case <-time.After(8 * time.Second):
			return errors.New("local node info not received yet — wait a few seconds after enable")
		}
	}
	dst := adapter.LocalNode()

	// Батч-транзакция. При enable BT **всегда** первым SetConfig тушим WiFi —
	// ESP32 радио одно. Безусловно, чтобы не зависеть от устаревшего
	// s.wifiStatus сразу после reboot (race с queryDeviceConnectionStatus).
	// disable-false идемпотентен.
	changes := make([]*meshpb.AdminMessage, 0, 2)
	if enabled {
		changes = append(changes, &meshpb.AdminMessage{
			PayloadVariant: &meshpb.AdminMessage_SetConfig{
				SetConfig: &meshpb.Config{
					PayloadVariant: &meshpb.Config_Network{
						Network: &meshpb.Config_NetworkConfig{WifiEnabled: false},
					},
				},
			},
		})
	}
	changes = append(changes, &meshpb.AdminMessage{
		PayloadVariant: &meshpb.AdminMessage_SetConfig{
			SetConfig: &meshpb.Config{
				PayloadVariant: &meshpb.Config_Bluetooth{
					Bluetooth: &meshpb.Config_BluetoothConfig{
						Enabled:  enabled,
						Mode:     pairingMode,
						FixedPin: fixedPin,
					},
				},
			},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := adapter.SendAdminBatch(ctx, dst, changes); err != nil {
		return fmt.Errorf("send admin batch: %w", err)
	}

	// PIN в логи не пишем — только факт его наличия.
	s.log.Info("bluetooth configured, device will reboot",
		"enabled", enabled,
		"mode", pairingMode.String(),
		"has_pin", fixedPin != 0,
		"wifi_auto_off", enabled, // при enable всегда шлём disable WiFi в batch
		"dst", fmt.Sprintf("%#x", dst))

	s.mu.RLock()
	transport := s.transport
	port := s.port
	s.mu.RUnlock()
	if port == "" {
		_ = s.closeAdapter("bluetooth config applied, device rebooting")
		return nil
	}
	s.scheduleReconnect(transport, port)
	return nil
}

// SetMeshDeviceConfig меняет LED-heartbeat + таймаут OLED-экрана + wake-on-tap.
// Все три поля живут в разных Config-подтипах (Device vs Display), но
// применяются одной batch-транзакцией = один reboot.
//
// Важно: в Config_DeviceConfig / Config_DisplayConfig много других полей
// (Role, broadcast intervals, GPS format и т.д.). Отправить partial-update
// нельзя — прошивка применит весь payload, стерев остальные настройки
// в default. Поэтому делаем read-modify-write через QueryConfig.
//
// Параметры:
//   - ledHeartbeat: true = LED мигает (LedHeartbeatDisabled=false);
//     false = LED не мигает.
//   - screenOnSecs: 0 = всегда включён; >0 = выключится через N сек после
//     последнего ввода/сообщения. Ограничение 0..3600.
//   - wakeOnTap: экран просыпается от тапа/встряхивания.
func (s *MeshtasticService) SetMeshDeviceConfig(
	ledHeartbeat bool,
	screenOnSecs uint32,
	wakeOnTap bool,
) error {
	if screenOnSecs > 3600 {
		return fmt.Errorf("screen_on_secs out of range (0..3600), got %d", screenOnSecs)
	}

	s.mu.RLock()
	adapter := s.adapter
	s.mu.RUnlock()
	if adapter == nil {
		return ErrMeshDisabled
	}
	if adapter.LocalNode() == 0 {
		select {
		case <-adapter.LocalNodeReady():
		case <-time.After(8 * time.Second):
			return errors.New("local node info not received yet — wait a few seconds after enable")
		}
	}
	dst := adapter.LocalNode()

	// Читаем текущие конфиги — мутируем только наши поля.
	devCtx, devCancel := context.WithTimeout(context.Background(), 5*time.Second)
	devCfg, derr := adapter.QueryConfig(devCtx, 5*time.Second, meshpb.AdminMessage_DEVICE_CONFIG)
	devCancel()
	if derr != nil {
		return fmt.Errorf("read device config: %w", derr)
	}
	deviceConfig := devCfg.GetDevice()
	if deviceConfig == nil {
		return errors.New("device config response empty")
	}
	dispCtx, dispCancel := context.WithTimeout(context.Background(), 5*time.Second)
	dispCfg, derr := adapter.QueryConfig(dispCtx, 5*time.Second, meshpb.AdminMessage_DISPLAY_CONFIG)
	dispCancel()
	if derr != nil {
		return fmt.Errorf("read display config: %w", derr)
	}
	displayConfig := dispCfg.GetDisplay()
	if displayConfig == nil {
		return errors.New("display config response empty")
	}

	// Mutate
	deviceConfig.LedHeartbeatDisabled = !ledHeartbeat
	displayConfig.ScreenOnSecs = screenOnSecs
	displayConfig.WakeOnTapOrMotion = wakeOnTap

	// Batch: begin + SetConfig.Device + SetConfig.Display + commit = 1 reboot.
	changes := []*meshpb.AdminMessage{
		{PayloadVariant: &meshpb.AdminMessage_SetConfig{
			SetConfig: &meshpb.Config{
				PayloadVariant: &meshpb.Config_Device{Device: deviceConfig},
			},
		}},
		{PayloadVariant: &meshpb.AdminMessage_SetConfig{
			SetConfig: &meshpb.Config{
				PayloadVariant: &meshpb.Config_Display{Display: displayConfig},
			},
		}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := adapter.SendAdminBatch(ctx, dst, changes); err != nil {
		return fmt.Errorf("send admin batch: %w", err)
	}

	s.log.Info("mesh device config, device will reboot",
		"led_heartbeat", ledHeartbeat,
		"screen_on_secs", screenOnSecs,
		"wake_on_tap", wakeOnTap,
		"dst", fmt.Sprintf("%#x", dst))

	s.mu.RLock()
	transport := s.transport
	port := s.port
	s.mu.RUnlock()
	if port == "" {
		_ = s.closeAdapter("device config applied, device rebooting")
		return nil
	}
	s.scheduleReconnect(transport, port)
	return nil
}

// AutoStart loads persisted config and, if Meshtastic was previously
// enabled, opens the saved transport/address. Called from main.go during app
// boot. Failures are logged but do not abort startup.
func (s *MeshtasticService) AutoStart() {
	enabled, transport, addr, portnum := s.loadConfig()
	if portnum > 0 {
		s.mu.Lock()
		s.portnum = portnum
		s.mu.Unlock()
	}
	if !enabled || addr == "" {
		return
	}
	if err := s.Enable(transport, addr); err != nil {
		s.log.Warn("autostart failed", "transport", transport, "addr", addr, "err", err)
	}
}

func (s *MeshtasticService) loadConfig() (enabled bool, transport string, addr string, portnum int) {
	if s.store == nil {
		return false, MeshTransportSerial, "", 0
	}
	db := s.store.DB()
	var v string
	if db.QueryRow("SELECT value FROM settings WHERE key = ?", settingMeshEnabled).Scan(&v) == nil {
		enabled = v == "1"
	}
	// Transport: default serial (обратная совместимость со старыми установками).
	transport = MeshTransportSerial
	var t string
	if db.QueryRow("SELECT value FROM settings WHERE key = ?", settingMeshTransport).Scan(&t) == nil {
		transport = normalizeMeshTransport(t)
	}
	_ = db.QueryRow("SELECT value FROM settings WHERE key = ?", settingMeshPort).Scan(&addr)
	var pn string
	if db.QueryRow("SELECT value FROM settings WHERE key = ?", settingMeshPortnum).Scan(&pn) == nil {
		if n, err := strconv.Atoi(pn); err == nil {
			portnum = n
		}
	}
	return enabled, transport, addr, portnum
}

func (s *MeshtasticService) saveSetting(key, value string) {
	if s.store == nil {
		return
	}
	if _, err := s.store.DB().Exec(
		"INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)", key, value,
	); err != nil {
		s.log.Warn("save setting", "key", key, "err", err)
	}
}

// SetPortNum updates the active portnum used for sending and receiving.
// Persisted but takes effect only on next Enable().
func (s *MeshtasticService) SetPortNum(n int) error {
	if n < 256 || n > 511 {
		return errors.New("portnum must be in 256..511 (private app range)")
	}
	s.mu.Lock()
	s.portnum = n
	s.mu.Unlock()
	s.saveSetting(settingMeshPortnum, strconv.Itoa(n))
	s.log.Info("portnum changed", "portnum", n, "note", "applies on next Enable")
	return nil
}

// PortNum returns the currently configured portnum.
func (s *MeshtasticService) PortNum() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.portnum
}

// MeshSavedWiFi — запись в журнале сохранённых WiFi-сетей устройства.
// PSK хранится открытым в БД, но сама БД шифруется через SQLCipher.
type MeshSavedWiFi struct {
	SSID string `json:"ssid"`
	PSK  string `json:"psk"`
}

// loadSavedMeshWiFi читает список сохранённых сетей из settings.
// Возвращает пустой слайс если ключа нет или JSON повреждён.
func (s *MeshtasticService) loadSavedMeshWiFi() []MeshSavedWiFi {
	if s.store == nil {
		return nil
	}
	var raw string
	err := s.store.DB().QueryRow(
		"SELECT value FROM settings WHERE key = ?", settingMeshWiFiNetworks,
	).Scan(&raw)
	if err != nil || raw == "" {
		return nil
	}
	var out []MeshSavedWiFi
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		s.log.Warn("mesh wifi networks unmarshal failed, resetting", "err", err)
		return nil
	}
	return out
}

func (s *MeshtasticService) persistSavedMeshWiFi(list []MeshSavedWiFi) error {
	b, err := json.Marshal(list)
	if err != nil {
		return fmt.Errorf("marshal wifi networks: %w", err)
	}
	s.saveSetting(settingMeshWiFiNetworks, string(b))
	return nil
}

// ListMeshWiFiNetworks возвращает сохранённые WiFi-сети устройства. Порядок —
// в котором они были сохранены (самая свежая — первая).
func (s *MeshtasticService) ListMeshWiFiNetworks() []MeshSavedWiFi {
	list := s.loadSavedMeshWiFi()
	if list == nil {
		return []MeshSavedWiFi{}
	}
	return list
}

// SaveMeshWiFiNetwork добавляет/обновляет запись. Дубликат по SSID вытесняется
// и запись всплывает в начало списка. При переполнении обрезается хвост.
func (s *MeshtasticService) SaveMeshWiFiNetwork(ssid, psk string) error {
	if ssid == "" {
		return errors.New("SSID is required")
	}
	if len(ssid) > 32 {
		return fmt.Errorf("SSID too long (%d chars, max 32)", len(ssid))
	}
	if len(psk) < 8 || len(psk) > 63 {
		return fmt.Errorf("PSK must be 8..63 chars (WPA2), got %d", len(psk))
	}
	list := s.loadSavedMeshWiFi()
	// Выкидываем существующий дубликат по SSID (case-sensitive как и в
	// самом протоколе 802.11).
	filtered := list[:0]
	for _, n := range list {
		if n.SSID != ssid {
			filtered = append(filtered, n)
		}
	}
	// Новая запись — в голову (MRU).
	next := append([]MeshSavedWiFi{{SSID: ssid, PSK: psk}}, filtered...)
	if len(next) > maxSavedMeshWiFi {
		next = next[:maxSavedMeshWiFi]
	}
	return s.persistSavedMeshWiFi(next)
}

// ForgetMeshWiFiNetwork удаляет запись по SSID. Неизвестные SSID игнорируются.
func (s *MeshtasticService) ForgetMeshWiFiNetwork(ssid string) error {
	list := s.loadSavedMeshWiFi()
	filtered := list[:0]
	for _, n := range list {
		if n.SSID != ssid {
			filtered = append(filtered, n)
		}
	}
	if len(filtered) == len(list) {
		return nil
	}
	return s.persistSavedMeshWiFi(filtered)
}

// SetEmitter wires the Wails event emitter. Called once during app boot.
func (s *MeshtasticService) SetEmitter(emit func(eventName string, data any)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emit = emit
}

// IsEnabled returns true if the radio is currently open.
func (s *MeshtasticService) IsEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.adapter != nil
}

// Status returns the current link state for the UI.
func (s *MeshtasticService) Status() MeshStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := MeshStatus{
		Enabled:          s.adapter != nil,
		Reconnecting:     s.reconnecting,
		ReconnectAttempt: s.reconnectAttempt,
		ReconnectMax:     s.reconnectMax,
		Transport:        s.transport,
		Port:             s.port,
		WiFi:             s.wifiStatus,
		Bluetooth:        s.btStatus,
		Device:           s.deviceStatus,
	}
	if s.tracker != nil {
		nodes := s.tracker.List()
		out.NumNodes = len(nodes)
		for i := range nodes {
			if nodes[i].IsLocal {
				out.NodeNum = nodes[i].NodeNum
				out.NodeID = nodes[i].NodeID
				out.HwModel = nodes[i].HwModel
				break
			}
		}
	}
	return out
}

// ListNodes returns the current snapshot of all known mesh nodes, sorted
// local-first then by recency. Safe for concurrent calls from the frontend.
func (s *MeshtasticService) ListNodes() []meshtastic.NodeSnapshot {
	s.mu.RLock()
	tracker := s.tracker
	s.mu.RUnlock()
	if tracker == nil {
		return []meshtastic.NodeSnapshot{}
	}
	return tracker.List()
}

// ListSerialPorts wraps the platform-specific port enumerator.
func (s *MeshtasticService) ListSerialPorts() ([]meshtastic.PortInfo, error) {
	return meshtastic.ListSerialPorts()
}

// Enable opens the device via the given transport and starts the receive loop.
// transport ∈ {"serial", "tcp"}:
//   - serial: addr = COM port name ("COM5" / "/dev/ttyUSB0").
//   - tcp:    addr = host or host:port ("192.168.1.42" / "192.168.1.42:4403").
//
// Returns error if the radio is already open.
func (s *MeshtasticService) Enable(transport, addr string) error {
	transport = normalizeMeshTransport(transport)
	// Защитно отменяем зависший reconnect, если он есть — пользователь мог
	// нажать Enable во время попыток автопереподключения.
	s.mu.Lock()
	if s.reconnectCancel != nil {
		s.reconnectCancel()
		s.reconnectCancel = nil
	}
	if s.adapter != nil {
		s.mu.Unlock()
		return errors.New("meshtastic: already enabled")
	}
	s.mu.Unlock()

	adapter, err := s.openAdapter(transport, addr)
	if err != nil {
		return err
	}

	// Проверяем что на том конце реально Meshtastic-устройство — ждём
	// MyInfo до 12 сек (serial: WakeDelay 4с + ответ прошивки ~0.1с; tcp:
	// ~0.1-0.3с). Если нет ответа — неправильный адрес / не-Meshtastic
	// сервер на 4403 / устройство зависло. Rollback с понятной ошибкой
	// вместо "тихо включено, но следующая команда падает через 10 сек".
	select {
	case <-adapter.LocalNodeReady():
	case <-time.After(12 * time.Second):
		_ = adapter.Close()
		return fmt.Errorf("устройство на %s не ответило (MyInfo). Проверьте адрес и что это Meshtastic-прошивка", addr)
	}

	s.applyAdapter(adapter, transport, addr)

	s.saveSetting(settingMeshEnabled, "1")
	s.saveSetting(settingMeshTransport, transport)
	s.saveSetting(settingMeshPort, addr)

	s.fireStatus()
	s.log.Info("enabled", "transport", transport, "addr", addr)

	// После успешного enable спрашиваем у устройства текущий WiFi-статус
	// и показываем его в UI (SSID/IP/RSSI). Запускается фоново, чтобы не
	// задерживать возврат Enable.
	go s.queryDeviceConnectionStatus(adapter)
	return nil
}

// normalizeMeshTransport нормализует строку транспорта — пустое значение и
// любое не-tcp трактуем как serial для обратной совместимости.
func normalizeMeshTransport(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	if t == MeshTransportTCP {
		return MeshTransportTCP
	}
	return MeshTransportSerial
}

// openAdapter создаёт Adapter на указанном транспорте/адресе с текущими
// настройками сервиса. Вынесен, потому что reconnectLoop делает то же самое
// без валидации "already enabled" и без сохранения persisted-настроек.
func (s *MeshtasticService) openAdapter(transport, addr string) (*meshtastic.Adapter, error) {
	s.mu.RLock()
	portnum := s.portnum
	s.mu.RUnlock()
	tracker := meshtastic.NewNodeTracker(s.onNodeChange)
	switch normalizeMeshTransport(transport) {
	case MeshTransportTCP:
		host, port, err := meshtastic.ParseTCPAddress(addr)
		if err != nil {
			return nil, fmt.Errorf("parse tcp addr %q: %w", addr, err)
		}
		a, err := meshtastic.NewTCPAdapter(meshtastic.TCPConfig{
			Host:    host,
			Port:    port,
			PortNum: meshpb.PortNum(portnum),
			Logger:  s.log,
			Tracker: tracker,
		})
		if err != nil {
			return nil, fmt.Errorf("tcp %s: %w", addr, err)
		}
		return a, nil
	default: // serial
		a, err := meshtastic.NewSerialAdapter(meshtastic.SerialConfig{
			Port:    addr,
			PortNum: meshpb.PortNum(portnum),
			Logger:  s.log,
			Tracker: tracker,
		})
		if err != nil {
			return nil, fmt.Errorf("serial %s: %w", addr, err)
		}
		return a, nil
	}
}

// applyAdapter устанавливает новый адаптер как активный + запускает все
// фоновые лупы (recv/recvText/announcement). Вызывается из Enable и
// reconnectLoop после успешного открытия порта.
func (s *MeshtasticService) applyAdapter(adapter *meshtastic.Adapter, transport, addr string) {
	ctx, cancel := context.WithCancel(context.Background())

	s.mu.Lock()
	s.adapter = adapter
	s.tracker = adapter.Tracker()
	s.reassembler = meshtastic.NewReassembler(func(src, msgID uint32, received, total int) {
		s.log.Warn("encrypted DM reassembly timed out",
			"src", fmt.Sprintf("%#x", src),
			"msgID", fmt.Sprintf("%#x", msgID),
			"received", received,
			"total", total,
			"missing", total-received)
	})
	s.transport = normalizeMeshTransport(transport)
	s.port = addr
	s.cancel = cancel
	s.mu.Unlock()

	go s.recvLoop(ctx, adapter)
	go s.recvTextLoop(ctx, adapter)
	go s.announcementLoop(ctx, adapter)
}

// Disable closes the radio AND persists the user preference (enabled=0).
// Called from the settings UI when the user explicitly toggles mesh off.
// Idempotent.
func (s *MeshtasticService) Disable() error {
	s.saveSetting(settingMeshEnabled, "0")
	return s.closeAdapter("disabled")
}

// Shutdown closes the radio WITHOUT persisting any preference change.
// Called from main.go's `defer` on app exit so a clean shutdown doesn't
// erase the "user wants mesh enabled on next launch" flag.
func (s *MeshtasticService) Shutdown() error {
	return s.closeAdapter("shutdown")
}

func (s *MeshtasticService) closeAdapter(reason string) error {
	s.mu.Lock()
	adapter := s.adapter
	cancel := s.cancel
	reconnectCancel := s.reconnectCancel
	s.adapter = nil
	s.tracker = nil
	s.reassembler = nil
	s.transport = ""
	s.port = ""
	s.cancel = nil
	s.wifiStatus = nil
	s.btStatus = nil
	s.deviceStatus = nil
	s.reconnecting = false
	s.reconnectAttempt = 0
	s.reconnectMax = 0
	s.reconnectCancel = nil
	s.mu.Unlock()

	// Параллельный reconnect, если был, отменяем первым — иначе он может
	// успешно открыть порт уже после closeAdapter и «воскресить» адаптер.
	if reconnectCancel != nil {
		reconnectCancel()
	}
	if adapter == nil {
		// Всё равно эмитим status, вдруг был reconnecting, а теперь его нет.
		s.fireStatus()
		return nil
	}
	if cancel != nil {
		cancel()
	}
	if err := adapter.Close(); err != nil {
		s.log.Warn("close adapter", "err", err)
	}
	s.fireStatus()
	s.log.Info(reason)
	return nil
}

// scheduleReconnect закрывает текущий адаптер (но сохраняет port в
// persisted-настройках и в UI), помечает состояние "reconnecting" и запускает
// фоновый reconnectLoop. Вызывается из SetWiFi после отправки AdminMessage,
// когда мы знаем, что устройство сейчас ребутнется.
//
// Вся важная часть — порт остаётся логически "наш": пользователь не должен
// вручную включать mesh заново, UI показывает прогресс переподключения.
func (s *MeshtasticService) scheduleReconnect(transport, addr string) {
	s.mu.Lock()
	// Отменяем предыдущий reconnect, если он висит (SetWiFi дважды подряд).
	if s.reconnectCancel != nil {
		s.reconnectCancel()
		s.reconnectCancel = nil
	}
	adapter := s.adapter
	cancel := s.cancel
	s.adapter = nil
	s.tracker = nil
	s.reassembler = nil
	s.cancel = nil
	s.wifiStatus = nil
	s.btStatus = nil
	s.deviceStatus = nil
	s.transport = normalizeMeshTransport(transport)
	s.port = addr
	s.reconnecting = true
	s.reconnectAttempt = 0
	s.reconnectMax = 12
	reconnectCtx, reconnectCancel := context.WithCancel(context.Background())
	s.reconnectCancel = reconnectCancel
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if adapter != nil {
		if err := adapter.Close(); err != nil {
			s.log.Warn("close adapter before reconnect", "err", err)
		}
	}
	s.fireStatus()
	go s.reconnectLoop(reconnectCtx, transport, addr)
}

// reconnectLoop пытается переоткрыть адаптер после programmatic-reboot
// устройства. Интервалы: 4 сек warmup, потом 12 попыток × 2.5 сек = итого до
// ~34 сек. Отменяется через ctx (Disable/Shutdown/повторный Enable).
func (s *MeshtasticService) reconnectLoop(ctx context.Context, transport, addr string) {
	// Warmup — ESP32 после set_config делает hw-reboot, CP210x может
	// пропасть из системы на несколько секунд. Не пытаемся открывать раньше.
	select {
	case <-time.After(4 * time.Second):
	case <-ctx.Done():
		return
	}

	for i := 0; i < 12; i++ {
		s.mu.Lock()
		s.reconnectAttempt = i + 1
		s.mu.Unlock()
		s.fireStatus()

		adapter, err := s.openAdapter(transport, addr)
		if err == nil {
			s.applyAdapter(adapter, transport, addr)
			s.mu.Lock()
			s.reconnecting = false
			s.reconnectAttempt = 0
			s.reconnectMax = 0
			s.reconnectCancel = nil
			s.mu.Unlock()
			s.fireStatus()
			s.log.Info("reconnected after reboot", "transport", transport, "addr", addr, "attempts", i+1)
			// Запрашиваем WiFi-статус заново: SSID/IP после применённой
			// конфигурации.
			go s.queryDeviceConnectionStatus(adapter)
			return
		}
		s.log.Debug("reconnect attempt failed", "attempt", i+1, "err", err)

		select {
		case <-ctx.Done():
			return
		case <-time.After(2500 * time.Millisecond):
		}
	}

	// Все попытки провалились — окончательно сдаёмся, UI покажет "не удалось".
	s.mu.Lock()
	s.reconnecting = false
	s.reconnectAttempt = 0
	s.reconnectMax = 0
	s.reconnectCancel = nil
	s.port = ""
	s.mu.Unlock()
	s.fireStatus()
	s.log.Warn("reconnect after reboot failed, giving up", "transport", transport, "addr", addr)
}

// queryDeviceConnectionStatus спрашивает у устройства WiFi-статус через
// AdminMessage и сохраняет результат в s.wifiStatus. Вызывается после
// Enable и после успешного reconnect. Ошибки не фэйлятся наверх — просто
// логируются (WiFi может быть не настроен, не подключился, device занят).
func (s *MeshtasticService) queryDeviceConnectionStatus(adapter *meshtastic.Adapter) {
	// Ждём сигнал что MyInfo пришёл (получен own nodeNum) — без него
	// admin-запрос идёт с dst=0 и прошивка его игнорирует.
	select {
	case <-adapter.LocalNodeReady():
	case <-time.After(20 * time.Second):
		s.log.Info("wifi status query skipped: MyInfo not received in 20s")
		return
	}

	// Читаем применённые конфиги через GetConfigRequest — source of truth
	// для Enabled/Mode/SSID. Это позволяет фронту заранее видеть реальное
	// состояние (чекбоксы, SSID инпут) до того как прийдёт WiFi-связность.
	btCfgCtx, btCfgCancel := context.WithTimeout(context.Background(), 5*time.Second)
	btCfg, err := adapter.QueryConfig(btCfgCtx, 5*time.Second, meshpb.AdminMessage_BLUETOOTH_CONFIG)
	btCfgCancel()
	var btFromConfig MeshBtStatus
	if err != nil {
		s.log.Info("bt config query failed", "err", err)
	} else if bc := btCfg.GetBluetooth(); bc != nil {
		btFromConfig.Enabled = bc.GetEnabled()
		btFromConfig.Mode = bc.GetMode().String()
		s.log.Info("bt config",
			"enabled", btFromConfig.Enabled,
			"mode", btFromConfig.Mode)
	}

	netCfgCtx, netCfgCancel := context.WithTimeout(context.Background(), 5*time.Second)
	netCfg, netErr := adapter.QueryConfig(netCfgCtx, 5*time.Second, meshpb.AdminMessage_NETWORK_CONFIG)
	netCfgCancel()
	var wifiFromConfig struct {
		Enabled bool
		SSID    string
	}
	if netErr != nil {
		s.log.Info("network config query failed", "err", netErr)
	} else if nc := netCfg.GetNetwork(); nc != nil {
		wifiFromConfig.Enabled = nc.GetWifiEnabled()
		wifiFromConfig.SSID = nc.GetWifiSsid()
		s.log.Info("network config",
			"wifi_enabled", wifiFromConfig.Enabled,
			"ssid", wifiFromConfig.SSID)
	}

	// Device + Display для LED/screen настроек. Если один из запросов упадёт —
	// сохраняем частичную инфу в s.deviceStatus (не блокируем WiFi/BT-потоки).
	devStatus := &MeshDeviceStatus{}
	haveDev := false
	devCtx, devCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if devCfg, derr := adapter.QueryConfig(devCtx, 5*time.Second, meshpb.AdminMessage_DEVICE_CONFIG); derr != nil {
		s.log.Info("device config query failed", "err", derr)
	} else if dc := devCfg.GetDevice(); dc != nil {
		devStatus.LedHeartbeat = !dc.GetLedHeartbeatDisabled()
		haveDev = true
		s.log.Info("device config", "led_heartbeat", devStatus.LedHeartbeat)
	}
	devCancel()

	dispCtx, dispCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if dispCfg, derr := adapter.QueryConfig(dispCtx, 5*time.Second, meshpb.AdminMessage_DISPLAY_CONFIG); derr != nil {
		s.log.Info("display config query failed", "err", derr)
	} else if dp := dispCfg.GetDisplay(); dp != nil {
		devStatus.ScreenOnSecs = dp.GetScreenOnSecs()
		devStatus.WakeOnTap = dp.GetWakeOnTapOrMotion()
		haveDev = true
		s.log.Info("display config",
			"screen_on_secs", devStatus.ScreenOnSecs,
			"wake_on_tap", devStatus.WakeOnTap)
	}
	dispCancel()

	if haveDev {
		s.mu.Lock()
		if s.adapter == adapter {
			s.deviceStatus = devStatus
		}
		s.mu.Unlock()
		s.fireStatus()
	}

	// Если WiFi в конфиге выключен — нет смысла в retry loop'е: даже при
	// сохранённом SSID устройство не пытается подключиться, IP никогда не
	// появится. Один опрос connection_status (для актуализации BT-части) и
	// выход.
	maxAttempts := 10
	if !wifiFromConfig.Enabled {
		maxAttempts = 1
	}
	const retryDelay = 3 * time.Second

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		s.mu.RLock()
		current := s.adapter
		s.mu.RUnlock()
		if current != adapter {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		resp, err := adapter.QueryDeviceConnectionStatus(ctx, 5*time.Second)
		cancel()
		if err != nil {
			s.log.Info("wifi status query failed", "attempt", attempt, "err", err)
			return
		}

		w := &MeshWiFiStatus{Enabled: wifiFromConfig.Enabled}
		if wifi := resp.GetWifi(); wifi != nil {
			w.SSID = wifi.GetSsid()
			w.Rssi = wifi.GetRssi()
			if st := wifi.GetStatus(); st != nil {
				w.Connected = st.GetIsConnected()
				w.IP = meshtastic.IPv4FromUint32(st.GetIpAddress())
				s.log.Info("wifi status",
					"attempt", attempt,
					"ssid", w.SSID,
					"ip", w.IP,
					"ip_raw", fmt.Sprintf("%#x", st.GetIpAddress()),
					"rssi", w.Rssi,
					"connected", w.Connected)
			}
		}
		// Если connection_status не вернул SSID (WiFi ещё не ассоциирован) —
		// подставляем из config, чтобы UI сразу показал "подключаюсь к X".
		if w.SSID == "" {
			w.SSID = wifiFromConfig.SSID
		}

		// BT: Enabled/Mode берём из прочитанного btFromConfig (source of truth),
		// Connected — runtime из BluetoothConnectionStatus. На Heltec V3
		// Meshtastic-прошивка часто присылает структуру даже когда BT стек
		// отключён (WiFi блокирует BT radio), поэтому полагаться на её
		// наличие нельзя.
		bt := &MeshBtStatus{
			Enabled: btFromConfig.Enabled,
			Mode:    btFromConfig.Mode,
		}
		if btResp := resp.GetBluetooth(); btResp != nil {
			bt.Connected = btResp.GetIsConnected()
		}

		s.mu.Lock()
		if s.adapter == adapter {
			s.wifiStatus = w
			s.btStatus = bt
		}
		s.mu.Unlock()
		s.fireStatus()

		// Терминал: либо подключились (connected=true), либо WiFi выключен
		// (пустой SSID).
		if w.Connected || w.SSID == "" {
			return
		}
		// Промежуточное "подключается" — ещё попытка через retryDelay.
		time.Sleep(retryDelay)
	}
	s.log.Info("wifi status: connect timed out after all attempts — device may still be trying")
}

// TestConnection opens the port briefly, waits up to 8s for the radio to
// announce its MyNodeInfo, then closes. Used by the settings UI's "Test"
// button. Does NOT touch the active adapter — runs in isolation.
func (s *MeshtasticService) TestConnection(transport, addr string) MeshTestResult {
	transport = normalizeMeshTransport(transport)
	s.log.Info("test connection start", "transport", transport, "addr", addr)
	s.mu.RLock()
	active := s.port == addr && s.transport == transport && s.adapter != nil
	s.mu.RUnlock()
	if active {
		s.log.Warn("test connection: already in use", "addr", addr)
		return MeshTestResult{
			OK:    false,
			Error: "already in use by enabled adapter — disable first",
		}
	}

	tracker := meshtastic.NewNodeTracker(nil)
	var (
		adapter *meshtastic.Adapter
		err     error
	)
	switch transport {
	case MeshTransportTCP:
		host, port, perr := meshtastic.ParseTCPAddress(addr)
		if perr != nil {
			return MeshTestResult{OK: false, Error: perr.Error()}
		}
		adapter, err = meshtastic.NewTCPAdapter(meshtastic.TCPConfig{
			Host:    host,
			Port:    port,
			Logger:  s.log.With("op", "test"),
			Tracker: tracker,
		})
	default:
		adapter, err = meshtastic.NewSerialAdapter(meshtastic.SerialConfig{
			Port:    addr,
			Logger:  s.log.With("op", "test"),
			Tracker: tracker,
		})
	}
	if err != nil {
		s.log.Warn("test connection: open failed", "transport", transport, "addr", addr, "err", err)
		return MeshTestResult{OK: false, Error: err.Error()}
	}
	defer adapter.Close()

	deadline := time.After(8 * time.Second)
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			s.log.Warn("test connection: timeout (8s)", "addr", addr)
			return MeshTestResult{
				OK:    false,
				Error: "no MyNodeInfo within 8s — wrong address, not Meshtastic firmware, or board still booting",
			}
		case <-tick.C:
			if num := adapter.LocalNode(); num != 0 {
				snap, _ := tracker.Get(num)
				s.log.Info("test connection: ok",
					"addr", addr,
					"nodenum", fmt.Sprintf("%#x", num),
					"hwModel", snap.HwModel)
				return MeshTestResult{
					OK:      true,
					NodeNum: num,
					NodeID:  snap.NodeID,
					HwModel: snap.HwModel,
				}
			}
		}
	}
}

// recvLoop drains the adapter's Recv channel and tries to decrypt every
// payload as a Vox-encrypted DM. Successful decrypts are handed to the
// onDecrypted callback (typically MessagingService.handleIncomingP2P), so
// mesh-DMs end up in the same chat history as P2P-DMs with no special-
// casing in the chat layer. Anything we can't attribute or decrypt is
// silently dropped — likely belongs to another Vox user on the channel.
func (s *MeshtasticService) recvLoop(ctx context.Context, adapter *meshtastic.Adapter) {
	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-adapter.Recv():
			if !ok {
				return
			}
			s.log.Debug("recv envelope",
				"src", fmt.Sprintf("%#x", env.Src),
				"bytes", len(env.Payload),
				"rxSnr", env.RxSnr,
				"rxRssi", env.RxRssi,
			)
			// Discovery announcements take precedence — they have a fixed
			// type byte (0x02) so we can route them without ambiguity.
			if s.tryParseAnnouncement(env) {
				continue
			}
			s.tryDecryptIncoming(env)
		}
	}
}

// announcementInterval — how often to broadcast our own discovery
// announcement when auto-discovery is enabled. Picked to balance
// reachability for newly-online friends against channel airtime.
const announcementInterval = 15 * time.Minute

// announcementLoop periodically broadcasts our identity → nodeNum mapping
// so friends with us in their contact list can auto-fill the mapping. Runs
// while the radio is open; cancelled by ctx on Disable/Shutdown.
func (s *MeshtasticService) announcementLoop(ctx context.Context, adapter *meshtastic.Adapter) {
	// First broadcast happens on a short fuse so a freshly-enabled mesh
	// doesn't wait the full interval for its first announcement to land.
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.broadcastAnnouncement(adapter)
			timer.Reset(announcementInterval)
		}
	}
}

func (s *MeshtasticService) broadcastAnnouncement(adapter *meshtastic.Adapter) {
	priv, err := s.sess.PrivateKey()
	if err != nil || len(priv) == 0 {
		// Identity not loaded — happens during recovery flow or pre-setup.
		// Logged at debug because it's an expected transient state, not a bug.
		s.log.Debug("announcement skipped: no identity", "err", err)
		return
	}
	nodeNum := adapter.LocalNode()
	if nodeNum == 0 {
		// Firmware hasn't sent MyNodeInfo yet — first 1-3s after Enable.
		// Next tick (15min later) will retry.
		s.log.Debug("announcement skipped: localNode unknown")
		return
	}
	blob, err := meshtastic.EncodeAnnouncement(priv, nodeNum, time.Now())
	if err != nil {
		s.log.Warn("encode announcement", "err", err)
		return
	}
	if err := adapter.Send(context.Background(), meshtastic.BroadcastDst, blob); err != nil {
		s.log.Warn("broadcast announcement", "err", err)
		return
	}
	s.log.Info("discovery announcement sent",
		"nodenum", fmt.Sprintf("%#x", nodeNum),
		"bytes", len(blob))
}

// tryParseAnnouncement attempts to interpret an envelope as a discovery
// announcement, validates its signature + timestamp, and if the announcing
// pubkey is one of our friends, persists the auto-discovered nodeNum
// mapping. Returns true if the bytes were a valid announcement (consumed),
// so the caller doesn't also feed them to the encrypted-DM path.
func (s *MeshtasticService) tryParseAnnouncement(env meshtastic.Envelope) bool {
	if len(env.Payload) == 0 || env.Payload[0] != meshtastic.AnnouncementType {
		return false
	}
	ann, err := meshtastic.DecodeAnnouncement(env.Payload, time.Now())
	if err != nil {
		s.log.Debug("announcement decode", "src", fmt.Sprintf("%#x", env.Src), "err", err)
		return true // type byte matched, just invalid — still our packet kind
	}
	peerID := hex.EncodeToString(ann.Pubkey)

	// Only act on announcements from our friends — strangers are ignored
	// to avoid contact-spoofing via the public channel.
	if s.store == nil {
		return true
	}
	var status string
	if err := s.store.DB().QueryRow(
		`SELECT status FROM contacts WHERE peer_id = ?`, peerID,
	).Scan(&status); err != nil || status != "accepted" {
		return true
	}

	// Save mapping only if it differs — avoids needless writes and a
	// peer-mesh-mapping log spam every 15 min.
	current, _ := s.GetPeerMeshNode(peerID)
	if current.NodeNum == ann.NodeNum {
		return true
	}
	if err := s.SetPeerMeshNode(peerID, ann.NodeNum); err != nil {
		s.log.Warn("auto-save discovered mapping", "peer", safeShort(peerID), "err", err)
		return true
	}
	s.log.Info("discovery: friend mesh node auto-mapped",
		"peer", safeShort(peerID),
		"nodenum", fmt.Sprintf("%#x", ann.NodeNum))
	return true
}

// tryDecryptIncoming runs the receive pipeline for one envelope:
//  1. Feed into the reassembler — single-fragment messages complete on the
//     first call; multi-fragment ones return (nil, false, nil) until the
//     last chunk arrives.
//  2. On a complete reassembled blob, look up the source nodeNum in our
//     friend mapping. No mapping → drop (not for us, or stranger on the
//     channel).
//  3. Decrypt with friend's pubkey + our private key. Failure means either
//     the bytes weren't a Vox blob or the wrong key — silent drop, log at
//     debug only.
//  4. Hand plaintext to onDecrypted which runs the same code path as P2P
//     receipts (handleIncomingP2P).
func (s *MeshtasticService) tryDecryptIncoming(env meshtastic.Envelope) {
	s.mu.RLock()
	cb := s.onDecrypted
	reassembler := s.reassembler
	s.mu.RUnlock()
	if cb == nil || reassembler == nil {
		return
	}

	blob, complete, err := reassembler.Feed(env.Src, env.Payload)
	if err != nil {
		// Wrong type byte (foreign packet) or malformed — drop quietly.
		s.log.Debug("reassemble feed", "src", fmt.Sprintf("%#x", env.Src), "err", err)
		return
	}
	if !complete {
		return // waiting for more fragments
	}

	peerID := s.findPeerByMeshNode(env.Src)
	if peerID == "" {
		return
	}
	peerPub, err := hex.DecodeString(peerID)
	if err != nil || len(peerPub) != ed25519.PublicKeySize {
		s.log.Warn("mapped peer has invalid pubkey", "peer", safeShort(peerID))
		return
	}
	mySec, err := s.sess.PrivateKey()
	if err != nil || len(mySec) == 0 {
		return
	}
	plaintext, err := meshtastic.DecryptFromPeer(ed25519.PublicKey(peerPub), mySec, blob)
	if err != nil {
		s.log.Debug("decrypt failed", "src", fmt.Sprintf("%#x", env.Src), "err", err)
		return
	}
	s.log.Info("encrypted DM received",
		"peer", safeShort(peerID),
		"src", fmt.Sprintf("%#x", env.Src),
		"plaintext_bytes", len(plaintext),
		"rxSnr", env.RxSnr,
		"rxRssi", env.RxRssi)
	cb(peerID, plaintext)
}

// MeshTextRecord is a row in the mesh_texts table, JSON-friendly for the UI.
type MeshTextRecord struct {
	ID         int64   `json:"id"`
	MsgID      uint32  `json:"msgId"`
	SrcNodeNum uint32  `json:"srcNodeNum"`
	SrcNodeID  string  `json:"srcNodeId"`
	SrcName    string  `json:"srcName"`
	DstNodeNum uint32  `json:"dstNodeNum"`           // 0xFFFFFFFF = broadcast
	Direction  string  `json:"direction"`            // "incoming" | "outgoing"
	Text       string  `json:"text"`
	RxSnr      float32 `json:"rxSnr"`
	RxRssi     int32   `json:"rxRssi"`
	CreatedAt  int64   `json:"createdAt"`            // unix epoch
	Read       bool    `json:"read"`
}

// BroadcastNodeNum is the wire constant for "all nodes". Stored verbatim in
// dst_node_num for outgoing broadcasts so per-node filtering can distinguish
// "to all" from "to no specific recipient".
const BroadcastNodeNum uint32 = 0xFFFFFFFF

// SendBroadcastText publishes a plaintext message on TEXT_MESSAGE_APP=1 to
// the broadcast destination (all nodes on channel).
func (s *MeshtasticService) SendBroadcastText(text string) error {
	return s.sendText(0, text) // 0 -> BroadcastDst
}

// SendDirectText publishes a plaintext message on TEXT_MESSAGE_APP=1 to a
// specific node by its nodeNum. Plaintext is still readable by anyone on
// the channel — the unicast just helps Meshtastic routing prioritize the
// intended recipient. Use for M6.2+ encrypted variants for actual privacy.
func (s *MeshtasticService) SendDirectText(dstNodeNum uint32, text string) error {
	if dstNodeNum == 0 {
		return errors.New("dstNodeNum is required for direct send (use SendBroadcastText for all)")
	}
	return s.sendText(dstNodeNum, text)
}

func (s *MeshtasticService) sendText(dst uint32, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("text is empty")
	}
	if len(text) > 200 {
		return fmt.Errorf("text too long (%d bytes, max 200 without fragmentation)", len(text))
	}

	s.mu.RLock()
	adapter := s.adapter
	tracker := s.tracker
	s.mu.RUnlock()
	if adapter == nil {
		return ErrMeshDisabled
	}

	target := dst
	if target == 0 {
		target = meshtastic.BroadcastDst
	}
	if err := adapter.SendText(context.Background(), target, text); err != nil {
		return err
	}

	// Persist as outgoing so it shows up in history immediately.
	srcNum := adapter.LocalNode()
	srcID := ""
	srcName := ""
	if tracker != nil {
		if snap, ok := tracker.Get(srcNum); ok {
			srcID = snap.NodeID
			srcName = snap.LongName
		}
	}
	rec := MeshTextRecord{
		MsgID:      0, // firmware assigns; we don't capture it
		SrcNodeNum: srcNum,
		SrcNodeID:  srcID,
		SrcName:    srcName,
		DstNodeNum: target,
		Direction:  "outgoing",
		Text:       text,
		CreatedAt:  time.Now().Unix(),
		Read:       true,
	}
	if id, err := s.insertText(rec); err == nil {
		rec.ID = id
		s.fireText(rec)
	} else {
		s.log.Warn("save outgoing text", "err", err)
	}
	return nil
}

// ListMeshTexts returns the most recent N text records, newest first.
// Pass limit=0 for the default (200). Used by the "Общий чат" view.
func (s *MeshtasticService) ListMeshTexts(limit int) ([]MeshTextRecord, error) {
	return s.queryMeshTexts(limit, "", nil)
}

// ListMeshTextsBroadcast returns only broadcast traffic (incoming addressed to
// 0xFFFFFFFF + outgoing we sent to broadcast). For the pinned "Эфир →
// общий" view in the new mesh nav.
func (s *MeshtasticService) ListMeshTextsBroadcast(limit int) ([]MeshTextRecord, error) {
	return s.queryMeshTexts(limit,
		`AND (
		    (direction = 'outgoing' AND dst_node_num IN (?, 0))
		 OR (direction = 'incoming' AND dst_node_num = ?)
		)`,
		[]any{BroadcastNodeNum, BroadcastNodeNum})
}

// ListMeshTextsForNode returns the conversation with one specific Meshtastic
// node: outgoing where we addressed dstNodeNum, incoming where they sent to us.
func (s *MeshtasticService) ListMeshTextsForNode(nodeNum uint32, limit int) ([]MeshTextRecord, error) {
	if nodeNum == 0 || nodeNum == BroadcastNodeNum {
		return s.ListMeshTextsBroadcast(limit)
	}
	return s.queryMeshTexts(limit,
		`AND (
		    (direction = 'outgoing' AND dst_node_num = ?)
		 OR (direction = 'incoming' AND src_node_num = ? AND dst_node_num != ?)
		)`,
		[]any{nodeNum, nodeNum, BroadcastNodeNum})
}

func (s *MeshtasticService) queryMeshTexts(limit int, whereExtra string, args []any) ([]MeshTextRecord, error) {
	if s.store == nil {
		return []MeshTextRecord{}, nil
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	q := `SELECT id, msg_id, src_node_num, src_node_id, src_long_name,
	             dst_node_num, direction, text, rx_snr, rx_rssi, created_at, read
	        FROM mesh_texts
	       WHERE 1=1 ` + whereExtra + `
	    ORDER BY created_at DESC, id DESC
	       LIMIT ?`
	args = append(args, limit)
	rows, err := s.store.DB().Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]MeshTextRecord, 0, limit)
	for rows.Next() {
		var r MeshTextRecord
		var readInt int
		if err := rows.Scan(&r.ID, &r.MsgID, &r.SrcNodeNum, &r.SrcNodeID, &r.SrcName,
			&r.DstNodeNum, &r.Direction, &r.Text, &r.RxSnr, &r.RxRssi,
			&r.CreatedAt, &readInt); err != nil {
			s.log.Warn("scan mesh_text", "err", err)
			continue
		}
		r.Read = readInt != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetMeshUnreadCount returns the number of unread incoming text messages.
func (s *MeshtasticService) GetMeshUnreadCount() (int, error) {
	if s.store == nil {
		return 0, nil
	}
	var n int
	err := s.store.DB().QueryRow(
		`SELECT COUNT(*) FROM mesh_texts WHERE direction = 'incoming' AND read = 0`,
	).Scan(&n)
	return n, err
}

// MarkMeshTextsRead clears the unread flag on all incoming texts.
func (s *MeshtasticService) MarkMeshTextsRead() error {
	if s.store == nil {
		return nil
	}
	_, err := s.store.DB().Exec(
		`UPDATE mesh_texts SET read = 1 WHERE direction = 'incoming' AND read = 0`,
	)
	return err
}

// ClearMeshTexts wipes all stored mesh text history. Destructive; logged
// at INFO so we have an audit trail in remote diagnostics.
func (s *MeshtasticService) ClearMeshTexts() error {
	if s.store == nil {
		return nil
	}
	res, err := s.store.DB().Exec(`DELETE FROM mesh_texts`)
	if err != nil {
		s.log.Warn("clear mesh texts", "err", err)
		return err
	}
	n, _ := res.RowsAffected()
	s.log.Info("mesh texts cleared", "rows", n)
	return nil
}

// recvTextLoop persists every incoming TEXT_MESSAGE_APP message, dedup by
// (msg_id, src_node_num), and emits meshtastic:text-received.
func (s *MeshtasticService) recvTextLoop(ctx context.Context, adapter *meshtastic.Adapter) {
	for {
		select {
		case <-ctx.Done():
			return
		case te, ok := <-adapter.RecvText():
			if !ok {
				return
			}
			rec := MeshTextRecord{
				MsgID:      te.MsgID,
				SrcNodeNum: te.Src,
				SrcNodeID:  meshtastic.FormatNodeID(te.Src),
				DstNodeNum: te.Dst, // 0xFFFFFFFF for broadcast, our localNode for unicast
				Direction:  "incoming",
				Text:       te.Text,
				RxSnr:      te.RxSnr,
				RxRssi:     te.RxRssi,
				CreatedAt:  time.Now().Unix(),
				Read:       false,
			}
			s.mu.RLock()
			tracker := s.tracker
			s.mu.RUnlock()
			if tracker != nil {
				if snap, ok := tracker.Get(te.Src); ok {
					rec.SrcName = snap.LongName
				}
			}
			id, err := s.insertText(rec)
			if err != nil {
				// Unique-index dup means we've already seen this msg via
				// rebroadcast — silently skip, don't log.
				if !isUniqueViolation(err) {
					s.log.Warn("save incoming text", "err", err, "src", te.Src)
				}
				continue
			}
			rec.ID = id
			s.log.Info("text received",
				"src", fmt.Sprintf("%#x", te.Src),
				"bytes", len(te.Text),
				"rxSnr", te.RxSnr,
				"rxRssi", te.RxRssi,
			)
			s.fireText(rec)
		}
	}
}

func (s *MeshtasticService) insertText(r MeshTextRecord) (int64, error) {
	if s.store == nil {
		return 0, errors.New("storage unavailable")
	}
	readInt := 0
	if r.Read {
		readInt = 1
	}
	res, err := s.store.DB().Exec(
		`INSERT INTO mesh_texts
		   (msg_id, src_node_num, src_node_id, src_long_name, dst_node_num,
		    direction, text, rx_snr, rx_rssi, created_at, read)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.MsgID, r.SrcNodeNum, r.SrcNodeID, r.SrcName, r.DstNodeNum,
		r.Direction, r.Text, r.RxSnr, r.RxRssi, r.CreatedAt, readInt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *MeshtasticService) fireText(r MeshTextRecord) {
	s.mu.RLock()
	emit := s.emit
	s.mu.RUnlock()
	if emit != nil {
		emit("meshtastic:text-received", r)
	}
}

// ErrMeshDisabled is returned when an operation requires the radio but it is
// not currently enabled.
var ErrMeshDisabled = errors.New("meshtastic: not enabled")

// isUniqueViolation checks for SQLite UNIQUE constraint violations from
// the mesh_texts dedup index. Brittle string match — sqlcipher driver
// doesn't expose typed errors but the message is stable.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE")
}

func (s *MeshtasticService) onNodeChange(snap meshtastic.NodeSnapshot) {
	s.mu.RLock()
	emit := s.emit
	s.mu.RUnlock()
	if emit != nil {
		emit("meshtastic:node-update", snap)
	}
}

func (s *MeshtasticService) fireStatus() {
	s.mu.RLock()
	emit := s.emit
	s.mu.RUnlock()
	if emit != nil {
		emit("meshtastic:status-change", s.Status())
	}
}

// PortNum exposes the meshpb constants the frontend may need (default
// PRIVATE_APP). Returned as a plain int for JSON friendliness.
func (s *MeshtasticService) DefaultPortNum() int {
	return int(meshpb.PortNum_PRIVATE_APP)
}

// PeerMeshMapping is the friend ↔ Meshtastic-node link for a single contact.
// Returned by the API as a JSON-friendly snapshot.
type PeerMeshMapping struct {
	PeerID  string `json:"peerId"`            // Vox public key, hex
	NodeNum uint32 `json:"nodeNum"`           // Meshtastic node number, 0 = unset
	NodeID  string `json:"nodeId,omitempty"`  // human form "!acaaba94"
}

// SetPeerMeshNode associates a friend's Vox public key with a Meshtastic node
// number for E2E encrypted DM over LoRa. Pass 0 to clear the mapping.
func (s *MeshtasticService) SetPeerMeshNode(peerID string, nodeNum uint32) error {
	if peerID == "" {
		return errors.New("peerID is empty")
	}
	if s.store == nil {
		return errors.New("storage unavailable")
	}
	res, err := s.store.DB().Exec(
		`UPDATE contacts SET meshtastic_nodenum = ? WHERE peer_id = ?`,
		nodeNum, peerID)
	if err != nil {
		return fmt.Errorf("update contact mesh node: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("no contact with peerID %q (add as friend first)", peerID)
	}
	s.log.Info("peer mesh mapping",
		"peer", safeShort(peerID),
		"nodenum", fmt.Sprintf("%#x", nodeNum))
	return nil
}

// GetPeerMeshNode returns the configured mapping for one peer. Returns
// nodeNum=0 if no mapping is set.
func (s *MeshtasticService) GetPeerMeshNode(peerID string) (PeerMeshMapping, error) {
	out := PeerMeshMapping{PeerID: peerID}
	if s.store == nil || peerID == "" {
		return out, nil
	}
	if err := s.store.DB().QueryRow(
		`SELECT meshtastic_nodenum FROM contacts WHERE peer_id = ?`, peerID,
	).Scan(&out.NodeNum); err != nil {
		// No row = no mapping = NodeNum stays 0; not an error.
		return out, nil
	}
	if out.NodeNum != 0 {
		out.NodeID = meshtastic.FormatNodeID(out.NodeNum)
	}
	return out, nil
}

// findPeerByMeshNode reverse-lookup: which Vox friend corresponds to this
// Meshtastic node number? Returns "" if no mapping.
func (s *MeshtasticService) findPeerByMeshNode(nodeNum uint32) string {
	if s.store == nil || nodeNum == 0 {
		return ""
	}
	var peerID string
	if err := s.store.DB().QueryRow(
		`SELECT peer_id FROM contacts WHERE meshtastic_nodenum = ? LIMIT 1`,
		nodeNum,
	).Scan(&peerID); err != nil {
		return ""
	}
	return peerID
}

