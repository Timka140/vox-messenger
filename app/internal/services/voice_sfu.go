package services

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"vox_app/internal/network"
)

const (
	// sfuReadLimit — совпадает с maxFrameSize SFU-сервера (server/sfu/internal/handler).
	// Прежние 512 байт были растяжкой: фрейм больше лимита = gorilla рвёт
	// соединение кодом 1009.
	sfuReadLimit = 4096
	// sfuPongWait — max тишина от SFU (сервер пингует каждые ~25s). Без
	// дедлайна мёртвый SFU оставлял клиента висеть в ReadMessage вечно
	// с sfuActive=true — «подключено», но тишина, и никакого fallback на mesh.
	sfuPongWait = 60 * time.Second
)

// sfuConn — клиентское WebSocket-соединение с SFU сервером.
// Все поля создаются при connectSFU и читаются только из своих goroutines.
// sfuConn целиком заменяется под s.mu при connect/disconnect.
type sfuConn struct {
	ws      *websocket.Conn
	writeCh chan []byte
	stopCh  chan struct{}
	roomID  string
	myHash  uint32
	log     *slog.Logger
	vkeys   *voiceE2E // шифрование кадров (voice_e2e.go); без ключа кадр не уходит

	// wMu сериализует ws.Write* вызовы. Требуется т.к. WriteControl (из close)
	// и WriteMessage (из writePump) идут из разных goroutines — без этого
	// gorilla/websocket роняет процесс на Windows (Exception 0xc0000005 в WSARecv).
	wMu sync.Mutex
	// closeOnce гарантирует идемпотентный close() — disconnectSFU,
	// onSFUDisconnect и writePump могут вызвать его параллельно.
	closeOnce sync.Once
}

// send отправляет фрейм в SFU. Non-blocking: если writeCh полон — фрейм дропается.
// Кадр шифруется ключом отправителя; ключа нет — кадр не уходит.
func (c *sfuConn) send(payload []byte) {
	hdr := wrapVOXS(nil, c.myHash)
	body := c.vkeys.seal(hdr, c.myHash, payload)
	if body == nil {
		return
	}
	wrapped := append(hdr, body...)
	select {
	case c.writeCh <- wrapped:
	default:
		// drop: SFU write pump не успевает
	}
}

// close будит read pump через SetReadDeadline и шлёт серверу Close-frame.
// Реальный ws.Close() делает ТОЛЬКО sfuReadPump после выхода из ReadMessage —
// чтобы не было одновременного Close+Read (это давало access violation на Windows).
func (c *sfuConn) close() {
	c.closeOnce.Do(func() {
		close(c.stopCh)
		// Пробуждаем залипший ReadMessage — он вернётся с timeout error и закроет ws.
		_ = c.ws.SetReadDeadline(time.Now())
		// Вежливое прощание серверу (best-effort, не ждём ответа).
		c.wMu.Lock()
		_ = c.ws.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(time.Second),
		)
		c.wMu.Unlock()
	})
}

