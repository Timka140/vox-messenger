package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
	"vox_app/internal/logsafe"
	"vox_app/internal/network"
	"vox_app/internal/session"
	"vox_app/internal/storage"
)

// SignalServerInfo — информация о сигнальном сервере для фронтенда
type SignalServerInfo struct {
	Addr    string `json:"addr"`
	ShortID string `json:"shortId"`
	Online  bool   `json:"online"`
	Pubkey  string `json:"pubkey,omitempty"`
}

// signalServerEntry — внутренняя строка signal_servers (адрес + ожидаемый pubkey).
// Pubkey пустой для legacy-записей до первого TOFU.
type signalServerEntry struct {
	Addr   string
	Pubkey string
}

// OnP2PMessageFunc — колбэк для входящих P2P сообщений (устанавливается MessagingService)
type OnP2PMessageFunc func(fromPubKey string, data []byte)

type NetworkService struct {
	log   *slog.Logger
	sess  *session.Session
	store *storage.Storage

	mu      sync.RWMutex
	client  *network.SignalClient
	peerMgr *network.PeerManager

	// Колбэк для отправки событий в Wails (устанавливается через SetEmitter)
	emit func(eventName string, data any)

	// Ссылка на FriendsService для обработки входящих friend сообщений
	friends *FriendsService

	// Ссылка на GroupsService для обработки входящих group_* сообщений
	// server relay (delivery_mode='server'). Устанавливается через SetGroupsService.
	groups *GroupsService

	// Колбэк для входящих P2P сообщений
	onP2PMessage OnP2PMessageFunc

	// Колбэки P2P-lifecycle, живущие вне EventBus (hot path / voice internals).
	// Остальные уведомления (peer online/offline, p2p ready, latest release)
	// идут через s.bus — см. ниже.
	onPeerConnected      func(pubKey string)
	onVoicePeerConnected func(pubKey string)
	onVoiceFrame         func(fromPubKey string, pcmData []byte)
	onAttachMessage      func(fromPubKey string, data []byte)
	onScreenMessage      func(fromPubKey string, data []byte)
	onScreenClosed       func(fromPubKey string)
	// onProfileSync — «DataChannel с пиром готов, обменяйтесь профилем» (хеш
	// фото, AvatarService). Зовётся на открытие DC и после ICE-restart.
	onProfileSync func(pubKey string)
	// onScreenAudio — пакеты звука демонстрации (network_screen_audio.go).
	onScreenAudio func(fromPubKey string, data []byte)
	// blocks — заблокированные пиры (blocklist.go).
	blocks blockList
	// peerCaps — возможности пира из P2P-сообщения caps (фаза 5e), например
	// "screenAudio". Стираются при разрыве P2P, приходят заново при подключении.
	capsMu   sync.RWMutex
	peerCaps map[string]map[string]bool

	// Auto-reconnect к сигнальному серверу
	reconnectStop chan struct{}
	// lastSignalAddr — узел последнего успешного подключения: бонус при
	// выборе узла, чтобы не прыгать между узлами (network_select.go).
	lastSignalAddr string

	// connectMu сериализует Connect/ReconnectSignal: параллельные вызовы
	// (пользователь + reconnect loop + AddSignalServer) создавали два клиента
	// и два PeerManager'а, проигравший оставался зомби с живым readLoop.
	connectMu sync.Mutex

	// Колбэк при отключении пира (для очистки голосового канала)
	onVoicePeerDisconnected func(pubKey string)

	// Ссылка на VoiceService для передачи SFU config от signal server
	voice *VoiceService

	// EventBus для кросс-сервисных уведомлений. Подписчики — CallsService,
	// GroupsService, UpdateService. См. bus.go / TopicXxx. Может быть nil в
	// юнит-тестах — все Emit-вызовы делают nil-check.
	bus *EventBus

	// Колбэк для входящих call-сигналов через signal server
	onCallSignal func(from string, msgType string, payload json.RawMessage)

	// P2P auto-reconnect
	reconnectTimers   map[string]*time.Timer
	reconnectAttempts map[string]int
	reconnectMu       sync.Mutex
	shuttingDown      bool
	// signalOnline — пиры, о которых сигналка сказала peer_online и ещё не
	// сказала peer_offline. Нужен scheduleReconnect'у: после исчерпания
	// быстрых попыток продолжать медленные, пока пир числится онлайн.
	// Защищён reconnectMu.
	signalOnline map[string]bool

	// In-memory кеш никнеймов — для не-друзей, чьи ники не хранятся в contacts.
	// Обновляется при получении nickname_update P2P и при voice_join с полем Nickname.
	nicknameCache   map[string]string
	nicknameCacheMu sync.RWMutex

	// Тестовые хуки (nil в продакшене)
	testSender    func(peerID string, data []byte) error
	testConnected func(peerID string) bool

	// Debug: путь к лог-файлу для отправки на сервер
	logFilePath string

	// Статус «в сети» только для друзей и общих групп (presence.go):
	// последний отправленный серверу watch-список и таймер дебаунса.
	presMu    sync.Mutex
	presSent  string
	presTimer *time.Timer
}