// connectSFU устанавливает WebSocket соединение с SFU. Вызывается из goroutine.
// При успехе ставит s.sfuActive=true и запускает read/write pump.
func (s *VoiceService) connectSFU(groupID string) {
	s.mu.Lock()
	// Guard: не подключаться если уже подключены или подключаемся
	if s.sfuActive || s.sfuConn != nil {
		s.mu.Unlock()
		return
	}
	endpoint := s.sfuEndpoint
	secret := s.sfuSecret
	certHash := s.sfuCertHash
	if endpoint == "" || !s.sfuEnabled {
		s.mu.Unlock()
		return
	}
	myID, err := s.sess.PeerID()
	if err != nil {
		s.mu.Unlock()
		return
	}
	// Ставим sfuActive сразу чтобы другие goroutine не прошли guard
	s.sfuActive = true
	s.mu.Unlock()

	// Generate HMAC token
	ts := time.Now().Unix()
	token := generateSFUToken(secret, myID, groupID, ts)
	url := fmt.Sprintf("%s?room=%s&peer=%s&token=%s&ts=%d", endpoint, groupID, myID, token, ts)

	s.log.Info("SFU connecting", slog.String("group", safeShort8(groupID)), slog.String("endpoint", endpoint))

	// wss: SFU живёт на том же узле, что и сигналка, с тем же сертификатом —
	// принимаем только его (отпечаток подтверждён ключом узла при входе).
	dialer := *websocket.DefaultDialer
	if strings.HasPrefix(endpoint, "wss://") {
		if len(certHash) == 0 {
			s.log.Warn("SFU wss без подтверждённого сертификата узла — не подключаюсь")
			s.mu.Lock()
			s.sfuActive = false
			s.mu.Unlock()
			return
		}
		dialer.TLSClientConfig = network.PinnedTLS(certHash)
	}
	ws, _, err := dialer.Dial(url, nil)
	if err != nil {
		s.log.Warn("SFU connect failed", slog.String("error", err.Error()))
		s.mu.Lock()
		s.sfuActive = false
		s.mu.Unlock()
		return
	}

	conn := &sfuConn{
		ws:      ws,
		writeCh: make(chan []byte, 50),
		stopCh:  make(chan struct{}),
		roomID:  groupID,
		myHash:  peerIDHash(myID),
		log:     s.log,
		vkeys:   s.vkeys,
	}

	s.mu.Lock()
	// Verify we're still in the same voice session
	if s.activeGroupID != groupID {
		s.sfuActive = false
		s.mu.Unlock()
		ws.Close()
		return
	}
	s.sfuConn = conn

	// Build hash↔peerID maps for receive path
	s.sfuPeerHash = make(map[string]uint32)
	s.sfuHashPeer = make(map[uint32]string)
	s.sfuPeerHash[myID] = conn.myHash
	s.sfuHashPeer[conn.myHash] = myID
	for peerID := range s.participants {
		h := peerIDHash(peerID)
		s.sfuPeerHash[peerID] = h
		s.sfuHashPeer[h] = peerID
	}
	s.mu.Unlock()

	s.log.Info("SFU connected", slog.String("group", safeShort8(groupID)))

	go s.sfuWritePump(conn)
	go s.sfuReadPump(conn)
}

// disconnectSFU закрывает SFU соединение. Вызывается из goroutine.
func (s *VoiceService) disconnectSFU() {
	s.mu.Lock()
	conn := s.sfuConn
	s.sfuConn = nil
	s.sfuActive = false
	s.sfuPeerHash = nil
	s.sfuHashPeer = nil
	s.mu.Unlock()

	if conn != nil {
		conn.close()
		s.log.Info("SFU disconnected")
	}
}

// sfuWritePump — goroutine: читает из writeCh и пишет в WebSocket.
// WriteMessage под wMu чтобы не конфликтовать с WriteControl из close().
func (s *VoiceService) sfuWritePump(conn *sfuConn) {
	for {
		select {
		case <-conn.stopCh:
			return
		case frame, ok := <-conn.writeCh:
			if !ok {
				return
			}
			conn.wMu.Lock()
			err := conn.ws.WriteMessage(websocket.BinaryMessage, frame)
			conn.wMu.Unlock()
			if err != nil {
				conn.log.Debug("SFU write error", slog.String("error", err.Error()))
				// markSFUDisconnected + conn.close() разбудит read pump, он закроет ws.
				s.onSFUDisconnect(conn)
				return
			}
		}
	}
}