// p2pReconnectDelays — задержки перед каждой попыткой P2P-реконнекта.
// Длина массива одновременно служит лимитом попыток (см. scheduleReconnect).
// Раньше было 2 захардкоженных места: `if attempt >= 4` и literal slice из
// 4 элементов — рассинхрон при изменении одного из них давал panic
// `index out of range`.
var p2pReconnectDelays = []time.Duration{
	2 * time.Second,
	5 * time.Second,
	10 * time.Second,
	20 * time.Second,
}

// p2pSlowRetryDelay — после исчерпания p2pReconnectDelays продолжаем пробовать
// с этим шагом, пока пир числится онлайн по сигналке (signalOnline). Раньше
// после 4 попыток (37 с) реконнект замолкал навсегда: оба онлайн по сигналке,
// P2P «Offline» 20+ минут, чинилось только перезапуском (glare 2026-09-08).
const p2pSlowRetryDelay = 60 * time.Second

// SetLogFilePath устанавливает путь к лог-файлу для debug-сбора
func (s *NetworkService) SetLogFilePath(path string) {
	s.logFilePath = path
}

func NewNetworkService(log *slog.Logger, sess *session.Session, store *storage.Storage) *NetworkService {
	svc := &NetworkService{
		log:               log,
		sess:              sess,
		store:             store,
		reconnectTimers:   make(map[string]*time.Timer),
		reconnectAttempts: make(map[string]int),
		signalOnline:      make(map[string]bool),
	}
	return svc
}

// SetEventBus подключает EventBus для кросс-сервисных уведомлений.
// Должен вызываться ДО Set-методов колбэков ниже (они реализованы как shims
// поверх bus.Subscribe), иначе подписка молча потеряется.
func (s *NetworkService) SetEventBus(bus *EventBus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bus = bus
}

// Bus возвращает подключённый EventBus (может быть nil).
func (s *NetworkService) Bus() *EventBus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.bus
}

// emitBus — безопасный helper: nil-check и type-preservation. Используется
// внутри NetworkService вместо прямого вызова s.bus.Emit.
func (s *NetworkService) emitBus(topic string, payload any) {
	bus := s.Bus()
	if bus == nil {
		return
	}
	bus.Emit(topic, payload)
}

// subscribeString — helper для Set-методов shim'ов. Подписывает обёртку,
// которая делает type assertion на string и пропускает payload неверного типа.
func (s *NetworkService) subscribeString(topic string, fn func(string)) {
	bus := s.Bus()
	if bus == nil {
		s.log.Warn("EventBus не установлен, подписка потеряна",
			slog.String("topic", topic))
		return
	}
	bus.Subscribe(topic, func(p any) {
		if v, ok := p.(string); ok {
			fn(v)
		}
	})
}

// SetOnP2PMessage устанавливает колбэк для входящих P2P сообщений
func (s *NetworkService) SetOnP2PMessage(fn OnP2PMessageFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onP2PMessage = fn
}