// sfuReadPump — goroutine: читает из WebSocket, снимает VOXS заголовок,
// маппит sender_hash на peerID и вызывает handleVoiceFrame.
//
// Единственная точка вызова ws.Close() — здесь, после выхода из ReadMessage.
// Все остальные пути закрытия (close/disconnect) только будят read pump через
// SetReadDeadline, чтобы избежать concurrent Read/Close (крэш на Windows).
func (s *VoiceService) sfuReadPump(conn *sfuConn) {
	conn.ws.SetReadLimit(sfuReadLimit)
	defer conn.ws.Close()

	// Продление дедлайна с оглядкой на stopCh: close() будит read pump
	// немедленным дедлайном — после этого продлевать нельзя, иначе wake
	// потеряется и pump зависнет ещё на sfuPongWait.
	refreshDeadline := func() {
		select {
		case <-conn.stopCh:
		default:
			_ = conn.ws.SetReadDeadline(time.Now().Add(sfuPongWait))
		}
	}
	refreshDeadline()
	conn.ws.SetPingHandler(func(appData string) error {
		refreshDeadline()
		// Кастомный PingHandler заменяет дефолтный — pong шлём сами.
		// WriteControl потокобезопасен относительно WriteMessage write pump'а.
		return conn.ws.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(5*time.Second))
	})

	for {
		_, msg, err := conn.ws.ReadMessage()
		if err != nil {
			// Проверяем, был ли это graceful close (через close()) или реальная ошибка.
			select {
			case <-conn.stopCh:
				// Graceful: sfuConn уже занулён в disconnectSFU/onSFUDisconnect.
			default:
				s.markSFUDisconnected(conn)
			}
			return
		}

		refreshDeadline()

		payload, senderHash, ok := unwrapVOXS(msg)
		if !ok {
			continue
		}

		// Map hash → peerID
		s.mu.Lock()
		peerID, found := s.sfuHashPeer[senderHash]
		if !found {
			// Unknown sender — try to resolve from participants
			for pid := range s.participants {
				h := peerIDHash(pid)
				if h == senderHash {
					s.sfuHashPeer[h] = pid
					s.sfuPeerHash[pid] = h
					peerID = pid
					found = true
					break
				}
			}
		}
		if found {
			s.prof.sfuRecvFrames++
		}
		s.mu.Unlock()

		if !found {
			continue
		}

		// Кадр зашифрован ключом отправителя (voice_e2e.go).
		frame, needKey := s.vkeys.open(peerID, msg[:voiceHdrLen], senderHash, payload)
		if frame == nil {
			if needKey {
				go s.requestVoiceKey(peerID)
			}
			continue
		}
		s.handleVoiceFrame(peerID, frame)
	}
}

// markSFUDisconnected обнуляет s.sfuConn/s.sfuActive если этот conn — текущий.
// Не закрывает ws: это делает sfuReadPump в defer. Идемпотентно.
func (s *VoiceService) markSFUDisconnected(conn *sfuConn) {
	s.mu.Lock()
	if s.sfuConn == conn {
		s.sfuConn = nil
		s.sfuActive = false
		s.log.Warn("SFU connection lost, falling back to mesh")
	}
	s.mu.Unlock()
}

// onSFUDisconnect — вызывается при обрыве SFU соединения из write pump.
// Ставит sfuActive=false → audioLoop fallback на mesh. conn.close() разбудит
// read pump через SetReadDeadline; read pump сам закроет ws.
func (s *VoiceService) onSFUDisconnect(conn *sfuConn) {
	s.markSFUDisconnected(conn)
	conn.close()
}

// generateSFUToken создаёт HMAC-SHA256 токен для авторизации на SFU.
func generateSFUToken(secret, peerID, roomID string, ts int64) string {
	if secret == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%s:%s:%d", peerID, roomID, ts)
	return hex.EncodeToString(mac.Sum(nil))
}

// updateSFUPeerHash добавляет нового участника в hash maps (вызывается под s.mu).
func (s *VoiceService) updateSFUPeerHash(peerID string) {
	if s.sfuPeerHash == nil {
		return
	}
	h := peerIDHash(peerID)
	s.sfuPeerHash[peerID] = h
	s.sfuHashPeer[h] = peerID
}

// removeSFUPeerHash удаляет участника из hash maps (вызывается под s.mu).
func (s *VoiceService) removeSFUPeerHash(peerID string) {
	if s.sfuPeerHash == nil {
		return
	}
	if h, ok := s.sfuPeerHash[peerID]; ok {
		delete(s.sfuHashPeer, h)
		delete(s.sfuPeerHash, peerID)
	}
}