// SetOnPeerConnected устанавливает колбэк при установке P2P соединения
func (s *NetworkService) SetOnPeerConnected(fn func(pubKey string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onPeerConnected = fn
}

// IsP2PConnected проверяет есть ли активное P2P соединение с пиром.
// Используется сервисами вместо прямого PeerMgr().IsConnected().
func (s *NetworkService) IsP2PConnected(peerID string) bool {
	s.mu.RLock()
	fn := s.testConnected
	pm := s.peerMgr
	s.mu.RUnlock()
	if fn != nil {
		return fn(peerID)
	}
	return pm != nil && pm.IsConnected(peerID)
}

// IsVoiceDCOpen возвращает true если Voice DataChannel пира готов к отправке.
func (s *NetworkService) IsVoiceDCOpen(peerID string) bool {
	s.mu.RLock()
	pm := s.peerMgr
	s.mu.RUnlock()
	return pm != nil && pm.IsVoiceDCOpen(peerID)
}

// SendVoiceFrame отправляет binary PCM фрейм через voice DataChannel.
func (s *NetworkService) SendVoiceFrame(peerID string, pcmData []byte) error {
	s.mu.RLock()
	pm := s.peerMgr
	s.mu.RUnlock()
	if pm == nil {
		return fmt.Errorf("no peer manager")
	}
	return pm.SendVoiceFrame(peerID, pcmData)
}

// SetOnVoiceFrame устанавливает колбэк для входящих voice фреймов.
func (s *NetworkService) SetOnVoiceFrame(fn func(fromPubKey string, pcmData []byte)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onVoiceFrame = fn
	if s.peerMgr != nil {
		s.peerMgr.SetOnVoiceFrame(fn)
	}
}

// SendP2PData отправляет данные через P2P DataChannel.
// Используется сервисами вместо прямого PeerMgr().SendData().
func (s *NetworkService) SendP2PData(peerID string, data []byte) error {
	s.mu.RLock()
	fn := s.testSender
	pm := s.peerMgr
	s.mu.RUnlock()
	if fn != nil {
		return fn(peerID, data)
	}
	if pm == nil {
		return fmt.Errorf("no peer manager")
	}
	return pm.SendData(peerID, data)
}

// SendP2PDataThrottled отправляет данные с backpressure — ждёт если SCTP буфер переполнен.
// Используется для больших данных (voice chunks, файлы), чтобы не блокировать realtime voice.
func (s *NetworkService) SendP2PDataThrottled(peerID string, data []byte) error {
	s.mu.RLock()
	pm := s.peerMgr
	s.mu.RUnlock()
	if pm == nil {
		return fmt.Errorf("no peer manager")
	}
	return pm.SendDataThrottled(peerID, data)
}

// SendAttachChunk отправляет бинарный чанк через DataChannel "attachments".
// Backpressure 8 MB — больше чем у "messages" (2 MB), потому что эти chunks
// толстые (256 KB) и нужно держать TURN-окно заполненным.
func (s *NetworkService) SendAttachChunk(peerID string, data []byte) error {
	s.mu.RLock()
	pm := s.peerMgr
	s.mu.RUnlock()
	if pm == nil {
		return fmt.Errorf("no peer manager")
	}
	return pm.SendAttachChunk(peerID, data)
}

// AttachBufferedAmount — байты в SCTP-буфере "attachments" DC пира (для
// `file transfer stats`).
func (s *NetworkService) AttachBufferedAmount(peerID string) uint64 {
	s.mu.RLock()
	pm := s.peerMgr
	s.mu.RUnlock()
	if pm == nil {
		return 0
	}
	return pm.AttachBufferedAmount(peerID)
}

// IsAttachDCOpen — true если бинарный канал готов отправлять чанки.
func (s *NetworkService) IsAttachDCOpen(peerID string) bool {
	s.mu.RLock()
	pm := s.peerMgr
	s.mu.RUnlock()
	return pm != nil && pm.IsAttachDCOpen(peerID)
}

// EnsureFilesPCReady — ленивый подъём files PeerConnection (PC #1) с пиром
// и блокирующее ожидание открытия attachments DC. Вызывать перед первой
// отправкой чанков/manifest.
func (s *NetworkService) EnsureFilesPCReady(ctx context.Context, peerID string) error {
	s.mu.RLock()
	pm := s.peerMgr
	s.mu.RUnlock()
	if pm == nil {
		return fmt.Errorf("no peer manager")
	}
	return pm.EnsureFilesPCReady(ctx, peerID)
}

// SetOnAttachMessage — callback входящих бинарных attachment-фреймов.
func (s *NetworkService) SetOnAttachMessage(fn func(fromPubKey string, data []byte)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onAttachMessage = fn
	if s.peerMgr != nil {
		s.peerMgr.SetOnAttachMessage(network.OnP2PMessage(fn))
	}
}

// SetOnScreenMessage — callback входящих бинарных screen-фреймов.
func (s *NetworkService) SetOnScreenMessage(fn func(fromPubKey string, data []byte)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onScreenMessage = fn
	if s.peerMgr != nil {
		s.peerMgr.SetOnScreenMessage(network.OnP2PMessage(fn))
	}
}

// SetOnScreenClosed — callback закрытия screen DC пира (см. PeerManager.SetOnScreenClosed).
func (s *NetworkService) SetOnScreenClosed(fn func(fromPubKey string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onScreenClosed = fn
	if s.peerMgr != nil {
		s.peerMgr.SetOnScreenClosed(fn)
	}
}

// SendScreenChunk отправляет бинарный screen-фрейм через DataChannel "screen".
// Backpressure 512 KiB — см. screenSendBufferThreshold в network/peermgr.go.
func (s *NetworkService) SendScreenChunk(peerID string, data []byte) error {
	s.mu.RLock()
	pm := s.peerMgr
	s.mu.RUnlock()
	if pm == nil {
		return fmt.Errorf("no peer manager")
	}
	return pm.SendScreenChunk(peerID, data)
}

// IsScreenDCOpen — true если screen DataChannel пира готов отправлять.
func (s *NetworkService) IsScreenDCOpen(peerID string) bool {
	s.mu.RLock()
	pm := s.peerMgr
	s.mu.RUnlock()
	return pm != nil && pm.IsScreenDCOpen(peerID)
}

// EnsureScreenPCReady — ленивый подъём screen PeerConnection (PC #2) и
// блокирующее ожидание открытия "screen" DC. Вызывать из ScreenShareService
// перед первой отправкой кадра пиру.
func (s *NetworkService) EnsureScreenPCReady(ctx context.Context, peerID string) error {
	s.mu.RLock()
	pm := s.peerMgr
	s.mu.RUnlock()
	if pm == nil {
		return fmt.Errorf("no peer manager")
	}
	return pm.EnsureScreenPCReady(ctx, peerID)
}

// CloseScreenPC закрывает screen PC с конкретным пиром. Вызывается из
// ScreenShareService.StopScreenShare для своих пиров, а у зрителей —
// можно закрыть когда отправитель прислал явное "stop" (не реализовано в v1:
// PC сам закроется на disconnect/close по штатному pion lifecycle).
func (s *NetworkService) CloseScreenPC(peerID string) {
	s.mu.RLock()
	pm := s.peerMgr
	s.mu.RUnlock()
	if pm == nil {
		return
	}
	pm.CloseScreenPC(peerID)
}

// ScreenDCSaturation — максимальный SCTP-buffered amount среди screen DC всех
// пиров. ScreenShareService использует это для проактивного rate-control'а.
func (s *NetworkService) ScreenDCSaturation() uint64 {
	s.mu.RLock()
	pm := s.peerMgr
	s.mu.RUnlock()
	if pm == nil {
		return 0
	}
	return pm.ScreenDCSaturation()
}

// BroadcastNicknameUpdate рассылает обновление ника всем подключённым пирам через P2P.
func (s *NetworkService) BroadcastNicknameUpdate(nickname string) {
	s.mu.RLock()
	pm := s.peerMgr
	s.mu.RUnlock()
	if pm == nil {
		return
	}

	msg := struct {
		Type    string `json:"type"`
		Content string `json:"content"`
	}{Type: "nickname_update", Content: nickname}

	data, err := json.Marshal(msg)
	if err != nil {
		return
	}

	for _, peerID := range pm.GetConnectedPeers() {
		if err := pm.SendData(peerID, data); err != nil {
			s.log.Debug("не удалось отправить nickname_update",
				slog.String("peer", safeShort(peerID)),
				slog.String("error", err.Error()))
		}
	}

	s.log.Info("📢 НИК ОБНОВЛЁН ДЛЯ ПИРОВ")
}

// SetTestSender устанавливает mock отправки P2P (для testkit).
func (s *NetworkService) SetTestSender(fn func(string, []byte) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.testSender = fn
}

// SetTestConnected устанавливает mock проверки подключения (для testkit).
func (s *NetworkService) SetTestConnected(fn func(string) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.testConnected = fn
}

// TestTriggerP2PMessage имитирует входящее P2P сообщение (для testkit).
func (s *NetworkService) TestTriggerP2PMessage(fromPubKey string, data []byte) {
	s.onP2PDataMessage(fromPubKey, data)
}

// PeerMgr возвращает PeerManager
func (s *NetworkService) PeerMgr() *network.PeerManager {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.peerMgr
}

// ConnectToPeer инициирует P2P соединение с пиром (вызывается после принятия заявки)
func (s *NetworkService) ConnectToPeer(peerPubKey string) {
	if s.IsBlocked(peerPubKey) {
		return // заблокирован (blocklist.go)
	}
	s.mu.RLock()
	pm := s.peerMgr
	s.mu.RUnlock()

	if pm == nil {
		return
	}
	if pm.HasActivePeer(peerPubKey) {
		return
	}
	go func() {
		if err := pm.InitiateConnection(peerPubKey); err != nil {
			if errors.Is(err, network.ErrSlotBusy) {
				s.log.Debug("P2P: handshake уже идёт, пропускаем",
					slog.String("peer", safeShort(peerPubKey)))
				return
			}
			s.log.Warn("не удалось инициировать P2P",
				slog.String("peer", safeShort(peerPubKey)),
				slog.String("error", err.Error()))
		}
	}()
}

// Wire подключает обратные ссылки на сервисы, которые циклически зависят от
// NetworkService (они созданы ПОСЛЕ network и передали его себе в конструкторе).
// Вызывается один раз при bootstrap из main.go после создания всех сервисов.
//
// Любой из аргументов может быть nil (например в тестовых окружениях где
// FriendsService не создан) — onSignalMessage дропнет соответствующее сообщение
// с Warn-логом.
func (s *NetworkService) Wire(friends *FriendsService, groups *GroupsService, voice *VoiceService) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.friends = friends
	s.groups = groups
	s.voice = voice
}

// Client возвращает signal client (для использования другими сервисами)
func (s *NetworkService) Client() *network.SignalClient {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.client
}

// SetEmitter устанавливает функцию для отправки событий во фронтенд
func (s *NetworkService) SetEmitter(emit func(eventName string, data any)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emit = emit
}

// Connect подключается к первому доступному сигнальному серверу
func (s *NetworkService) Connect(listenAddr, dialAddr, seedAddr string) error {
	const op = "NetworkService.Connect"

	// Сериализуем весь Connect: параллельный вызов создал бы второй клиент и
	// PeerManager, проигравший стал бы зомби с живым readLoop и лишним
	// reconnect-циклом.
	s.connectMu.Lock()
	defer s.connectMu.Unlock()

	// Получаем ключи из сессии
	privKey, err := s.sess.PrivateKey()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	pubKey, err := s.sess.PublicKey()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}

	// Получаем nick#tag
	nickTag, err := s.getNickTag()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}

	// Останавливаем предыдущий reconnect loop (если был)
	s.stopReconnectLoop()

	s.log.Info("подключение к сигнальному серверу", slog.String("nick_tag", nickTag))

	// Получаем список серверов из БД (с pubkey для mutual auth)
	servers, err := s.getServerEntries()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}

	if len(servers) == 0 {
		return fmt.Errorf("%s: нет сигнальных серверов, добавьте сервер в настройках", op)
	}

	// Создаём signal client
	client := network.NewSignalClient(s.log, s.onSignalMessage)

	// Создаём PeerManager ДО подключения к signal — readLoop стартует внутри
	// client.Connect(), и входящие offer/answer/ice должны иметь доступ к peerMgr.
	peerID, _ := s.sess.PeerID()
	pm := network.NewPeerManager(
		s.log,
		peerID,
		client.Send,
		s.onP2PDataMessage,
		s.onP2PStateChanged,
	)
	pm.SetOnDataChannelReady(s.onDataChannelReady)
	pm.SetIsBlocked(s.IsBlocked) // заблокированный: offer отклоняется (blocklist.go)
	pm.SetOnAttachDCOpen(func(pubKey string) {
		s.log.Debug("attach DC ready → доставка pending вложений",
			slog.String("peer", safeShort(pubKey)))
		s.emitBus(TopicAttachReady, pubKey)
	})
	if s.onVoiceFrame != nil {
		pm.SetOnVoiceFrame(s.onVoiceFrame)
	}
	if s.onAttachMessage != nil {
		pm.SetOnAttachMessage(network.OnP2PMessage(s.onAttachMessage))
	}
	if s.onScreenMessage != nil {
		pm.SetOnScreenMessage(network.OnP2PMessage(s.onScreenMessage))
	}
	if s.onScreenClosed != nil {
		pm.SetOnScreenClosed(s.onScreenClosed)
	}
	if s.onScreenAudio != nil {
		pm.SetOnScreenAudio(network.OnP2PMessage(s.onScreenAudio))
	}

	// Устанавливаем callbacks ДО Connect — auth_ok парсится внутри Connect
	client.SetOnDisconnect(func() {
		s.log.Warn("связь с сигнальным сервером потеряна")
		s.emitEvent("vox:signal-updated", nil)
		s.startReconnectLoop()
	})
	client.SetOnLogRequest(func() {
		s.log.Info("📋 сервер запросил логи")
		s.pushLogsToServer()
	})
	client.SetOnLatestRelease(func(info *network.LatestReleaseInfo) {
		s.emitBus(TopicLatestRelease, info)
	})
	client.SetOnSFUConfig(func(endpoint, secret string) {
		if s.voice != nil {
			s.voice.SetSFUConfig(endpoint, secret)
		}
	})
	client.SetPresenceProvider(s.presenceForAuth) // статус «в сети» — presence.go

	s.mu.Lock()
	if s.client != nil {
		s.client.Close()
	}
	if s.peerMgr != nil {
		s.peerMgr.CloseAll()
	}
	s.client = client
	s.peerMgr = pm
	s.mu.Unlock()

	// Пробуем подключиться к серверам по очереди, с retry
	const maxRetries = 3
	const retryDelay = 2 * time.Second

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			s.log.Info("повтор подключения",
				slog.Int("attempt", attempt+1),
				slog.Int("max", maxRetries))
			time.Sleep(retryDelay)
		}

		{
			// Выбор узла, перебор, redirect и TOFU — network_select.go.
			entry, err := s.connectOrdered(client, servers, privKey, pubKey, nickTag)
			if err != nil {
				s.log.Warn("не удалось подключиться ни к одному серверу",
					slog.String("error", err.Error()),
					slog.Int("attempt", attempt+1))
				lastErr = err
				continue
			}

			// Успешно подключились — применяем ICE серверы
			s.applyServerICEServers(client)
			s.loadICEServers(pm)

			s.log.Info("подключён к сигнальному серверу", slog.String("addr", entry.Addr))
			s.emitEvent("vox:signal-updated", nil)

			// Отправляем логи на сервер (автоматически при подключении)
			go s.pushLogsToServer()

			// Подключаемся к онлайн-друзьям
			go s.connectToOnlineFriends()

			// Догоняем пропущенные сообщения в group relay группах.
			s.mu.RLock()
			groups := s.groups
			s.mu.RUnlock()
			if groups != nil {
				go groups.SyncAllServerGroups()
			}

			return nil
		}
	}

	// Все попытки провалились — очищаем peerMgr
	s.mu.Lock()
	s.client = nil
	s.peerMgr = nil
	s.mu.Unlock()
	pm.CloseAll()

	// Запускаем фоновое переподключение
	s.startReconnectLoop()

	return fmt.Errorf("%s: не удалось подключиться ни к одному серверу после %d попыток: %w", op, maxRetries, lastErr)
}

// UpdateNickTag отправляет серверу новый nick_tag через активное WS-соединение.
// НЕ разрывает сигналку и НЕ роняет P2P (в отличие от ReconnectSignal).
// Если сигналка не подключена — молча пропускается, новый ник подхватится при следующем auth.
func (s *NetworkService) UpdateNickTag() error {
	s.mu.RLock()
	client := s.client
	s.mu.RUnlock()
	if client == nil || !client.IsConnected() {
		return nil
	}
	nickTag, err := s.getNickTag()
	if err != nil {
		return fmt.Errorf("NetworkService.UpdateNickTag: %w", err)
	}
	if err := client.SendUpdateNickTag(nickTag); err != nil {
		return fmt.Errorf("NetworkService.UpdateNickTag: %w", err)
	}
	s.log.Info("📛 nick_tag отправлен на сигналку", slog.String("nick_tag", nickTag))
	return nil
}

// ReconnectSignal переподключается к сигнальному серверу БЕЗ пересоздания PeerManager.
// Используется при смене ника — P2P соединения сохраняются.
func (s *NetworkService) ReconnectSignal() error {
	const op = "NetworkService.ReconnectSignal"

	// Тот же mutex, что и Connect — они конкурируют за s.client/s.peerMgr.
	s.connectMu.Lock()
	defer s.connectMu.Unlock()

	privKey, err := s.sess.PrivateKey()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	pubKey, err := s.sess.PublicKey()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}

	nickTag, err := s.getNickTag()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}

	servers, err := s.getServerEntries()
	if err != nil || len(servers) == 0 {
		return fmt.Errorf("%s: нет серверов", op)
	}

	s.log.Info("переподключение к сигнальному серверу", slog.String("nick_tag", nickTag))

	newClient := network.NewSignalClient(s.log, s.onSignalMessage)
	// Callbacks ДО Connect — auth_ok парсится внутри Connect
	newClient.SetOnDisconnect(func() {
		s.log.Warn("связь с сигнальным сервером потеряна")
		s.emitEvent("vox:signal-updated", nil)
		s.startReconnectLoop()
	})
	newClient.SetOnLatestRelease(func(info *network.LatestReleaseInfo) {
		s.emitBus(TopicLatestRelease, info)
	})
	newClient.SetOnSFUConfig(func(endpoint, secret string) {
		if s.voice != nil {
			s.voice.SetSFUConfig(endpoint, secret)
		}
	})
	newClient.SetPresenceProvider(s.presenceForAuth)

	// Выбор узла, перебор, redirect и TOFU — network_select.go.
	if entry, err := s.connectOrdered(newClient, servers, privKey, pubKey, nickTag); err == nil {
		// Применяем серверные ICE-серверы
		s.applyServerICEServers(newClient)

		// Закрываем старый клиент, но НЕ PeerManager
		s.mu.Lock()
		if s.client != nil {
			s.client.Close()
		}
		s.client = newClient
		// Обновляем sendFunc у существующего PeerManager
		if s.peerMgr != nil {
			s.peerMgr.SetSendFunc(newClient.Send)
			s.loadICEServers(s.peerMgr)
		}
		s.mu.Unlock()

		s.log.Info("переподключён к сигнальному серверу", slog.String("addr", entry.Addr))
		s.emitEvent("vox:signal-updated", nil)

		// Подключаемся к друзьям которые были офлайн
		go s.connectToOnlineFriends()

		// Догоняем пропущенные сообщения в group relay группах.
		s.mu.RLock()
		groups := s.groups
		s.mu.RUnlock()
		if groups != nil {
			go groups.SyncAllServerGroups()
		}

		return nil
	}

	s.startReconnectLoop()
	return fmt.Errorf("%s: не удалось переподключиться", op)
}