// checkSFUSwitchLocked проверяет нужно ли переключить режим mesh↔SFU.
// Вызывается под s.mu из handleVoiceJoinLocked/handleVoiceLeaveLocked.
func (s *VoiceService) checkSFUSwitchLocked() {
	if !s.sfuEnabled || s.sfuEndpoint == "" || s.activeGroupID == "" {
		return
	}
	count := len(s.participants) + 1 // +1 за себя
	shouldUseSFU := s.sfuForce || count >= s.sfuThreshold

	if shouldUseSFU && !s.sfuActive {
		groupID := s.activeGroupID
		go s.connectSFU(groupID)
	} else if !shouldUseSFU && s.sfuActive {
		go s.disconnectSFU()
	}
}

// GetSFUStatus возвращает текущее состояние SFU (для отладки/UI).
func (s *VoiceService) GetSFUStatus() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"enabled":   s.sfuEnabled,
		"force":     s.sfuForce,
		"threshold": s.sfuThreshold,
		"endpoint":  s.sfuEndpoint,
		"active":    s.sfuActive,
		"connected": s.sfuConn != nil,
	}
}

// SetSFUEnabled включает/выключает SFU. При выключении отключает активное SFU соединение.
func (s *VoiceService) SetSFUEnabled(enabled bool) error {
	val := "0"
	if enabled {
		val = "1"
	}
	_, err := s.store.DB().Exec("INSERT OR REPLACE INTO settings (key, value) VALUES ('sfu_enabled', ?)", val)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.sfuEnabled = enabled
	shouldDisconnect := !enabled && s.sfuActive
	s.mu.Unlock()
	if shouldDisconnect {
		go s.disconnectSFU()
	}
	return nil
}

// SetSFUForce включает/выключает принудительный SFU (для тестирования).
func (s *VoiceService) SetSFUForce(force bool) error {
	val := "0"
	if force {
		val = "1"
	}
	_, err := s.store.DB().Exec("INSERT OR REPLACE INTO settings (key, value) VALUES ('sfu_force', ?)", val)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.sfuForce = force
	// Если включили force и мы уже в канале — подключиться к SFU
	shouldConnect := force && s.sfuEnabled && s.sfuEndpoint != "" && s.activeGroupID != "" && !s.sfuActive
	groupID := s.activeGroupID
	s.mu.Unlock()
	if shouldConnect {
		go s.connectSFU(groupID)
	}
	return nil
}

// GetSFUForce возвращает текущее значение force.
func (s *VoiceService) GetSFUForce() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sfuForce, nil
}

// GetSFUEnabled возвращает текущее значение настройки SFU.
func (s *VoiceService) GetSFUEnabled() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sfuEnabled, nil
}

// SetSFUThreshold устанавливает порог участников для автопереключения на SFU.
func (s *VoiceService) SetSFUThreshold(n int) error {
	if n < 2 {
		n = 2
	}
	_, err := s.store.DB().Exec("INSERT OR REPLACE INTO settings (key, value) VALUES ('sfu_threshold', ?)", strconv.Itoa(n))
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.sfuThreshold = n
	s.mu.Unlock()
	return nil
}

// GetSFUThreshold возвращает текущий порог.
func (s *VoiceService) GetSFUThreshold() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sfuThreshold, nil
}

// LoadSFUSettings загружает SFU настройки из БД. Вызывается при старте.
func (s *VoiceService) LoadSFUSettings() {
	var val string
	if s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'sfu_enabled'").Scan(&val) == nil {
		s.sfuEnabled = val == "1"
	}
	if s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'sfu_force'").Scan(&val) == nil {
		s.sfuForce = val == "1"
	}
	if s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'sfu_threshold'").Scan(&val) == nil {
		if n, err := strconv.Atoi(val); err == nil && n >= 2 {
			s.sfuThreshold = n
		}
	}
}

// SetSFUConfig вызывается при получении SFU endpoint от signal server.
func (s *VoiceService) SetSFUConfig(endpoint, secret string, certHash []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sfuEndpoint = endpoint
	s.sfuSecret = secret
	s.sfuCertHash = certHash
	s.log.Info("SFU config received",
		slog.String("endpoint", endpoint),
		slog.Bool("has_secret", secret != ""))
}