// Disconnect отключается от сигнального сервера
func (s *NetworkService) Disconnect() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.client != nil {
		s.client.Close()
		s.client = nil
	}

	s.log.Info("отключён от сигнальной сети")
	return nil
}

// Shutdown корректно завершает все сетевые ресурсы при выходе из приложения
func (s *NetworkService) Shutdown() {
	s.log.Info("завершение NetworkService")

	// Отправляем логи перед закрытием (с таймаутом)
	done := make(chan struct{})
	go func() {
		s.pushLogsToServer()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		s.log.Warn("таймаут отправки логов при shutdown")
	}

	// Блокируем новые reconnect-попытки
	s.reconnectMu.Lock()
	s.shuttingDown = true
	for id, t := range s.reconnectTimers {
		t.Stop()
		delete(s.reconnectTimers, id)
	}
	s.reconnectMu.Unlock()

	// Останавливаем reconnect-циклы к сигнальному серверу
	s.stopReconnectLoop()

	// Закрываем все WebRTC peer connections
	s.mu.Lock()
	if s.peerMgr != nil {
		s.peerMgr.CloseAll()
	}
	// Закрываем signal WebSocket
	if s.client != nil {
		s.client.Close()
		s.client = nil
	}
	s.mu.Unlock()
}

// pushLogsToServer отправляет лог-файл на signal server.
// Дополнительно: если рядом лежит vox-app.log.1.unsent — однократно отправляет
// и предыдущую сессию (после крэша/перезагрузки), затем удаляет маркер.
// Перед отправкой каждый файл проходит logsafe.ScrubFile: прошлые сессии могли
// быть записаны версией, которая ещё писала личное.
func (s *NetworkService) pushLogsToServer() {
	if s.logFilePath == "" {
		return
	}

	s.mu.RLock()
	client := s.client
	s.mu.RUnlock()

	if client == nil {
		return
	}

	nickTag, err := s.getNickTag()
	if err != nil {
		s.log.Warn("pushLogs: не удалось получить nickTag", slog.String("error", err.Error()))
		return
	}

	// Пушим все ротированные сессии с .unsent маркером — от самой старой
	// к самой свежей, чтобы на сервере files отсортировались в хронологическом
	// порядке. logRotateKeep=4 в main.go, держим в синхроне.
	const logRotateKeep = 4
	for i := logRotateKeep; i >= 1; i-- {
		prevPath := fmt.Sprintf("%s.%d", s.logFilePath, i)
		prevMarker := prevPath + ".unsent"
		if _, err := os.Stat(prevMarker); err != nil {
			continue
		}
		prevData, err := os.ReadFile(prevPath)
		if err != nil || len(prevData) == 0 {
			_ = os.Remove(prevMarker)
			continue
		}
		prevData = logsafe.ScrubFile(prevData)
		if err := client.SendLogPush(nickTag, prevData); err != nil {
			s.log.Warn("pushLogs: ошибка отправки ротированной сессии",
				slog.Int("slot", i),
				slog.String("error", err.Error()))
			// Маркер не удаляем — попробуем на следующем reconnect
			continue
		}
		s.log.Info("📋 лог прошлой сессии отправлен на сервер",
			slog.Int("slot", i),
			slog.Int("bytes", len(prevData)))
		_ = os.Remove(prevMarker)
	}

	// Затем текущий лог
	data, err := os.ReadFile(s.logFilePath)
	if err != nil || len(data) == 0 {
		return
	}
	data = logsafe.ScrubFile(data)

	if err := client.SendLogPush(nickTag, data); err != nil {
		s.log.Warn("pushLogs: ошибка отправки", slog.String("error", err.Error()))
	} else {
		s.log.Info("📋 логи отправлены на сервер", slog.Int("bytes", len(data)))
	}
}

// SetOnVoicePeerConnected устанавливает колбэк для ре-анонса voice при подключении пира
func (s *NetworkService) SetOnVoicePeerConnected(fn func(pubKey string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onVoicePeerConnected = fn
}

// EmitEvent отправляет событие на фронтенд (экспортированная обёртка для сервисов).
func (s *NetworkService) EmitEvent(name string, data any) {
	s.emitEvent(name, data)
}

func (s *NetworkService) emitEvent(name string, data any) {
	s.mu.RLock()
	emit := s.emit
	s.mu.RUnlock()

	if emit != nil {
		emit(name, data)
	}
	// Друзья, группы или блокировки изменились — watch-список статуса «в сети».
	if presenceEvents[name] {
		s.RefreshPresence()
	}
}

// peerNickname возвращает никнейм пира из контактов или кеша (или shortID если не найден)
func (s *NetworkService) peerNickname(peerID string) string {
	var nick string
	s.store.DB().QueryRow("SELECT nickname FROM contacts WHERE peer_id = ?", peerID).Scan(&nick)
	if nick != "" {
		return nick
	}
	// Fallback на in-memory кеш (для не-друзей)
	s.nicknameCacheMu.RLock()
	cached := s.nicknameCache[peerID]
	s.nicknameCacheMu.RUnlock()
	if cached != "" {
		return cached
	}
	// Ник не-друга, пришедший в прошлых сессиях (peer_profiles, фаза 5a).
	s.store.DB().QueryRow("SELECT nickname FROM peer_profiles WHERE peer_id = ?", peerID).Scan(&nick)
	if nick != "" {
		s.nicknameCacheMu.Lock()
		if s.nicknameCache == nil {
			s.nicknameCache = make(map[string]string)
		}
		s.nicknameCache[peerID] = nick
		s.nicknameCacheMu.Unlock()
		return nick
	}
	return safeShort(peerID)
}

// SetOnProfileSync — колбэк обмена профилем при готовности DataChannel.
func (s *NetworkService) SetOnProfileSync(fn func(pubKey string)) {
	s.mu.Lock()
	s.onProfileSync = fn
	s.mu.Unlock()
}

// CachePeerNickname сохраняет ник пира в кеше и в peer_profiles — для
// не-друзей, чьих ников нет в contacts: без записи в БД после перезапуска
// участники групп снова показывались бы короткими ID.
func (s *NetworkService) CachePeerNickname(peerID, nickname string) {
	if peerID == "" || nickname == "" {
		return
	}
	s.nicknameCacheMu.Lock()
	if s.nicknameCache == nil {
		s.nicknameCache = make(map[string]string)
	}
	changed := s.nicknameCache[peerID] != nickname
	s.nicknameCache[peerID] = nickname
	s.nicknameCacheMu.Unlock()
	if !changed || s.store == nil {
		return
	}
	if _, err := s.store.DB().Exec(
		`INSERT INTO peer_profiles (peer_id, nickname, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(peer_id) DO UPDATE SET nickname = excluded.nickname, updated_at = excluded.updated_at`,
		peerID, nickname, time.Now().Unix()); err != nil {
		s.log.Debug("peer_profiles: ник не сохранён", slog.String("peer", safeShort(peerID)), slog.String("error", err.Error()))
	}
}

// PeerNickname — публичный доступ к peerNickname для других сервисов
func (s *NetworkService) PeerNickname(peerID string) string {
	return s.peerNickname(peerID)
}

// MyNickname возвращает наш текущий никнейм из IdentityService
func (s *NetworkService) MyNickname() string {
	var nick string
	s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'nickname'").Scan(&nick)
	return nick
}

// --- P2P переподключение ---

// SetOnVoicePeerDisconnected устанавливает колбэк при отключении пира (для очистки голосового канала)
func (s *NetworkService) SetOnVoicePeerDisconnected(fn func(pubKey string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onVoicePeerDisconnected = fn
}

// SetOnPeerOnlineForCall — legacy shim поверх EventBus.
//
// Deprecated: подписывайтесь на TopicPeerOnline через EventBus напрямую.
func (s *NetworkService) SetOnPeerOnlineForCall(fn func(peerID string)) {
	s.subscribeString(TopicPeerOnline, fn)
}

// SetOnPeerReconnectedForCall — legacy shim поверх EventBus.
//
// Deprecated: подписывайтесь на TopicPeerOnline через EventBus напрямую.
// Оба callback'а (OnlineForCall и ReconnectedForCall) раньше вызывались
// последовательно на одном peer_online, поэтому через bus оба — TopicPeerOnline.
func (s *NetworkService) SetOnPeerReconnectedForCall(fn func(peerID string)) {
	s.subscribeString(TopicPeerOnline, fn)
}

// SetOnPeerOfflineForCall — legacy shim поверх EventBus.
//
// Deprecated: подписывайтесь на TopicPeerOffline через EventBus напрямую.
func (s *NetworkService) SetOnPeerOfflineForCall(fn func(peerID string)) {
	s.subscribeString(TopicPeerOffline, fn)
}

// SetOnCallSignal устанавливает колбэк для входящих call-сигналов через signal server
func (s *NetworkService) SetOnCallSignal(fn func(from string, msgType string, payload json.RawMessage)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onCallSignal = fn
}

// ConnectToCallPeer инициирует временное P2P соединение для звонка (не друг)
func (s *NetworkService) ConnectToCallPeer(peerPubKey string) {
	if s.IsBlocked(peerPubKey) {
		return // заблокирован (blocklist.go)
	}
	s.mu.RLock()
	pm := s.peerMgr
	s.mu.RUnlock()
	if pm == nil || pm.HasActivePeer(peerPubKey) {
		return
	}
	go func() {
		if err := pm.InitiateCallConnection(peerPubKey); err != nil {
			if errors.Is(err, network.ErrSlotBusy) {
				s.log.Debug("call P2P: handshake уже идёт, пропускаем",
					slog.String("peer", safeShort(peerPubKey)))
				return
			}
			s.log.Warn("не удалось инициировать call P2P",
				slog.String("peer", safeShort(peerPubKey)),
				slog.String("error", err.Error()))
		}
	}()
}

// ConnectToGroupPeer устанавливает P2P с участником группы.
// Друзья → permanent P2P, не-друзья → CallOnly (временный) P2P.
func (s *NetworkService) ConnectToGroupPeer(peerPubKey string) {
	if s.IsBlocked(peerPubKey) {
		return // заблокирован (blocklist.go)
	}
	s.mu.RLock()
	pm := s.peerMgr
	s.mu.RUnlock()
	if pm == nil || pm.HasActivePeer(peerPubKey) {
		return
	}
	// Проверяем — друг ли это
	var count int
	if s.store != nil {
		_ = s.store.DB().QueryRow(
			"SELECT COUNT(*) FROM contacts WHERE peer_id = ? AND status = 'accepted'",
			peerPubKey).Scan(&count)
	}
	if count > 0 {
		s.ConnectToPeer(peerPubKey)
	} else {
		s.ConnectToCallPeer(peerPubKey)
	}
}

// SetOnPeerOnlineForGroups — legacy shim поверх EventBus.
//
// Deprecated: подписывайтесь на TopicPeerOnline через EventBus напрямую.
func (s *NetworkService) SetOnPeerOnlineForGroups(fn func(peerID string)) {
	s.subscribeString(TopicPeerOnline, fn)
}

// SetOnLatestRelease — legacy shim поверх EventBus для информации о последнем
// релизе (из auth_ok payload сигнального сервера).
//
// Deprecated: подписывайтесь на TopicLatestRelease через EventBus напрямую.
func (s *NetworkService) SetOnLatestRelease(fn func(*network.LatestReleaseInfo)) {
	bus := s.Bus()
	if bus == nil {
		s.log.Warn("EventBus не установлен, подписка потеряна",
			slog.String("topic", TopicLatestRelease))
		return
	}
	bus.Subscribe(TopicLatestRelease, func(p any) {
		if v, ok := p.(*network.LatestReleaseInfo); ok {
			fn(v)
		}
	})
}
