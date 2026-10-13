package network

import (
	"crypto/ed25519"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

// DataChannel-метки. Каналы разнесены по трём PeerConnection:
//
//	PC #0 (media):  "messages" + "voice"
//	PC #1 (files):  "attachments"
//	PC #2 (screen): "screen" — H264/JPEG чанки демонстрации экрана
//
// Файлы изолированы от голоса — чанки на отдельном SCTP/DTLS не конкурируют
// с Opus-фреймами за исходящую полосу. См. логи v0.0068: при общем PC одна
// 7-MB передача обнуляла playback на receiver (pb_silence ×10, seq_skip 700+).
// Screen вынесен по той же причине: видеопоток 1-3 Mbps съел бы любой запас
// media SCTP буфера.
const (
	DataChannelLabel       = "messages"
	VoiceDataChannelLabel  = "voice"
	AttachDataChannelLabel = "attachments"
	ScreenDataChannelLabel = "screen"
	// ScreenAudioDataChannelLabel — звук демонстрации (фаза 5e), см. peermgr_screen_audio.go.
	ScreenAudioDataChannelLabel = "screen-audio"
)

// pcKind — discriminator: какой из трёх PeerConnection'ов пира.
// Совпадает с PcId в server/signal/pkg/protocol.Message.
type pcKind int

const (
	pcKindMedia  pcKind = 0 // voice + messages
	pcKindFiles  pcKind = 1 // attachments
	pcKindScreen pcKind = 2 // screen share
)

// injectPcID добавляет поле "pc_id" в JSON-payload (offer/answer/ICE — всегда
// JSON-объекты). Дублирует Message.PcId на случай если signal-сервер старой
// версии не пробрасывает top-level поле — payload для сервера непрозрачный
// (json.RawMessage), pc_id внутри проходит as-is.
// На receive-стороне см. extractPcID — fallback если msg.PcId == 0.
func injectPcID(payload []byte, pcId int) []byte {
	if pcId == 0 {
		return payload
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		return payload
	}
	pcBytes, _ := json.Marshal(pcId)
	m["pc_id"] = pcBytes
	out, err := json.Marshal(m)
	if err != nil {
		return payload
	}
	return out
}

// extractPcID читает pc_id из payload (fallback если top-level msg.PcId == 0).
// Возвращает 0 если поле отсутствует или payload невалидный.
func extractPcID(payload []byte) int {
	var probe struct {
		PcID int `json:"pc_id"`
	}
	_ = json.Unmarshal(payload, &probe)
	return probe.PcID
}

// resolvePcID объединяет top-level msg.PcId с pc_id внутри payload.
// Если top-level не задан (старый сервер), используем payload-fallback.
func resolvePcID(msgPcID int, payload []byte) int {
	if msgPcID != 0 {
		return msgPcID
	}
	return extractPcID(payload)
}

// Публичные STUN серверы
var iceServers = []webrtc.ICEServer{
	{URLs: []string{"stun:stun.l.google.com:19302"}},
	{URLs: []string{"stun:stun1.l.google.com:19302"}},
}

// P2PPeer — активное WebRTC соединение с пиром.
// Два независимых PeerConnection:
//
//	PC (media):  voice + messages — поднимается всегда
//	FilesPC:     attachments — поднимается лениво при первой передаче файла
type P2PPeer struct {
	PublicKey string

	// --- Media PC (voice + messages) ---
	PC            *webrtc.PeerConnection
	DC            *webrtc.DataChannel // "messages" — текст/JSON
	VoiceDC       *webrtc.DataChannel // "voice" — binary PCM фреймы
	Connected     bool
	Initiating    bool // true пока InitiateConnection не завершится — для collision detection
	ICERestarting bool // true пока tryICERestart выполняется — предотвращает параллельные вызовы

	// --- Files PC (attachments) ---
	FilesPC         *webrtc.PeerConnection
	AttachDC        *webrtc.DataChannel // "attachments" — binary chunks
	FilesConnected  bool
	FilesInitiating bool

	// --- Screen PC (screen share) ---
	// Поднимается лениво при StartScreenShare; на стороне зрителя — при
	// получении screen offer от шарящего. Видеопоток высокого битрейта на
	// отдельном SCTP/DTLS, чтобы не давить voice/messages при пиках I-кадров.
	ScreenPC         *webrtc.PeerConnection
	ScreenDC         *webrtc.DataChannel // "screen" — binary H264/JPEG chunks
	ScreenAudioDC    *webrtc.DataChannel // "screen-audio" — Opus звука демонстрации (фаза 5e)
	ScreenConnected  bool
	ScreenInitiating bool

	// --- glare / жизненный цикл offer (см. HandleOffer, initiateConnection) ---
	// answering*: HandleOffer чистит слот и до установки answer-PC работает
	// без лока (createPeerConnection, SetRemoteDescription, CreateAnswer,
	// отправка). Конкурирующий initiateConnection в этом окне видел пустой
	// слот, ставил Initiating и перезаписывал answer-PC своим offer-PC —
	// «stale PC» на нашей стороне и answer в никуда у пира (glare 2026-09-08).
	// offerSentAt*: момент отправки нашего offer (initiateConnection /
	// tryICERestart). По нему impolite-сторона решает, свежий ли её offer
	// (offerGrace), а watchdog — не завис ли PC в have-local-offer без answer
	// (offerAnswerTimeout).
	answering         bool
	filesAnswering    bool
	screenAnswering   bool
	offerSentAt       time.Time
	filesOfferSentAt  time.Time
	screenOfferSentAt time.Time

	// --- shared ---
	VoiceActive     bool          // true = отправляем/получаем аудио
	CallOnly        bool          // true = временный P2P для звонка (не друг)
	sendReady       chan struct{} // backpressure для "messages" DC
	attachSendReady chan struct{} // backpressure для "attachments" DC
	screenSendReady chan struct{} // backpressure для "screen" DC
}

// ErrSlotBusy — initiateConnection пропущен: для этого пира и типа PC уже
// летит наш offer или мы отвечаем на чужой. Это не ошибка транспорта:
// callers логируют Debug, NetworkService.scheduleReconnect перепланирует
// попытку вместо того, чтобы считать «пропущен» успехом.
var ErrSlotBusy = errors.New("p2p slot busy")

const (
	// offerGrace — пока наш offer моложе этого, impolite-сторона при collision
	// игнорирует чужой offer (polite ответит на наш). Старше — answer уже не
	// придёт (пир заменил PC), принимаем входящий вместо вечного
	// «initiateConnection пропущен: уже инициируем».
	offerGrace = 8 * time.Second

	// offerAnswerTimeout — watchdog от отправки offer: если PC за это время не
	// connected (answer не пришёл или ICE не стартовал) → слот сбрасывается
	// через closePeerKind, для media уходит onStateChange(false) и
	// NetworkService планирует reconnect. Без него PC, не получивший remote
	// description или кандидатов, никогда не доходит до failed и Initiating
	// висит навсегда. См. armConnectWatchdog.
	offerAnswerTimeout = 15 * time.Second
	// politeRestartWait — сколько polite-сторона ждёт восстановления после
	// failed/disconnected, прежде чем пересоздать PC сама. Больше
	// offerAnswerTimeout: impolite успевает сбросить свой слот по watchdog и
	// прислать offer с нового PC (reconnect через 2 с) раньше нашего fallback.
	politeRestartWait = offerAnswerTimeout + 3*time.Second

	// connectTimeout — watchdog от применения answer / отправки answer:
	// столько даём ICE дойти до connected (через TURN обычно 3-5 с).
	connectTimeout = 20 * time.Second
)

// sendBufferThreshold — если BufferedAmount media-DC превышает, SendDataThrottled
// ждёт. 2MB достаточно для текстовых JSON и служебных пакетов (voice идёт мимо).
const sendBufferThreshold = 2 * 1024 * 1024 // 2MB

// attachSendBufferThreshold — порог backpressure для бинарного attachments DC.
// 256 KiB при chunk_size 64 KiB означает «не больше 4 чанков в SCTP буфере» —
// каждый chunk дренится <50 ms на типичной связи, send_timeout (5 s) не
// достигается. Files PC изолирован от voice, но низкий порог всё равно полезен
// для receiver-side flow control (ack/request ходят по тому же DC).
const attachSendBufferThreshold = 256 * 1024

// screenSendBufferThreshold — порог backpressure для screen DC.
// Screen — live-поток высокого битрейта (1-3 Mbps); 512 KiB ≈ 1-4 кадра в очереди.
// Меньше — encoder будет блокироваться на send и дропать кадры; больше — задержка
// "до экрана зрителя" растёт нелинейно при сетевом jitter.
const screenSendBufferThreshold = 512 * 1024

// OnP2PMessage — колбэк при получении сообщения через DataChannel
type OnP2PMessage func(fromPubKey string, data []byte)

// OnP2PStateChange — колбэк при изменении состояния P2P соединения
type OnP2PStateChange func(pubKey string, connected bool)

// SendSignal — функция для отправки SDP/ICE через сигнальный сервер
type SendSignal func(msg *Message) error

// OnVoiceFrame — колбэк при получении voice фрейма (binary PCM)
type OnVoiceFrame func(fromPubKey string, pcmData []byte)

// maxPendingICE — потолок буфера отложенных ICE-кандидатов на (peer, kind).
// При Relay-only policy кандидатов 1-2, при All — десятки; 64 хватает с запасом,
// а битый/злонамеренный пир не раздует память.
const maxPendingICE = 64

// pendingICEKey — ключ буфера отложенных кандидатов.
type pendingICEKey struct {
	pubKey string
	kind   pcKind
}

// PeerManager — управляет WebRTC peer connections
type PeerManager struct {
	mu    sync.RWMutex
	peers map[string]*P2PPeer // publicKey → P2PPeer
	log   *slog.Logger

	// pendingICE — кандидаты, пришедшие раньше offer/answer. Trickle ICE не
	// переотправляется: без буфера кандидат, обогнавший SDP (обычное дело —
	// pion начинает gathering сразу после SetLocalDescription), терялся
	// навсегда → пара не собиралась → dtls timeout.
	pendingICE map[pendingICEKey][]webrtc.ICECandidateInit

	// mediaBackoff — троттлинг fullReconnect по пирам: без него цикл
	// disconnected → restart → fail крутится каждые ~10 секунд бесконечно.
	mediaBackoff map[string]*reconnectBackoff

	onMessage          OnP2PMessage
	onStateChange      OnP2PStateChange
	onVoiceFrame       OnVoiceFrame
	onAttachMessage    OnP2PMessage              // бинарные frames из "attachments" DC
	onScreenMessage    OnP2PMessage              // бинарные frames из "screen" DC
	onScreenClosed     func(remotePubKey string) // screen DC закрыт — зритель убирает плитку сразу, не дожидаясь GC
	onScreenAudio      OnP2PMessage              // пакеты звука демонстрации из "screen-audio" DC
	isBlocked          func(string) bool         // заблокированный пир — offer отклоняется (services/blocklist.go)
	onDataChannelReady func(pubKey string)       // вызывается когда DataChannel открыт и готов к отправке
	onAttachDCOpen     func(pubKey string)       // "attachments" DC открыт — можно слать manifest/чанки
	sendSignal         SendSignal
	myPubKey           string
	signKey            ed25519.PrivateKey // ключ профиля: подпись offer/answer (sdpsign.go)
	customICEServers   []webrtc.ICEServer // пользовательские ICE/TURN серверы
}

type reconnectBackoff struct {
	attempts int
	last     time.Time
}

// NewPeerManager создаёт менеджер соединений
func NewPeerManager(log *slog.Logger, myPubKey string, sendSignal SendSignal, onMessage OnP2PMessage, onStateChange OnP2PStateChange) *PeerManager {
	return &PeerManager{
		peers:         make(map[string]*P2PPeer),
		pendingICE:    make(map[pendingICEKey][]webrtc.ICECandidateInit),
		mediaBackoff:  make(map[string]*reconnectBackoff),
		log:           log,
		myPubKey:      myPubKey,
		sendSignal:    sendSignal,
		onMessage:     onMessage,
		onStateChange: onStateChange,
	}
}

// signal отправляет сообщение на сигналку через текущую send-функцию.
// pm.sendSignal подменяется при ReconnectSignal (SetSendFunc) — читать поле
// без лока из pion-колбэков было data race.
func (pm *PeerManager) signal(msg *Message) error {
	pm.mu.RLock()
	fn := pm.sendSignal
	pm.mu.RUnlock()
	if fn == nil {
		return fmt.Errorf("signal client не подключён")
	}
	return fn(msg)
}

// clampKind нормализует pcId из недоверенного входящего сообщения.
// Всё вне [0,2] трактуем как media — так же, как это делает slotState.
func clampKind(pcId int) pcKind {
	if pcId < int(pcKindMedia) || pcId > int(pcKindScreen) {
		return pcKindMedia
	}
	return pcKind(pcId)
}

// bufferPendingICE откладывает кандидата до установки remote description.
func (pm *PeerManager) bufferPendingICE(pubKey string, kind pcKind, cand webrtc.ICECandidateInit) {
	key := pendingICEKey{pubKey, kind}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if len(pm.pendingICE[key]) >= maxPendingICE {
		return
	}
	pm.pendingICE[key] = append(pm.pendingICE[key], cand)
}

// takePendingICE забирает (и очищает) отложенные кандидаты для (peer, kind).
func (pm *PeerManager) takePendingICE(pubKey string, kind pcKind) []webrtc.ICECandidateInit {
	key := pendingICEKey{pubKey, kind}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	cands := pm.pendingICE[key]
	delete(pm.pendingICE, key)
	return cands
}

// clearPendingICELocked удаляет буфер кандидатов слота. Вызывать под pm.mu.
func (pm *PeerManager) clearPendingICELocked(pubKey string, kind pcKind) {
	delete(pm.pendingICE, pendingICEKey{pubKey, kind})
}

// flushPendingICE применяет отложенные кандидаты к pc после того, как
// SetRemoteDescription прошёл. Ошибки отдельных кандидатов не фатальны
// (кандидат от предыдущей ICE-сессии отвергается по ufrag — это норма).
func (pm *PeerManager) flushPendingICE(pubKey string, kind pcKind, pc *webrtc.PeerConnection) {
	cands := pm.takePendingICE(pubKey, kind)
	for _, cand := range cands {
		if err := pc.AddICECandidate(cand); err != nil {
			pm.log.Debug("pending ICE: кандидат отвергнут",
				slog.String("peer", safeShort(pubKey)),
				slog.Int("pcId", int(kind)),
				slog.String("error", err.Error()))
		}
	}
	if len(cands) > 0 {
		pm.log.Info("pending ICE: применены отложенные кандидаты",
			slog.String("peer", safeShort(pubKey)),
			slog.Int("pcId", int(kind)),
			slog.Int("count", len(cands)))
	}
}

// SetICEServers устанавливает пользовательские ICE/TURN серверы
func (pm *PeerManager) SetICEServers(servers []webrtc.ICEServer) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.customICEServers = servers
}

// InitiateConnection создаёт media PeerConnection и отправляет offer
func (pm *PeerManager) InitiateConnection(remotePubKey string) error {
	return pm.initiateConnection(remotePubKey, false, pcKindMedia)
}

// InitiateCallConnection создаёт временное media P2P соединение для звонка (не друг)
func (pm *PeerManager) InitiateCallConnection(remotePubKey string) error {
	return pm.initiateConnection(remotePubKey, true, pcKindMedia)
}

// EnsureFilesPC ленивый подъём files PeerConnection с пиром.
// Идемпотентен: если уже подключён или инициируется — no-op. Не блокируется
// на handshake — просто триггерит инициацию.
func (pm *PeerManager) EnsureFilesPC(remotePubKey string) error {
	pm.mu.RLock()
	peer, ok := pm.peers[remotePubKey]
	pm.mu.RUnlock()
	if ok && peer.FilesPC != nil {
		return nil
	}
	if ok && peer.FilesInitiating {
		return nil
	}
	return pm.initiateConnection(remotePubKey, false, pcKindFiles)
}

// EnsureFilesPCReady инициирует FilesPC если нужно, и блокируется до тех пор
// пока attachments DC не откроется или ctx не отменится.
// Вызывается seeder'ом перед первым SendAttachChunk и receiver'ом перед
// первым CHUNK_REQUEST. Опрос с шагом 50 мс — хэндшейк обычно укладывается
// в 1-2 секунды на P2P direct, до 5 секунд через TURN.
func (pm *PeerManager) EnsureFilesPCReady(ctx context.Context, remotePubKey string) error {
	// ErrSlotBusy — handshake уже идёт (наш offer или чужой answer в работе):
	// не ошибка, просто ждём открытия DC ниже.
	if err := pm.EnsureFilesPC(remotePubKey); err != nil && !errors.Is(err, ErrSlotBusy) {
		return fmt.Errorf("ensure files PC: %w", err)
	}
	for {
		if pm.IsAttachDCOpen(remotePubKey) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("files PC not ready: %w", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (pm *PeerManager) initiateConnection(remotePubKey string, callOnly bool, kind pcKind) error {
	const op = "PeerManager.initiateConnection"

	pm.log.Info("инициирую P2P соединение",
		slog.String("to", safeShort(remotePubKey)),
		slog.Bool("callOnly", callOnly),
		slog.Int("pcId", int(kind)))

	// Помечаем peer как Initiating СРАЗУ — чтобы HandleOffer мог обнаружить collision,
	// даже если offer от другого пира придёт пока мы создаём свой offer.
	// Для каждого kind отдельный флаг — три PC живут независимо.
	pm.mu.Lock()
	if existing, ok := pm.peers[remotePubKey]; ok {
		curPC, curInit := slotState(existing, kind)
		if curPC != nil && !curInit {
			pm.mu.Unlock()
			pm.log.Info("initiateConnection пропущен: PC уже подключён через HandleOffer",
				slog.String("to", safeShort(remotePubKey)),
				slog.Int("pcId", int(kind)))
			return nil
		}
		if curInit {
			pm.mu.Unlock()
			pm.log.Info("initiateConnection пропущен: уже инициируем",
				slog.String("to", safeShort(remotePubKey)),
				slog.Int("pcId", int(kind)))
			return ErrSlotBusy
		}
		if slotAnswering(existing, kind) {
			pm.mu.Unlock()
			pm.log.Info("initiateConnection пропущен: отвечаем на входящий offer",
				slog.String("to", safeShort(remotePubKey)),
				slog.Int("pcId", int(kind)))
			return ErrSlotBusy
		}
	}
	// Создаём peer entry или обновляем флаги существующего
	if existing, ok := pm.peers[remotePubKey]; ok {
		setInitiating(existing, kind, true)
		if kind == pcKindMedia {
			existing.CallOnly = callOnly
		}
	} else {
		newPeer := &P2PPeer{
			PublicKey: remotePubKey,
			CallOnly:  callOnly,
		}
		setInitiating(newPeer, kind, true)
		pm.peers[remotePubKey] = newPeer
	}
	pm.mu.Unlock()

	pc, err := pm.createPeerConnection(remotePubKey, kind)
	if err != nil {
		pm.mu.Lock()
		pm.clearInitiatingLocked(remotePubKey, kind)
		pm.mu.Unlock()
		return fmt.Errorf("%s: %w", op, err)
	}

	// Создаём DataChannels в зависимости от типа PC.
	var (
		dc       *webrtc.DataChannel
		voiceDC  *webrtc.DataChannel
		attachDC *webrtc.DataChannel
		screenDC *webrtc.DataChannel
	)
	switch kind {
	case pcKindMedia:
		dc, err = pc.CreateDataChannel(DataChannelLabel, nil)
		if err != nil {
			pc.Close()
			pm.mu.Lock()
			pm.clearInitiatingLocked(remotePubKey, kind)
			pm.mu.Unlock()
			return fmt.Errorf("%s: create messages dc: %w", op, err)
		}
		pm.setupDataChannel(remotePubKey, dc)

		ordered := false
		maxRetransmits := uint16(0)
		voiceDC, err = pc.CreateDataChannel(VoiceDataChannelLabel, &webrtc.DataChannelInit{
			Ordered:        &ordered,
			MaxRetransmits: &maxRetransmits,
		})
		if err == nil {
			pm.setupVoiceDataChannel(remotePubKey, voiceDC)
		}
	case pcKindFiles:
		attachDC, err = pc.CreateDataChannel(AttachDataChannelLabel, nil)
		if err != nil {
			pc.Close()
			pm.mu.Lock()
			pm.clearInitiatingLocked(remotePubKey, kind)
			pm.mu.Unlock()
			return fmt.Errorf("%s: create attachments dc: %w", op, err)
		}
		pm.setupAttachDataChannel(remotePubKey, attachDC)
	case pcKindScreen:
		// Screen DC: ordered + reliable (nil init). H.264 P-кадры ссылаются на
		// предыдущие; потеря одного чанка ломала бы картинку до следующего
		// keyframe (2 с). Раньше был unordered + maxRetransmits=0 под JPEG,
		// где каждый кадр самодостаточен. Задержку под конгестией держит
		// adaptive-контроллер битрейта по BufferedAmount (см. ScreenShareService),
		// а не транспорт; PLI на keyframe идёт control-фреймом по этому же DC.
		screenDC, err = pc.CreateDataChannel(ScreenDataChannelLabel, nil)
		if err != nil {
			pc.Close()
			pm.mu.Lock()
			pm.clearInitiatingLocked(remotePubKey, kind)
			pm.mu.Unlock()
			return fmt.Errorf("%s: create screen dc: %w", op, err)
		}
		pm.setupScreenDataChannel(remotePubKey, screenDC)
	}

	// Создаём SDP offer
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		pc.Close()
		pm.mu.Lock()
		pm.clearInitiatingLocked(remotePubKey, kind)
		pm.mu.Unlock()
		return fmt.Errorf("%s: create offer: %w", op, err)
	}

	if err := pc.SetLocalDescription(offer); err != nil {
		pc.Close()
		pm.mu.Lock()
		pm.clearInitiatingLocked(remotePubKey, kind)
		pm.mu.Unlock()
		return fmt.Errorf("%s: set local desc: %w", op, err)
	}

	// Обновляем peer — но только если HandleOffer не перехватил collision.
	pm.mu.Lock()
	existing, ok := pm.peers[remotePubKey]
	if !ok {
		pm.mu.Unlock()
		pc.Close()
		return fmt.Errorf("%s: peer entry vanished", op)
	}
	// HandleOffer уже принял чужой offer (сбросил Initiating и/или установил
	// answer-PC) — наш PC больше не нужен. Проверяем и флаг, и сам слот:
	// флаг может быть выставлен заново нашим же вызовом, а слот — занят.
	if curPC, curInit := slotState(existing, kind); !curInit || curPC != nil || slotAnswering(existing, kind) {
		pm.mu.Unlock()
		pc.Close()
		pm.log.Info("initiateConnection отменён: HandleOffer уже принял offer",
			slog.String("to", safeShort(remotePubKey)),
			slog.Int("pcId", int(kind)))
		return nil
	}
	// offerSentAt ставим уже здесь (SetLocalDescription сделан, PC в слоте):
	// между установкой слота и отправкой offer входящий чужой offer должен
	// видеть наш как свежий, а не как «нулевой».
	setOfferSentAt(existing, kind, time.Now())
	switch kind {
	case pcKindMedia:
		existing.PC = pc
		existing.DC = dc
		existing.sendReady = make(chan struct{}, 1)
	case pcKindFiles:
		existing.FilesPC = pc
		existing.AttachDC = attachDC
		if existing.attachSendReady == nil {
			existing.attachSendReady = make(chan struct{}, 1)
		}
	case pcKindScreen:
		existing.ScreenPC = pc
		existing.ScreenDC = screenDC
		if existing.screenSendReady == nil {
			existing.screenSendReady = make(chan struct{}, 1)
		}
	}
	pm.mu.Unlock()

	// Отправляем offer через сигнальный сервер (подписанный — sdpsign.go)
	offerPayload, err := pm.signedSDP(offer, remotePubKey, int(kind))
	if err != nil {
		pm.closePeerKind(remotePubKey, kind)
		return fmt.Errorf("%s: sign offer: %w", op, err)
	}
	if err := pm.signal(&Message{
		Type:    TypeOffer,
		To:      remotePubKey,
		Payload: offerPayload,
		PcId:    int(kind),
	}); err != nil {
		pm.closePeerKind(remotePubKey, kind)
		return fmt.Errorf("%s: send offer: %w", op, err)
	}

	pm.markOfferSent(remotePubKey, kind, pc)
	pm.armConnectWatchdog(remotePubKey, kind, pc, offerAnswerTimeout)
	pm.log.Info("offer отправлен",
		slog.String("to", safeShort(remotePubKey)),
		slog.Int("pcId", int(kind)))
	return nil
}

// clearInitiatingLocked сбрасывает флаг initiating для нужного PC.
// Если в peer entry больше ничего нет (все PC отсутствуют) — удаляет запись.
// Должна вызываться под pm.mu.Lock().
func (pm *PeerManager) clearInitiatingLocked(remotePubKey string, kind pcKind) {
	peer, ok := pm.peers[remotePubKey]
	if !ok {
		return
	}
	switch kind {
	case pcKindMedia:
		peer.Initiating = false
	case pcKindFiles:
		peer.FilesInitiating = false
	case pcKindScreen:
		peer.ScreenInitiating = false
	}
	if peerEmpty(peer) {
		delete(pm.peers, remotePubKey)
	}
}

// peerEmpty — true если все три PC слота свободны и нет инициаций в полёте.
func peerEmpty(peer *P2PPeer) bool {
	return peer.PC == nil && peer.FilesPC == nil && peer.ScreenPC == nil &&
		!peer.Initiating && !peer.FilesInitiating && !peer.ScreenInitiating &&
		!peer.answering && !peer.filesAnswering && !peer.screenAnswering
}

// HandleOffer обрабатывает входящий SDP offer.
// pcId выбирает целевой PC (0=media, 1=files). Collision detection и
// жизненный цикл выполняются изолированно для каждого PC.
// Если top-level pcId == 0, читаем fallback из payload (старый сигналка-сервер
// не пробрасывает поле pc_id, но payload остаётся непрозрачным RawMessage).
func (pm *PeerManager) HandleOffer(fromPubKey string, payload json.RawMessage, pcId int) error {
	const op = "PeerManager.HandleOffer"
	pm.mu.RLock()
	isBlocked := pm.isBlocked
	pm.mu.RUnlock()
	if isBlocked != nil && isBlocked(fromPubKey) {
		return fmt.Errorf("%s: пир заблокирован", op)
	}
	pcId = resolvePcID(pcId, payload)
	kind := clampKind(pcId)

	// Подпись собеседника — до любых решений по слоту: неподписанная смена
	// DTLS-отпечатка иначе выглядела бы как «пир пересоздал PC».
	if _, err := pm.verifySDP(fromPubKey, payload, pcId); err != nil {
		pm.log.Warn("offer отвергнут: подпись SDP",
			slog.String("from", safeShort(fromPubKey)),
			slog.Int("pcId", pcId),
			slog.String("error", err.Error()))
		return fmt.Errorf("%s: %w", op, err)
	}

	// Collision detection: если мы уже инициируем соединение для ЭТОГО же PC
	pm.mu.RLock()
	existing, hasExisting := pm.peers[fromPubKey]
	var curSig string
	if hasExisting {
		if cur, _ := slotState(existing, kind); cur != nil {
			curSig = cur.SignalingState().String()
		}
	}
	pm.mu.RUnlock()

	// sig — состояние нашего слота ДО обработки: без него glare по логам
	// восстанавливается только по секундам.
	pm.log.Info("получен offer",
		slog.String("from", safeShort(fromPubKey)),
		slog.Int("pcId", pcId),
		slog.String("sig", curSig))

	var peerReplacedPC bool
	if hasExisting {
		curPC, curInit := slotState(existing, kind)

		// ICE restart: есть существующий PC (не в фазе initiate). Различаем два
		// случая по DTLS-fingerprint входящего offer (Certificates в конфиге PC
		// не задаём — у каждого PeerConnection свой сертификат):
		//   * совпадает с текущим remote description → пир делает ICE restart на
		//     ТОМ ЖЕ PC — отвечаем на существующем, в любом pcState (restart из
		//     failed — штатный путь восстановления);
		//   * отличается → пир пересоздал PC (fullReconnect / перезапуск). Наш PC
		//     для него мёртв, DTLS с ним уже не поднять — идём в общий путь ниже:
		//     закрыть свой, ответить новым, минуя impolite-ignore.
		if curPC != nil && !curInit {
			pcState := curPC.ConnectionState()
			sigState := curPC.SignalingState()
			sameSession, known := offerSameSession(curPC, payload)
			if sameSession {
				if sigState != webrtc.SignalingStateHaveLocalOffer {
					pm.log.Info("ICE restart: принимаем offer на существующем PC",
						slog.String("from", safeShort(fromPubKey)),
						slog.Int("pcId", pcId),
						slog.String("pcState", pcState.String()),
						slog.String("sigState", sigState.String()))
					return pm.handleICERestartOffer(fromPubKey, curPC, payload, kind)
				}
				// Оба в have-local-offer с restart-offer'ами. pion v4 не умеет
				// rollback из have-local-offer (ни SetLocal, ни SetRemote), а
				// ответить с нового PC нельзя — у пира DTLS установлен на старом.
				// tryICERestart шлёт restart-offer только impolite-сторона, так что
				// сюда попадаем лишь в гонке. Игнорируем: connect-watchdog сбросит
				// слот, reconnect поднимет новый PC.
				pm.log.Warn("ICE restart collision: оба в have-local-offer, игнорируем (rollback в pion недоступен)",
					slog.String("from", safeShort(fromPubKey)),
					slog.Int("pcId", pcId),
					slog.Bool("polite", pm.myPubKey < fromPubKey))
				return nil
			}
			if known {
				peerReplacedPC = true
				pm.log.Info("пир пересоздал PC (fingerprint изменился): отвечаем новым PC",
					slog.String("from", safeShort(fromPubKey)),
					slog.Int("pcId", pcId),
					slog.String("pcState", pcState.String()),
					slog.String("sigState", sigState.String()))
			}
		}

		isCollision := curInit
		if !isCollision && curPC != nil {
			isCollision = curPC.SignalingState() == webrtc.SignalingStateHaveLocalOffer
		}
		// peerReplacedPC: наш offer (в т.ч. restart-offer) адресован PC, которого
		// у пира больше нет — «свежим»/«отвеченным» он быть не может, принимаем.
		if isCollision && !peerReplacedPC {
			polite := pm.myPubKey < fromPubKey
			fresh, age := pm.ourOfferFresh(fromPubKey, kind)
			// «Устаревший» = answer так и не пришёл. Если answer уже применён
			// (sig=stable, ICE в connecting, Initiating ещё true до connected),
			// наш offer не мёртв — impolite ждёт ICE; если тот упадёт,
			// tryICERestart/fullReconnect сделают новый круг.
			answered := curPC != nil && curPC.SignalingState() != webrtc.SignalingStateHaveLocalOffer
			// Impolite игнорирует чужой offer ТОЛЬКО пока свой свежий или уже
			// отвечен: polite на него ответит / ICE идёт. Если answer не пришёл
			// за offerGrace (пир заменил PC, answer ушёл в никуда — glare
			// 2026-09-08) — наш offer мёртв, принимаем входящий; иначе оба
			// висят в have-local-offer навсегда.
			if !polite && (fresh || answered) {
				pm.log.Info("collision: мы impolite, игнорируем входящий offer",
					slog.String("from", safeShort(fromPubKey)),
					slog.Int("pcId", pcId),
					slog.Duration("our_offer_age", age))
				return nil
			}
			if polite {
				pm.log.Info("collision: мы polite, принимаем входящий offer",
					slog.String("from", safeShort(fromPubKey)),
					slog.Int("pcId", pcId))
			} else {
				pm.log.Info("collision: наш offer без answer устарел, принимаем входящий",
					slog.String("from", safeShort(fromPubKey)),
					slog.Int("pcId", pcId),
					slog.Duration("our_offer_age", age))
			}
		}
	}

	// Закрываем старый PC того же типа (атомарно, чтобы избежать race с InitiateConnection).
	// CallOnly наследуется. Второй PC (другого типа) остаётся нетронутым.
	pm.mu.Lock()
	var oldPC *webrtc.PeerConnection
	var oldCallOnly bool
	oldPeer, ok := pm.peers[fromPubKey]
	if !ok {
		// Entry нужен сразу: answering-флаг должен быть виден initiateConnection
		// всё время, пока мы собираем answer-PC без лока.
		oldPeer = &P2PPeer{PublicKey: fromPubKey}
		pm.peers[fromPubKey] = oldPeer
	}
	oldCallOnly = oldPeer.CallOnly
	oldPC, _ = slotState(oldPeer, kind)
	// Сбрасываем слот того же типа — будем заполнять заново.
	clearSlot(oldPeer, kind)
	// Glare: гасим Initiating СРАЗУ. Наш initiateConnection, находящийся
	// между созданием offer и финальной записью слота, проверяет этот флаг
	// и, увидев false, закроет свой PC вместо перезаписи нашего answer-PC.
	// Без этого при collision жили два PC на пира: утёкший слал кандидаты
	// старой сессии → «ufrag doesn't match» → dtls timeout.
	setInitiating(oldPeer, kind, false)
	// answering закрывает второе окно: initiateConnection, стартовавший ПОСЛЕ
	// этой точки, видит пустой слот и Initiating=false — без флага он ставил
	// Initiating заново и перезаписывал наш answer-PC своим offer-PC.
	setAnswering(oldPeer, kind, true)
	pm.mu.Unlock()
	defer func() {
		pm.mu.Lock()
		if p, ok := pm.peers[fromPubKey]; ok {
			setAnswering(p, kind, false)
			if peerEmpty(p) {
				delete(pm.peers, fromPubKey)
			}
		}
		pm.mu.Unlock()
	}()

	if oldPC != nil {
		// Асинхронно: Close() с DTLS/ICE через TURN занимает секунды, а answer
		// пиру нужен сейчас (замер 2026-09-08: answer уходил через ~8 с, ICE у
		// пира успевал упасть). Слот уже очищен под локом, старый PC никому
		// не виден.
		go oldPC.Close()
	}

	pc, err := pm.createPeerConnection(fromPubKey, kind)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}

	// Обработка входящего DataChannel (создаётся инициатором).
	// Тип DC зависит от kind: media — messages+voice, files — attachments,
	// screen — screen.
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		pm.log.Info("получен DataChannel",
			slog.String("label", dc.Label()),
			slog.String("from", safeShort(fromPubKey)),
			slog.Int("pcId", pcId))
		switch dc.Label() {
		case VoiceDataChannelLabel:
			pm.setupVoiceDataChannel(fromPubKey, dc)
			pm.mu.Lock()
			if p, ok := pm.peers[fromPubKey]; ok {
				p.VoiceDC = dc
			}
			pm.mu.Unlock()
		case AttachDataChannelLabel:
			pm.setupAttachDataChannel(fromPubKey, dc)
			pm.mu.Lock()
			if p, ok := pm.peers[fromPubKey]; ok {
				p.AttachDC = dc
				if p.attachSendReady == nil {
					p.attachSendReady = make(chan struct{}, 1)
				}
			}
			pm.mu.Unlock()
		case ScreenDataChannelLabel:
			pm.setupScreenDataChannel(fromPubKey, dc)
			pm.mu.Lock()
			if p, ok := pm.peers[fromPubKey]; ok {
				p.ScreenDC = dc
				if p.screenSendReady == nil {
					p.screenSendReady = make(chan struct{}, 1)
				}
			}
			pm.mu.Unlock()
		case ScreenAudioDataChannelLabel:
			// Без явного case канал ушёл бы в default и затёр DC сообщений.
			pm.setupScreenAudioDataChannel(fromPubKey, dc)
			pm.mu.Lock()
			if p, ok := pm.peers[fromPubKey]; ok {
				p.ScreenAudioDC = dc
			}
			pm.mu.Unlock()
		default:
			pm.setupDataChannel(fromPubKey, dc)
			pm.mu.Lock()
			if p, ok := pm.peers[fromPubKey]; ok {
				p.DC = dc
			}
			pm.mu.Unlock()
		}
	})

	// Парсим и устанавливаем remote description (offer)
	var offer webrtc.SessionDescription
	if err := json.Unmarshal(payload, &offer); err != nil {
		pc.Close()
		return fmt.Errorf("%s: parse offer: %w", op, err)
	}

	if err := pc.SetRemoteDescription(offer); err != nil {
		pc.Close()
		return fmt.Errorf("%s: set remote desc: %w", op, err)
	}

	// Применяем кандидаты, пришедшие раньше этого offer'а.
	pm.flushPendingICE(fromPubKey, kind, pc)

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		pc.Close()
		return fmt.Errorf("%s: create answer: %w", op, err)
	}

	if err := pc.SetLocalDescription(answer); err != nil {
		pc.Close()
		return fmt.Errorf("%s: set local desc: %w", op, err)
	}

	answerPayload, err := pm.signedSDP(answer, fromPubKey, pcId)
	if err != nil {
		pc.Close()
		return fmt.Errorf("%s: sign answer: %w", op, err)
	}
	if err := pm.signal(&Message{
		Type:    TypeAnswer,
		To:      fromPubKey,
		Payload: answerPayload,
		PcId:    pcId,
	}); err != nil {
		pc.Close()
		return fmt.Errorf("%s: send answer: %w", op, err)
	}

	// Сохраняем peer entry. Если уже есть — обновляем нужный слот, не теряя другой.
	pm.mu.Lock()
	peer, ok := pm.peers[fromPubKey]
	if !ok {
		peer = &P2PPeer{
			PublicKey: fromPubKey,
			CallOnly:  oldCallOnly,
		}
		pm.peers[fromPubKey] = peer
	} else {
		peer.CallOnly = oldCallOnly
	}
	// Защита от сироты: если кто-то успел положить в слот другой PC (при
	// answering это невозможно, но дешевле проверить), закрываем его, а не
	// оставляем слать кандидаты чужой сессии.
	var strayPC *webrtc.PeerConnection
	if cur, _ := slotState(peer, kind); cur != nil && cur != pc {
		strayPC = cur
	}
	switch kind {
	case pcKindMedia:
		peer.PC = pc
		peer.sendReady = make(chan struct{}, 1)
	case pcKindFiles:
		peer.FilesPC = pc
		if peer.attachSendReady == nil {
			peer.attachSendReady = make(chan struct{}, 1)
		}
	case pcKindScreen:
		peer.ScreenPC = pc
		if peer.screenSendReady == nil {
			peer.screenSendReady = make(chan struct{}, 1)
		}
	}
	// Мы ответили на чужой offer — инициатором этого слота больше не являемся
	// (см. glare-комментарий выше: initiateConnection увидит false и отступит).
	setInitiating(peer, kind, false)
	setAnswering(peer, kind, false)
	pm.mu.Unlock()
	if strayPC != nil {
		pm.log.Warn("HandleOffer: в слоте оказался чужой PC, закрываем",
			slog.String("from", safeShort(fromPubKey)),
			slog.Int("pcId", pcId))
		_ = strayPC.Close()
	}
	// Answer-PC тоже должен дойти до connected за разумное время (см.
	// armConnectWatchdog): иначе слот с мёртвым PC блокирует initiateConnection
	// («PC уже подключён через HandleOffer»).
	pm.armConnectWatchdog(fromPubKey, kind, pc, connectTimeout)

	pm.log.Info("answer отправлен",
		slog.String("to", safeShort(fromPubKey)),
		slog.Int("pcId", pcId))
	return nil
}

// setInitiating выставляет нужный *Initiating флаг для типа PC.
func setInitiating(peer *P2PPeer, kind pcKind, v bool) {
	switch kind {
	case pcKindMedia:
		peer.Initiating = v
	case pcKindFiles:
		peer.FilesInitiating = v
	case pcKindScreen:
		peer.ScreenInitiating = v
	}
}

// slotState возвращает PC и Initiating-флаг для нужного типа.
func slotState(peer *P2PPeer, kind pcKind) (*webrtc.PeerConnection, bool) {
	switch kind {
	case pcKindFiles:
		return peer.FilesPC, peer.FilesInitiating
	case pcKindScreen:
		return peer.ScreenPC, peer.ScreenInitiating
	default:
		return peer.PC, peer.Initiating
	}
}

// clearSlot обнуляет PC слот нужного типа (чтобы перезаполнить новым PC).
func clearSlot(peer *P2PPeer, kind pcKind) {
	switch kind {
	case pcKindFiles:
		peer.FilesPC = nil
		peer.AttachDC = nil
		peer.FilesConnected = false
	case pcKindScreen:
		peer.ScreenPC = nil
		peer.ScreenDC = nil
		peer.ScreenAudioDC = nil
		peer.ScreenConnected = false
	default:
		peer.PC = nil
		peer.DC = nil
		peer.VoiceDC = nil
		peer.Connected = false
	}
	setOfferSentAt(peer, kind, time.Time{})
	setAnswering(peer, kind, false)
}

// slotAnswering — true пока HandleOffer для этого типа PC ещё не установил
// answer-PC в слот (см. поля answering* в P2PPeer).
func slotAnswering(peer *P2PPeer, kind pcKind) bool {
	switch kind {
	case pcKindFiles:
		return peer.filesAnswering
	case pcKindScreen:
		return peer.screenAnswering
	default:
		return peer.answering
	}
}

func setAnswering(peer *P2PPeer, kind pcKind, v bool) {
	switch kind {
	case pcKindFiles:
		peer.filesAnswering = v
	case pcKindScreen:
		peer.screenAnswering = v
	default:
		peer.answering = v
	}
}

// slotOfferSentAt — когда ушёл наш последний offer для этого типа PC
// (нулевое время = не отправляли / слот сброшен).
func slotOfferSentAt(peer *P2PPeer, kind pcKind) time.Time {
	switch kind {
	case pcKindFiles:
		return peer.filesOfferSentAt
	case pcKindScreen:
		return peer.screenOfferSentAt
	default:
		return peer.offerSentAt
	}
}

func setOfferSentAt(peer *P2PPeer, kind pcKind, t time.Time) {
	switch kind {
	case pcKindFiles:
		peer.filesOfferSentAt = t
	case pcKindScreen:
		peer.screenOfferSentAt = t
	default:
		peer.offerSentAt = t
	}
}

// ourOfferFresh — наш offer для слота ещё ждёт answer в пределах offerGrace.
// Нулевое offerSentAt (мы ещё собираем свой offer, он вот-вот уйдёт)
// считается свежим: polite-пир на него ответит, а если он всё же
// потеряется — слот сбросит watchdog. Считать его устаревшим нельзя: тогда
// обе стороны принимали чужой offer одновременно, оба answer'а приходили на
// PC, которые уже не ждут answer, и оба соединения падали по ICE.
func (pm *PeerManager) ourOfferFresh(remotePubKey string, kind pcKind) (bool, time.Duration) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	peer, ok := pm.peers[remotePubKey]
	if !ok {
		return false, 0
	}
	sent := slotOfferSentAt(peer, kind)
	if sent.IsZero() {
		return true, 0
	}
	age := time.Since(sent)
	return age < offerGrace, age
}

// markOfferSent фиксирует момент отправки offer для слота, если слот всё ещё
// держит именно этот PC (иначе offer относится к уже заменённому PC).
func (pm *PeerManager) markOfferSent(remotePubKey string, kind pcKind, thisPC *webrtc.PeerConnection) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	peer, ok := pm.peers[remotePubKey]
	if !ok {
		return
	}
	if cur, _ := slotState(peer, kind); cur != thisPC {
		return
	}
	setOfferSentAt(peer, kind, time.Now())
}

// armConnectWatchdog — через timeout проверяет, что PC дошёл до connected.
// Если слот всё ещё держит thisPC, а он не connected — сбрасываем слот
// (closePeerKind), для media дополнительно onStateChange(false):
// NetworkService.scheduleReconnect сам решит, когда пробовать снова.
//
// Покрывает три тупика, у которых нет собственного таймаута в pion:
//   - offer без answer (have-local-offer навсегда, Initiating=true);
//   - answer применён, но ICE так и не стартовал (пир не прислал ни одного
//     кандидата, например TURN-аллокация у него не удалась) — pion держит
//     connecting бесконечно, failedTimeout работает только после connected;
//   - ICE restart на failed PC с тем же исходом (видел вживую 14:39:11).
//
// Хранить handle таймера не нужно: все условия проверяются в момент
// срабатывания; уже connected PC не трогаем.
func (pm *PeerManager) armConnectWatchdog(remotePubKey string, kind pcKind, thisPC *webrtc.PeerConnection, timeout time.Duration) {
	time.AfterFunc(timeout, func() {
		pm.mu.RLock()
		peer, ok := pm.peers[remotePubKey]
		var cur *webrtc.PeerConnection
		if ok {
			cur, _ = slotState(peer, kind)
		}
		pm.mu.RUnlock()
		if !ok || cur != thisPC {
			return
		}
		if thisPC.ConnectionState() == webrtc.PeerConnectionStateConnected {
			return
		}
		pm.log.Warn("PC не подключился за таймаут: сбрасываем слот",
			slog.String("peer", safeShort(remotePubKey)),
			slog.Int("pcId", int(kind)),
			slog.String("state", thisPC.ConnectionState().String()),
			slog.String("sig", thisPC.SignalingState().String()),
			slog.Duration("timeout", timeout))
		pm.closePeerKind(remotePubKey, kind)
		if kind == pcKindMedia && pm.onStateChange != nil {
			pm.onStateChange(remotePubKey, false)
		}
	})
}

// HandleAnswer обрабатывает входящий SDP answer.
// pcId выбирает целевой PC (0=media, 1=files). Fallback из payload если
// top-level не задан (см. HandleOffer).
func (pm *PeerManager) HandleAnswer(fromPubKey string, payload json.RawMessage, pcId int) error {
	const op = "PeerManager.HandleAnswer"
	pcId = resolvePcID(pcId, payload)
	kind := clampKind(pcId)
	if _, err := pm.verifySDP(fromPubKey, payload, pcId); err != nil {
		pm.log.Warn("answer отвергнут: подпись SDP",
			slog.String("from", safeShort(fromPubKey)),
			slog.Int("pcId", pcId),
			slog.String("error", err.Error()))
		return fmt.Errorf("%s: %w", op, err)
	}

	pm.mu.RLock()
	peer, ok := pm.peers[fromPubKey]
	pm.mu.RUnlock()

	if !ok {
		return fmt.Errorf("%s: нет peer entry для %s", op, safeShort(fromPubKey))
	}
	pc, _ := slotState(peer, kind)
	if pc == nil {
		return fmt.Errorf("%s: нет PC pcId=%d для %s", op, pcId, safeShort(fromPubKey))
	}

	var answer webrtc.SessionDescription
	if err := json.Unmarshal(payload, &answer); err != nil {
		return fmt.Errorf("%s: parse answer: %w", op, err)
	}

	// Поздний answer на уже заменённый/отвеченный PC (после collision пир мог
	// ответить на наш старый offer): слот не ждёт answer — тихо игнорируем,
	// иначе pion вернёт invalid state transition и это уйдёт Warn'ом наверх.
	if state := pc.SignalingState(); state != webrtc.SignalingStateHaveLocalOffer {
		pm.log.Debug("answer проигнорирован: PC не ждёт answer",
			slog.String("from", safeShort(fromPubKey)),
			slog.Int("pcId", pcId),
			slog.String("sig", state.String()))
		return nil
	}

	if err := pc.SetRemoteDescription(answer); err != nil {
		return fmt.Errorf("%s: set remote desc: %w", op, err)
	}

	// Кандидаты ответчика часто обгоняют его answer — применяем отложенные.
	pm.flushPendingICE(fromPubKey, kind, pc)

	// Answer есть — теперь дело за ICE. Если пир не прислал кандидатов (его
	// TURN-аллокация не удалась), pion висит в connecting бесконечно.
	pm.armConnectWatchdog(fromPubKey, kind, pc, connectTimeout)

	pm.log.Info("answer установлен",
		slog.String("from", safeShort(fromPubKey)),
		slog.Int("pcId", pcId))
	return nil
}

// HandleICE обрабатывает входящий ICE candidate.
// pcId выбирает целевой PC (0=media, 1=files). Fallback из payload если
// top-level не задан (см. HandleOffer).
func (pm *PeerManager) HandleICE(fromPubKey string, payload json.RawMessage, pcId int) error {
	pcId = resolvePcID(pcId, payload)
	kind := clampKind(pcId)

	var candidate webrtc.ICECandidateInit
	if err := json.Unmarshal(payload, &candidate); err != nil {
		return fmt.Errorf("parse ICE candidate: %w", err)
	}

	pm.mu.RLock()
	peer, ok := pm.peers[fromPubKey]
	pm.mu.RUnlock()

	var pc *webrtc.PeerConnection
	if ok {
		pc, _ = slotState(peer, kind)
	}

	// Кандидат обогнал offer/answer (pion начинает gathering сразу после
	// SetLocalDescription, SDP уходит позже) — буферизуем вместо потери.
	// Flush произойдёт после SetRemoteDescription в HandleOffer/HandleAnswer.
	if pc == nil || pc.RemoteDescription() == nil {
		pm.bufferPendingICE(fromPubKey, kind, candidate)
		pm.log.Debug("ICE кандидат отложен (SDP ещё не установлен)",
			slog.String("peer", safeShort(fromPubKey)),
			slog.Int("pcId", pcId))
		return nil
	}

	return pc.AddICECandidate(candidate)
}

// SendData отправляет данные через DataChannel
func (pm *PeerManager) SendData(remotePubKey string, data []byte) error {
	pm.mu.RLock()
	peer, ok := pm.peers[remotePubKey]
	pm.mu.RUnlock()

	if !ok || peer.DC == nil {
		return fmt.Errorf("нет DataChannel для %s", safeShort(remotePubKey))
	}

	if peer.DC.ReadyState() != webrtc.DataChannelStateOpen {
		return fmt.Errorf("DataChannel не открыт (state: %s)", peer.DC.ReadyState().String())
	}

	return peer.DC.Send(data)
}

// SendDataThrottled отправляет данные с backpressure — ждёт если SCTP буфер переполнен.
// Используется для больших данных (voice chunks, файлы), чтобы не блокировать voice DC.
func (pm *PeerManager) SendDataThrottled(remotePubKey string, data []byte) error {
	pm.mu.RLock()
	peer, ok := pm.peers[remotePubKey]
	pm.mu.RUnlock()

	if !ok || peer.DC == nil {
		return fmt.Errorf("нет DataChannel для %s", safeShort(remotePubKey))
	}

	if peer.DC.ReadyState() != webrtc.DataChannelStateOpen {
		return fmt.Errorf("DataChannel не открыт (state: %s)", peer.DC.ReadyState().String())
	}

	// Backpressure: если SCTP буфер переполнен — ждём пока освободится
	if peer.DC.BufferedAmount() > sendBufferThreshold {
		select {
		case <-peer.sendReady:
		case <-time.After(5 * time.Second):
			return fmt.Errorf("send timeout: буфер DataChannel переполнен")
		}
	}

	return peer.DC.Send(data)
}

// IsConnected проверяет есть ли активное P2P соединение с пиром
func (pm *PeerManager) IsConnected(remotePubKey string) bool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	peer, ok := pm.peers[remotePubKey]
	return ok && peer.Connected
}

// HasActivePeer возвращает true если для пира есть активное соединение или инициация
// (подключён, подключается, инициируется, или ICE restart в процессе)
func (pm *PeerManager) HasActivePeer(remotePubKey string) bool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	peer, ok := pm.peers[remotePubKey]
	return ok && (peer.PC != nil || peer.Initiating)
}

// SetOnDataChannelReady устанавливает колбэк при открытии DataChannel.
// Сеттеры колбэков под pm.mu: вызываются после старта соединений, а читаются
// из pion-горутин — без лока это data race.
func (pm *PeerManager) SetOnDataChannelReady(fn func(pubKey string)) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.onDataChannelReady = fn
}

// SetOnVoiceFrame устанавливает колбэк для входящих voice фреймов
func (pm *PeerManager) SetOnVoiceFrame(fn OnVoiceFrame) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.onVoiceFrame = fn
}

// IsVoiceDCOpen возвращает true если Voice DataChannel пира открыт и готов к отправке.
// Используется для фильтрации voice frame отправок без инкремента счётчика ошибок
// в ситуациях когда DC ещё не успел открыться (первая секунда после connected).
func (pm *PeerManager) IsVoiceDCOpen(remotePubKey string) bool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	peer, ok := pm.peers[remotePubKey]
	return ok && peer.VoiceDC != nil && peer.VoiceDC.ReadyState() == webrtc.DataChannelStateOpen
}

// voiceDCMaxBuffered — потолок исходящей SCTP-очереди voice DC. При конгестии
// (TURN/CGNAT) очередь без ограничения растёт неограниченно → секунды задержки
// голоса и рост памяти. Свежий кадр важнее старых: при переполнении дропаем.
// ~40 кадров Opus (20ms) ≈ 0.8с буфера — достаточно чтобы пережить джиттер,
// но не копить задержку.
const voiceDCMaxBuffered = 64 * 1024

// SendVoiceFrame отправляет binary PCM фрейм через voice DataChannel
func (pm *PeerManager) SendVoiceFrame(remotePubKey string, pcmData []byte) error {
	pm.mu.RLock()
	peer, ok := pm.peers[remotePubKey]
	pm.mu.RUnlock()

	if !ok || peer.VoiceDC == nil || peer.VoiceDC.ReadyState() != webrtc.DataChannelStateOpen {
		return fmt.Errorf("no voice DC for %s", safeShort(remotePubKey))
	}
	// Backpressure: очередь переполнена — дропаем кадр (реалтайм-голос, старые
	// кадры бесполезны). Без ограничения задержка накапливалась бы секундами.
	if peer.VoiceDC.BufferedAmount() > voiceDCMaxBuffered {
		return nil
	}
	return peer.VoiceDC.Send(pcmData)
}

// SetVoiceActive marks a peer as participating in voice chat
func (pm *PeerManager) SetVoiceActive(remotePubKey string, active bool) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if peer, ok := pm.peers[remotePubKey]; ok {
		peer.VoiceActive = active
	}
}

// IsVoiceActive checks if peer has active voice
func (pm *PeerManager) IsVoiceActive(remotePubKey string) bool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	if peer, ok := pm.peers[remotePubKey]; ok {
		return peer.VoiceActive
	}
	return false
}

// SetSendFunc обновляет функцию отправки сигнальных сообщений (при reconnect)
func (pm *PeerManager) SetSendFunc(fn SendSignal) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.sendSignal = fn
}

// GetConnectedPeers возвращает список publicKey с активным P2P
func (pm *PeerManager) GetConnectedPeers() []string {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	var keys []string
	for key, peer := range pm.peers {
		if peer.Connected {
			keys = append(keys, key)
		}
	}
	return keys
}

// ClosePeer закрывает соединение с пиром
func (pm *PeerManager) ClosePeer(remotePubKey string) {
	pm.closePeer(remotePubKey)
}

// CloseIfNotConnected закрывает media PeerConnection только если пир НЕ в Connected.
// Files и screen PC оставляются как есть (если живы — продолжают работать).
// Возвращает true если media PC был закрыт (или не существовал).
func (pm *PeerManager) CloseIfNotConnected(remotePubKey string) bool {
	pm.mu.Lock()
	peer, ok := pm.peers[remotePubKey]
	if !ok {
		pm.mu.Unlock()
		return true
	}
	if peer.Connected {
		pm.mu.Unlock()
		return false
	}
	toClose := peer.PC
	peer.PC = nil
	peer.DC = nil
	peer.VoiceDC = nil
	peer.Initiating = false
	if peerEmpty(peer) {
		delete(pm.peers, remotePubKey)
	}
	pm.mu.Unlock()

	if toClose != nil {
		toClose.Close()
		pm.log.Info("stale P2P очищен", slog.String("peer", safeShort(remotePubKey)))
	}
	return true
}

// CloseAll закрывает все соединения (media + files + screen).
func (pm *PeerManager) CloseAll() {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	for key, peer := range pm.peers {
		if peer.PC != nil {
			peer.PC.Close()
		}
		if peer.FilesPC != nil {
			peer.FilesPC.Close()
		}
		if peer.ScreenPC != nil {
			peer.ScreenPC.Close()
		}
		delete(pm.peers, key)
	}
	pm.pendingICE = make(map[pendingICEKey][]webrtc.ICECandidateInit)
	pm.mediaBackoff = make(map[string]*reconnectBackoff)
	pm.log.Info("все P2P соединения закрыты")
}

// CloseCallOnlyPeers закрывает все временные P2P соединения (для звонков с не-друзьями)
func (pm *PeerManager) CloseCallOnlyPeers() {
	pm.mu.Lock()
	var toClose []string
	for key, peer := range pm.peers {
		if peer.CallOnly {
			toClose = append(toClose, key)
		}
	}
	pm.mu.Unlock()
	for _, key := range toClose {
		pm.log.Info("закрываю call-only P2P", slog.String("peer", safeShort(key)))
		pm.closePeer(key)
	}
}

// ClearCallOnly снимает флаг CallOnly (пир стал другом)
func (pm *PeerManager) ClearCallOnly(remotePubKey string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if peer, ok := pm.peers[remotePubKey]; ok {
		peer.CallOnly = false
	}
}

// IsCallOnly проверяет, является ли соединение временным для звонка
func (pm *PeerManager) IsCallOnly(remotePubKey string) bool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	if peer, ok := pm.peers[remotePubKey]; ok {
		return peer.CallOnly
	}
	return false
}

// --- internal ---

func (pm *PeerManager) createPeerConnection(remotePubKey string, kind pcKind) (*webrtc.PeerConnection, error) {
	pm.mu.RLock()
	servers := pm.customICEServers
	pm.mu.RUnlock()
	if len(servers) == 0 {
		servers = iceServers
	}
	// ICETransportPolicy: см. ice_policy_release.go / ice_policy_voxdebug.go.
	// Production — Relay (CGNAT-safe), voxdebug — All (LAN-скорость для двух
	// копий на одной машине). Override --force-relay ниже работает в обоих
	// случаях (в production он по сути no-op).
	policy := iceTransportPolicy()
	// Screen PC использует ТУ ЖЕ policy что и остальные (production — Relay).
	// Раньше тут форсился All «чтобы обойти TURN ради битрейта», но:
	//  1. удалённый пир в production сам Relay-only, поэтому selected pair всё
	//     равно host↔relay — обход не работал (логи 2026-05-02: screen шёл
	//     через TURN узла);
	//  2. All переоткрывал тот самый CGNAT-баг, ради которого voice/files
	//     откатили на Relay (direct-пара → DTLS consent-timeout), т.е. ронял
	//     не только screen, но и грузил аплинк битыми кандидатами.
	// Битрейт теперь под контролем adaptive-контроллера в ScreenShareService,
	// так что узкое горлышко TURN больше не требует «direct любой ценой».
	config := webrtc.Configuration{
		ICEServers:         servers,
		ICETransportPolicy: policy,
	}
	// Dev-флаг --force-relay подменяет policy на Relay: ICE собирает только
	// relay-кандидаты, host/srflx исключаются. Нужно для тестов через VPS,
	// когда оба инстанса на одной машине: иначе ICE срезает через loopback.
	if ForceRelay() {
		config.ICETransportPolicy = webrtc.ICETransportPolicyRelay
	}

	// SettingEngine: keepalive для aggressive NAT + фильтрация виртуальных интерфейсов
	se := webrtc.SettingEngine{}
	// ICE timeouts: media-PC агрессивный (5/8/2) — voice-фреймы летят
	// каждые 20 ms, любая дыра > 5 сек значит реальный разрыв. Files-PC же
	// поднимается лениво и до первого CHUNK_REQUEST DataChannel молчит. На
	// srflx-паре через CGNAT 5-сек дыра без DC-трафика провоцирует SCTP/DTLS
	// timeout — PC рвётся ещё до того, как receiver успел запросить чанки
	// (см. логи 19:02:22-28: manifest sent → 6 сек тишины → dtls timeout).
	if kind == pcKindFiles || kind == pcKindScreen {
		// Screen, как и files: при адаптивном дросселе битрейта (конгестия)
		// отправка кадров может замолкать на несколько секунд — DC idle. С
		// aggressive 5-сек disconnectedTimeout это вызывало бы churn close/reopen
		// screen PC. Relaxed таймауты + keepalive 2s держат ICE-путь живым.
		se.SetICETimeouts(
			20*time.Second, // disconnectedTimeout — переживёт idle до chunk_request
			40*time.Second, // failedTimeout
			2*time.Second,  // keepaliveInterval
		)
	} else {
		se.SetICETimeouts(
			5*time.Second, // disconnectedTimeout
			8*time.Second, // failedTimeout
			2*time.Second, // keepaliveInterval — ICE keepalive каждые 2 сек
		)
	}

	// Dev-bind: --bind-iface форсит ОДИН интерфейс. Whitelist переопределяет
	// дефолтный blacklist-фильтр ниже — host-кандидаты собираются только с
	// указанного NIC, остальные (включая VPN, физические, loopback) игнорируются.
	if forcedIface := DevBindIfaceName(); forcedIface != "" || DevBindIP() != nil {
		forcedIP := DevBindIP()
		se.SetInterfaceFilter(func(name string) bool {
			return forcedIface != "" && strings.EqualFold(name, forcedIface)
		})
		se.SetIPFilter(func(ip net.IP) bool {
			if forcedIP == nil {
				return true
			}
			ip4 := ip.To4()
			fip4 := forcedIP.To4()
			return ip4 != nil && fip4 != nil && ip4.Equal(fip4)
		})
	} else {
		// Фильтруем виртуальные сетевые интерфейсы (Docker, VirtualBox, WSL, VPN-адаптеры).
		// Без фильтра ICE генерирует кандидаты для всех интерфейсов, что вызывает нестабильность:
		// ICE выбирает пару через виртуальный интерфейс → он становится недоступен → disconnect.
		se.SetInterfaceFilter(func(name string) bool {
			lower := strings.ToLower(name)
			// Исключаем известные виртуальные адаптеры
			for _, prefix := range []string{
				"docker", "veth", "br-", // Docker
				"vbox", "virtualbox", // VirtualBox
				"vmware", "vmnet", // VMware
				"vethernet", "wsl", // WSL/Hyper-V
				"tailscale", "utun", "tun", "tap", // VPN
				"nordlynx", "wg", // WireGuard
			} {
				if strings.HasPrefix(lower, prefix) {
					return false
				}
			}
			return true
		})

		// Дополнительно фильтруем по IP — исключаем частные подсети виртуальных сетей
		// и loopback (для production). Loopback нужен только в voxdebug-сборке когда
		// две локальные копии должны найти host-пару — см. allowLoopbackIP().
		allowLoopback := allowLoopbackIP()
		se.SetIPFilter(func(ip net.IP) bool {
			if ip4 := ip.To4(); ip4 != nil {
				if ip4[0] == 172 && ip4[1] >= 17 && ip4[1] <= 31 {
					return false
				}
				if ip4[0] == 192 && ip4[1] == 168 && ip4[2] == 56 {
					return false
				}
				if ip4[0] == 127 {
					return allowLoopback
				}
			}
			return true
		})
	}

	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(config)
	if err != nil {
		return nil, fmt.Errorf("create PeerConnection: %w", err)
	}

	// Отправка ICE candidates через сигнальный сервер.
	// PcId маркирует к какому из двух PC относится кандидат.
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		// Адрес пишем только у relay (наш TURN): host/srflx — IP пользователя.
		addr := "‹скрыт›"
		if c.Typ == webrtc.ICECandidateTypeRelay {
			addr = c.Address
		}
		pm.log.Debug("ICE candidate",
			slog.String("peer", safeShort(remotePubKey)),
			slog.Int("pcId", int(kind)),
			slog.String("type", c.Typ.String()),
			slog.String("addr", addr),
			slog.Int("port", int(c.Port)),
			slog.String("protocol", c.Protocol.String()))
		candidateBytes, _ := json.Marshal(c.ToJSON())
		pm.signal(&Message{
			Type:    TypeICE,
			To:      remotePubKey,
			Payload: injectPcID(candidateBytes, int(kind)),
			PcId:    int(kind),
		})
	})

	// Логируем ICE серверы для отладки
	pm.mu.RLock()
	srvCount := len(pm.customICEServers)
	pm.mu.RUnlock()
	pm.log.Debug("PeerConnection создан",
		slog.String("peer", safeShort(remotePubKey)),
		slog.Int("ice_servers", srvCount))

	// Мониторинг состояния соединения.
	// onStateChange (видимый JS-слою) триггерится только media PC — он представляет
	// общее P2P-состояние пира. Files и screen PC живут независимо: на disconnect/failed
	// просто закрываются, следующая отправка поднимет PC заново.
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		// Guard: игнорируем события от заменённого PeerConnection.
		pm.mu.RLock()
		currentPeer, exists := pm.peers[remotePubKey]
		var isCurrentPC bool
		if exists {
			switch kind {
			case pcKindMedia:
				isCurrentPC = currentPeer.PC == pc
			case pcKindFiles:
				isCurrentPC = currentPeer.FilesPC == pc
			case pcKindScreen:
				isCurrentPC = currentPeer.ScreenPC == pc
			}
		}
		pm.mu.RUnlock()

		if !isCurrentPC {
			pm.log.Debug("P2P состояние (stale PC, игнорируем)",
				slog.String("peer", safeShort(remotePubKey)),
				slog.Int("pcId", int(kind)),
				slog.String("state", state.String()))
			return
		}

		pm.log.Info("P2P состояние",
			slog.String("peer", safeShort(remotePubKey)),
			slog.Int("pcId", int(kind)),
			slog.String("state", state.String()))

		switch state {
		case webrtc.PeerConnectionStateConnected:
			pm.mu.Lock()
			if p, ok := pm.peers[remotePubKey]; ok {
				switch kind {
				case pcKindMedia:
					p.Connected = true
					p.Initiating = false
					// Успешное соединение сбрасывает троттлинг fullReconnect
					delete(pm.mediaBackoff, remotePubKey)
				case pcKindFiles:
					p.FilesConnected = true
					p.FilesInitiating = false
				case pcKindScreen:
					p.ScreenConnected = true
					p.ScreenInitiating = false
				}
			}
			pm.mu.Unlock()
			// Логируем выбранную ICE пару
			if sctp := pc.SCTP(); sctp != nil {
				if transport := sctp.Transport(); transport != nil {
					if iceTransport := transport.ICETransport(); iceTransport != nil {
						pair, _ := iceTransport.GetSelectedCandidatePair()
						if pair != nil {
							pm.log.Info("ICE selected pair",
								slog.String("peer", safeShort(remotePubKey)),
								slog.Int("pcId", int(kind)),
								slog.String("local", fmt.Sprintf("%s %s:%d", pair.Local.Typ, pair.Local.Address, pair.Local.Port)),
								slog.String("remote", fmt.Sprintf("%s %s:%d", pair.Remote.Typ, pair.Remote.Address, pair.Remote.Port)))
						}
					}
				}
			}
			if kind == pcKindMedia && pm.onStateChange != nil {
				pm.onStateChange(remotePubKey, true)
			}

		case webrtc.PeerConnectionStateDisconnected:
			switch kind {
			case pcKindMedia:
				go pm.tryICERestart(remotePubKey)
			case pcKindFiles:
				pm.log.Info("files PC disconnected, закрываем (поднимется при следующей передаче)",
					slog.String("peer", safeShort(remotePubKey)))
				go pm.closeFilesPC(remotePubKey)
			case pcKindScreen:
				pm.log.Info("screen PC disconnected, закрываем",
					slog.String("peer", safeShort(remotePubKey)))
				go pm.closeScreenPC(remotePubKey)
			}

		case webrtc.PeerConnectionStateFailed:
			switch kind {
			case pcKindMedia:
				pm.log.Info("media P2P failed, пробуем ICE restart",
					slog.String("peer", safeShort(remotePubKey)))
				go pm.tryICERestart(remotePubKey)
			case pcKindFiles:
				pm.log.Info("files PC failed, закрываем",
					slog.String("peer", safeShort(remotePubKey)))
				go pm.closeFilesPC(remotePubKey)
			case pcKindScreen:
				pm.log.Info("screen PC failed, закрываем",
					slog.String("peer", safeShort(remotePubKey)))
				go pm.closeScreenPC(remotePubKey)
			}

		case webrtc.PeerConnectionStateClosed:
			pm.mu.Lock()
			if p, ok := pm.peers[remotePubKey]; ok {
				switch kind {
				case pcKindMedia:
					p.Connected = false
				case pcKindFiles:
					p.FilesConnected = false
				case pcKindScreen:
					p.ScreenConnected = false
				}
			}
			pm.mu.Unlock()
			if kind == pcKindMedia && pm.onStateChange != nil {
				pm.onStateChange(remotePubKey, false)
			}
			// Screen: закрываем slot полностью, чтобы следующий start не упёрся в
			// "stale ScreenPC != nil → ICE restart на closed PC". Безопасно благодаря
			// isCurrentPC guard'у в начале callback'а — если HandleOffer уже подменил
			// slot новым PC, мы сюда даже не зайдём.
			if kind == pcKindScreen {
				go pm.closeScreenPC(remotePubKey)
			}
		}
	})

	return pc, nil
}

// handleICERestartOffer обрабатывает ICE restart offer на существующем PeerConnection.
// Вызывается только когда наш signaling state НЕ have-local-offer: pion v4.2
// не реализует rollback (SetLocalDescription с пустым SDP отбрасывает тип
// rollback до state machine, а checkNextSignalingState не имеет перехода
// have-local-offer→stable для него), поэтому «polite принимает чужой offer с
// откатом своего» невозможен. Коллизии restart-offer'ов исключены на уровне
// tryICERestart: restart инициирует только impolite-сторона.
func (pm *PeerManager) handleICERestartOffer(fromPubKey string, pc *webrtc.PeerConnection, payload json.RawMessage, kind pcKind) error {
	var offer webrtc.SessionDescription
	if err := json.Unmarshal(payload, &offer); err != nil {
		return fmt.Errorf("ICE restart: parse offer: %w", err)
	}

	if pc.SignalingState() == webrtc.SignalingStateHaveLocalOffer {
		return fmt.Errorf("ICE restart: PC в have-local-offer, rollback в pion недоступен")
	}

	if err := pc.SetRemoteDescription(offer); err != nil {
		return fmt.Errorf("ICE restart: set remote desc: %w", err)
	}

	// Кандидаты новой ICE-сессии могли прийти раньше restart-offer'а.
	pm.flushPendingICE(fromPubKey, kind, pc)

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("ICE restart: create answer: %w", err)
	}

	if err := pc.SetLocalDescription(answer); err != nil {
		return fmt.Errorf("ICE restart: set local desc: %w", err)
	}

	answerPayload, err := pm.signedSDP(answer, fromPubKey, int(kind))
	if err != nil {
		return fmt.Errorf("ICE restart: sign answer: %w", err)
	}
	if err := pm.signal(&Message{
		Type:    TypeAnswer,
		To:      fromPubKey,
		Payload: answerPayload,
		PcId:    int(kind),
	}); err != nil {
		return fmt.Errorf("ICE restart: send answer: %w", err)
	}

	pm.log.Info("ICE restart: answer отправлен",
		slog.String("to", safeShort(fromPubKey)),
		slog.Int("pcId", int(kind)))
	return nil
}

// sdpFingerprint — значение первого a=fingerprint в SDP (upper-case), пустая
// строка если атрибута нет.
func sdpFingerprint(sdp string) string {
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "a=fingerprint:") {
			continue
		}
		if f := strings.Fields(strings.TrimPrefix(line, "a=fingerprint:")); len(f) >= 2 {
			return strings.ToUpper(f[1])
		}
	}
	return ""
}

// offerSameSession — относится ли входящий offer к DTLS-сессии pc.
// known=false: сравнить не с чем (remote description ещё нет — наш первый
// offer ждёт answer, glare нового соединения) или fingerprint не найден.
// same=true: fingerprint совпал — это ICE restart на том же PC пира.
func offerSameSession(pc *webrtc.PeerConnection, payload json.RawMessage) (same, known bool) {
	rd := pc.RemoteDescription()
	if rd == nil {
		return false, false
	}
	var offer webrtc.SessionDescription
	if err := json.Unmarshal(payload, &offer); err != nil {
		return false, false
	}
	cur, in := sdpFingerprint(rd.SDP), sdpFingerprint(offer.SDP)
	if cur == "" || in == "" {
		return false, false
	}
	return cur == in, true
}

// tryICERestart пытается восстановить соединение через ICE restart без пересоздания PeerConnection
func (pm *PeerManager) tryICERestart(remotePubKey string) {
	pm.mu.Lock()
	peer, ok := pm.peers[remotePubKey]
	if !ok || peer.PC == nil || peer.ICERestarting {
		pm.mu.Unlock()
		return
	}
	peer.ICERestarting = true
	thisPC := peer.PC
	pm.mu.Unlock()

	defer func() {
		pm.mu.Lock()
		if p, ok := pm.peers[remotePubKey]; ok {
			p.ICERestarting = false
		}
		pm.mu.Unlock()
	}()

	// ICE restart требует чтобы signaling state был Stable. Если PC застрял
	// в HaveLocalOffer (предыдущий offer не получил answer и таймаутнул) —
	// CreateOffer/SetLocalDescription падают с InvalidModificationError и мы
	// циклимся каждые 13 сек на одном и том же PC. Делаем полный reconnect.
	if state := thisPC.SignalingState(); state != webrtc.SignalingStateStable {
		pm.log.Info("ICE restart: signaling state не stable, делаем полный reconnect",
			slog.String("peer", safeShort(remotePubKey)),
			slog.String("state", state.String()))
		pm.fullReconnect(remotePubKey, thisPC)
		return
	}

	// Restart-offer шлёт только impolite-сторона (больший pubKey). Когда слали
	// обе, offers пересекались в have-local-offer, а rollback в pion
	// недоступен: polite не мог принять чужой offer, обе стороны висели до
	// watchdog'а — 40 с до «Online» у друга (2026-09-14). Polite ждёт
	// restart-offer; если за politeRestartWait соединение не поднялось —
	// полный reconnect новым PC (пир примет его по изменившемуся fingerprint).
	if pm.myPubKey < remotePubKey {
		pm.log.Info("ICE restart: мы polite, ждём restart-offer от пира",
			slog.String("peer", safeShort(remotePubKey)),
			slog.Duration("wait", politeRestartWait))
		time.AfterFunc(politeRestartWait, func() {
			pm.mu.RLock()
			p, ok := pm.peers[remotePubKey]
			same := ok && p.PC == thisPC
			pm.mu.RUnlock()
			if !same || thisPC.ConnectionState() == webrtc.PeerConnectionStateConnected {
				return
			}
			pm.log.Info("ICE restart: пир не восстановил соединение, полный reconnect",
				slog.String("peer", safeShort(remotePubKey)),
				slog.String("state", thisPC.ConnectionState().String()))
			pm.fullReconnect(remotePubKey, thisPC)
		})
		return
	}

	pm.log.Info("ICE restart: попытка восстановления",
		slog.String("peer", safeShort(remotePubKey)))

	// Только thisPC: поле peer.PC может быть конкурентно заменено HandleOffer
	// или fullReconnect — restart-offer от нового PC под видом старого давал
	// кандидаты чужой сессии («ufrag doesn't match»).
	offer, err := thisPC.CreateOffer(&webrtc.OfferOptions{ICERestart: true})
	if err != nil {
		pm.log.Warn("ICE restart: ошибка CreateOffer",
			slog.String("peer", safeShort(remotePubKey)),
			slog.String("error", err.Error()))
		// Fallback: сообщаем о disconnect для полного reconnect (только если PC не заменён)
		pm.mu.Lock()
		if p, ok := pm.peers[remotePubKey]; ok && p.PC == thisPC {
			p.Connected = false
			pm.mu.Unlock()
			if pm.onStateChange != nil {
				pm.onStateChange(remotePubKey, false)
			}
		} else {
			pm.mu.Unlock()
		}
		return
	}

	if err := thisPC.SetLocalDescription(offer); err != nil {
		pm.log.Warn("ICE restart: ошибка SetLocalDescription, fallback на full reconnect",
			slog.String("peer", safeShort(remotePubKey)),
			slog.String("error", err.Error()))
		pm.fullReconnect(remotePubKey, thisPC)
		return
	}

	restartPayload, signErr := pm.signedSDP(offer, remotePubKey, int(pcKindMedia))
	if signErr != nil {
		pm.log.Warn("ICE restart: не удалось подписать offer", slog.String("error", signErr.Error()))
		pm.fullReconnect(remotePubKey, thisPC)
		return
	}
	if err := pm.signal(&Message{
		Type:    TypeOffer,
		To:      remotePubKey,
		Payload: restartPayload,
		PcId:    int(pcKindMedia), // ICE restart применяется только к media PC
	}); err != nil {
		pm.log.Warn("ICE restart: ошибка отправки offer",
			slog.String("peer", safeShort(remotePubKey)),
			slog.String("error", err.Error()))
		// PC остался в have-local-offer без offer в полёте — watchdog ниже
		// сбросит слот, если ICE сам не восстановится.
	} else {
		pm.log.Info("ICE restart: offer отправлен",
			slog.String("peer", safeShort(remotePubKey)))
	}
	pm.markOfferSent(remotePubKey, pcKindMedia, thisPC)
	pm.armConnectWatchdog(remotePubKey, pcKindMedia, thisPC, offerAnswerTimeout)
}

func (pm *PeerManager) setupDataChannel(remotePubKey string, dc *webrtc.DataChannel) {
	// Backpressure: сигнал когда SCTP буфер освободился (< 1MB).
	// Согласовано с sendBufferThreshold (2MB) — даём ~3-4 крупным
	// attachment-chunk'ам (256KB) быть в полёте до возобновления отправки.
	dc.SetBufferedAmountLowThreshold(1024 * 1024)
	dc.OnBufferedAmountLow(func() {
		pm.mu.RLock()
		peer, ok := pm.peers[remotePubKey]
		pm.mu.RUnlock()
		if ok && peer.sendReady != nil {
			select {
			case peer.sendReady <- struct{}{}:
			default:
			}
		}
	})

	dc.OnOpen(func() {
		pm.log.Info("DataChannel открыт",
			slog.String("peer", safeShort(remotePubKey)),
			slog.String("label", dc.Label()))
		pm.mu.RLock()
		ready := pm.onDataChannelReady
		pm.mu.RUnlock()
		if ready != nil {
			go ready(remotePubKey)
		}
	})

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if pm.onMessage != nil {
			pm.onMessage(remotePubKey, msg.Data)
		}
	})

	dc.OnClose(func() {
		pm.log.Info("DataChannel закрыт", slog.String("peer", safeShort(remotePubKey)))
	})

	dc.OnError(func(err error) {
		pm.log.Warn("DataChannel ошибка",
			slog.String("peer", safeShort(remotePubKey)),
			slog.String("error", err.Error()))
	})
}

func (pm *PeerManager) setupVoiceDataChannel(remotePubKey string, dc *webrtc.DataChannel) {
	dc.OnOpen(func() {
		pm.log.Info("Voice DataChannel открыт", slog.String("peer", safeShort(remotePubKey)))
		pm.mu.Lock()
		if p, ok := pm.peers[remotePubKey]; ok {
			p.VoiceDC = dc
		}
		pm.mu.Unlock()
	})

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		pm.mu.RLock()
		fn := pm.onVoiceFrame
		pm.mu.RUnlock()
		if fn != nil {
			fn(remotePubKey, msg.Data)
		}
	})

	dc.OnClose(func() {
		pm.log.Info("Voice DataChannel закрыт", slog.String("peer", safeShort(remotePubKey)))
	})

	dc.OnError(func(err error) {
		pm.log.Debug("Voice DataChannel ошибка",
			slog.String("peer", safeShort(remotePubKey)),
			slog.String("error", err.Error()))
	})
}

// setupAttachDataChannel — handlers для бинарного канала чанков attachment'ов.
// Backpressure threshold 256 KiB + low watermark 64 KiB. Намеренно низкие
// чтобы bulk-transfer не забивал SCTP буфер и текстовые сообщения через
// общий SCTP association доходили быстро (см. attachSendBufferThreshold).
func (pm *PeerManager) setupAttachDataChannel(remotePubKey string, dc *webrtc.DataChannel) {
	dc.SetBufferedAmountLowThreshold(64 * 1024) // 64 KiB low watermark
	dc.OnBufferedAmountLow(func() {
		pm.mu.RLock()
		peer, ok := pm.peers[remotePubKey]
		pm.mu.RUnlock()
		if ok && peer.attachSendReady != nil {
			select {
			case peer.attachSendReady <- struct{}{}:
			default:
			}
		}
	})

	dc.OnOpen(func() {
		pm.log.Info("Attach DataChannel открыт", slog.String("peer", safeShort(remotePubKey)))
		pm.mu.RLock()
		fn := pm.onAttachDCOpen
		pm.mu.RUnlock()
		if fn != nil {
			// В горутине: callback ведёт к доставке pending-сообщений и
			// отправке manifest'ов, pion-колбэк блокировать нельзя.
			go fn(remotePubKey)
		}
	})

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		pm.mu.RLock()
		fn := pm.onAttachMessage
		pm.mu.RUnlock()
		if fn != nil {
			fn(remotePubKey, msg.Data)
		}
	})

	dc.OnClose(func() {
		pm.log.Info("Attach DataChannel закрыт", slog.String("peer", safeShort(remotePubKey)))
	})

	dc.OnError(func(err error) {
		pm.log.Debug("Attach DataChannel ошибка",
			slog.String("peer", safeShort(remotePubKey)),
			slog.String("error", err.Error()))
	})
}

// SendAttachChunk отправляет бинарный фрейм через attachments DC с backpressure.
// Возвращает ту же "send timeout" ошибку что SendDataThrottled — caller-логика
// различения throttle vs fatal не меняется.
func (pm *PeerManager) SendAttachChunk(remotePubKey string, data []byte) error {
	pm.mu.RLock()
	peer, ok := pm.peers[remotePubKey]
	pm.mu.RUnlock()

	if !ok || peer.AttachDC == nil {
		return fmt.Errorf("нет Attach DataChannel для %s", safeShort(remotePubKey))
	}
	if peer.AttachDC.ReadyState() != webrtc.DataChannelStateOpen {
		return fmt.Errorf("Attach DataChannel не открыт (state: %s)", peer.AttachDC.ReadyState().String())
	}

	if peer.AttachDC.BufferedAmount() > attachSendBufferThreshold {
		select {
		case <-peer.attachSendReady:
		case <-time.After(5 * time.Second):
			return fmt.Errorf("send timeout: буфер Attach DataChannel переполнен")
		}
	}

	return peer.AttachDC.Send(data)
}

// IsAttachDCOpen — true если канал чанков готов отправлять. Используется
// sender'ом в attachments_p2p.go для выбора между бинарным путём и (legacy)
// fallback'ом в "messages".
func (pm *PeerManager) IsAttachDCOpen(remotePubKey string) bool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	peer, ok := pm.peers[remotePubKey]
	return ok && peer.AttachDC != nil && peer.AttachDC.ReadyState() == webrtc.DataChannelStateOpen
}

// SetOnAttachMessage устанавливает callback входящих бинарных attachment-фреймов.
func (pm *PeerManager) SetOnAttachMessage(fn OnP2PMessage) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.onAttachMessage = fn
}

// SetOnAttachDCOpen — callback открытия "attachments" DC с пиром (любая
// сторона: creator или answerer). NetworkService по нему доставляет pending
// вложения: manifest, не ушедший из-за «files PC not ready», раньше ждал
// следующего медиа-реконнекта.
func (pm *PeerManager) SetOnAttachDCOpen(fn func(pubKey string)) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.onAttachDCOpen = fn
}

// AttachBufferedAmount — байты в SCTP-буфере "attachments" DC пира (0 если
// канала нет). Для статистики передачи файлов.
func (pm *PeerManager) AttachBufferedAmount(remotePubKey string) uint64 {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	peer, ok := pm.peers[remotePubKey]
	if !ok || peer.AttachDC == nil {
		return 0
	}
	return peer.AttachDC.BufferedAmount()
}

// SetOnScreenMessage устанавливает callback входящих бинарных screen-фреймов.
func (pm *PeerManager) SetOnScreenMessage(fn OnP2PMessage) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.onScreenMessage = fn
}

// SetOnScreenClosed устанавливает callback закрытия screen DC пира (отправитель
// остановил демонстрацию / PC умер). Нужен зрителю, чтобы не держать
// замороженный последний кадр до таймаута.
// SetIsBlocked — проверка «пир заблокирован» для входящих offer.
func (pm *PeerManager) SetIsBlocked(fn func(string) bool) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.isBlocked = fn
}

func (pm *PeerManager) SetOnScreenClosed(fn func(remotePubKey string)) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.onScreenClosed = fn
}

// EnsureScreenPC ленивый подъём screen PeerConnection с пиром (для отправителя).
// Идемпотентен. Зрителю screen PC создаётся через HandleOffer.
func (pm *PeerManager) EnsureScreenPC(remotePubKey string) error {
	pm.mu.RLock()
	peer, ok := pm.peers[remotePubKey]
	pm.mu.RUnlock()
	if ok && peer.ScreenPC != nil {
		return nil
	}
	if ok && peer.ScreenInitiating {
		return nil
	}
	return pm.initiateConnection(remotePubKey, false, pcKindScreen)
}

// EnsureScreenPCReady — блокируется до открытия screen DC или ctx.Done.
func (pm *PeerManager) EnsureScreenPCReady(ctx context.Context, remotePubKey string) error {
	if err := pm.EnsureScreenPC(remotePubKey); err != nil && !errors.Is(err, ErrSlotBusy) {
		return fmt.Errorf("ensure screen PC: %w", err)
	}
	for {
		if pm.IsScreenDCOpen(remotePubKey) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("screen PC not ready: %w", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// IsScreenDCOpen — true если канал screen готов отправлять.
func (pm *PeerManager) IsScreenDCOpen(remotePubKey string) bool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	peer, ok := pm.peers[remotePubKey]
	return ok && peer.ScreenDC != nil && peer.ScreenDC.ReadyState() == webrtc.DataChannelStateOpen
}

// ScreenDCSaturation возвращает максимальный BufferedAmount среди ScreenDC всех
// пиров (в байтах). Используется ScreenShareService как proactive rate-control:
// если SCTP буфер заполнен > screenSendBufferThreshold уже на момент проверки,
// нет смысла запускать новый capture+encode — кадр всё равно встанет в backpressure
// и устареет к моменту реальной отправки. Лучше пропустить, дать буферу разгрузиться,
// захватить следующий кадр уже свежим.
//
// Возвращает 0 если ни у кого нет открытого ScreenDC.
func (pm *PeerManager) ScreenDCSaturation() uint64 {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	var maxBuf uint64
	for _, peer := range pm.peers {
		if peer.ScreenDC == nil || peer.ScreenDC.ReadyState() != webrtc.DataChannelStateOpen {
			continue
		}
		if b := peer.ScreenDC.BufferedAmount(); b > maxBuf {
			maxBuf = b
		}
	}
	return maxBuf
}

// SendScreenChunk отправляет бинарный screen-фрейм через screen DC с backpressure.
// Возвращает "send timeout" если SCTP буфер не освободился за 5 секунд (та же
// семантика что у SendAttachChunk — caller-логика различения throttle vs fatal
// не меняется).
func (pm *PeerManager) SendScreenChunk(remotePubKey string, data []byte) error {
	pm.mu.RLock()
	peer, ok := pm.peers[remotePubKey]
	pm.mu.RUnlock()

	if !ok || peer.ScreenDC == nil {
		return fmt.Errorf("нет Screen DataChannel для %s", safeShort(remotePubKey))
	}
	if peer.ScreenDC.ReadyState() != webrtc.DataChannelStateOpen {
		return fmt.Errorf("Screen DataChannel не открыт (state: %s)", peer.ScreenDC.ReadyState().String())
	}
	if peer.ScreenDC.BufferedAmount() > screenSendBufferThreshold {
		// 500 ms (а не 5s как у attachments): screen — низколатентный поток.
		// Если буфер не освободился за полсекунды — этот кадр уже устарел, лучше
		// дропнуть и пускать следующий, чем держать stale chunks в очереди и
		// растягивать end-to-end задержку. fanout-loop break'нет на err и
		// перейдёт к следующему пиру.
		select {
		case <-peer.screenSendReady:
		case <-time.After(500 * time.Millisecond):
			return fmt.Errorf("send timeout: буфер Screen DataChannel переполнен")
		}
	}
	return peer.ScreenDC.Send(data)
}

// setupScreenDataChannel — handlers для бинарного канала screen-фреймов.
// Backpressure: 512 KiB порог + 128 KiB low watermark. Выше чем у attachments
// потому что screen — низколатентный поток I/P-кадров; маленький буфер заставлял
// бы отправителя дропать фреймы при любом jitter'е сети.
func (pm *PeerManager) setupScreenDataChannel(remotePubKey string, dc *webrtc.DataChannel) {
	dc.SetBufferedAmountLowThreshold(128 * 1024)
	dc.OnBufferedAmountLow(func() {
		pm.mu.RLock()
		peer, ok := pm.peers[remotePubKey]
		pm.mu.RUnlock()
		if ok && peer.screenSendReady != nil {
			select {
			case peer.screenSendReady <- struct{}{}:
			default:
			}
		}
	})

	dc.OnOpen(func() {
		pm.log.Info("Screen DataChannel открыт", slog.String("peer", safeShort(remotePubKey)))
	})

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		pm.mu.RLock()
		fn := pm.onScreenMessage
		pm.mu.RUnlock()
		if fn != nil {
			fn(remotePubKey, msg.Data)
		}
	})

	dc.OnClose(func() {
		pm.log.Info("Screen DataChannel закрыт", slog.String("peer", safeShort(remotePubKey)))
		pm.mu.RLock()
		closed := pm.onScreenClosed
		pm.mu.RUnlock()
		if closed != nil {
			closed(remotePubKey)
		}
		// НЕ closeScreenPC отсюда — race с HandleOffer на быстрый restart:
		// 1. user останавливает screen, DC closes
		// 2. user сразу нажимает Share screen снова
		// 3. sender HandleOffer ставит новый peer.ScreenPC = pcNew
		// 4. наш closeScreenPC goroutine просыпается и nil'ит pcNew → новый
		//    DC живёт но не достижим, "загружается-выключается-загружается"
		// Чистка слота сделана через PC.OnConnectionStateChange Closed branch
		// в createPeerConnection — там есть guard `isCurrentPC == pc` который
		// защищает от race.
	})

	dc.OnError(func(err error) {
		pm.log.Debug("Screen DataChannel ошибка",
			slog.String("peer", safeShort(remotePubKey)),
			slog.String("error", err.Error()))
	})
}

func (pm *PeerManager) closePeer(remotePubKey string) {
	pm.mu.Lock()
	peer, ok := pm.peers[remotePubKey]
	if ok {
		delete(pm.peers, remotePubKey)
	}
	for kind := pcKindMedia; kind <= pcKindScreen; kind++ {
		pm.clearPendingICELocked(remotePubKey, kind)
	}
	delete(pm.mediaBackoff, remotePubKey)
	pm.mu.Unlock()

	if !ok {
		return
	}
	if peer.PC != nil {
		pm.log.Info("P2P media соединение закрыто", slog.String("peer", safeShort(remotePubKey)))
		peer.PC.Close()
	}
	if peer.FilesPC != nil {
		pm.log.Info("P2P files соединение закрыто", slog.String("peer", safeShort(remotePubKey)))
		peer.FilesPC.Close()
	}
	if peer.ScreenPC != nil {
		pm.log.Info("P2P screen соединение закрыто", slog.String("peer", safeShort(remotePubKey)))
		peer.ScreenPC.Close()
	}
}

// closePeerKind закрывает только один из трёх PC (по типу).
// Если другие PC ещё живы — peer entry сохраняется. Иначе удаляется.
func (pm *PeerManager) closePeerKind(remotePubKey string, kind pcKind) {
	pm.mu.Lock()
	peer, ok := pm.peers[remotePubKey]
	if !ok {
		pm.mu.Unlock()
		return
	}
	var toClose *webrtc.PeerConnection
	switch kind {
	case pcKindMedia:
		toClose = peer.PC
		peer.PC = nil
		peer.DC = nil
		peer.VoiceDC = nil
		peer.Connected = false
		peer.Initiating = false
	case pcKindFiles:
		toClose = peer.FilesPC
		peer.FilesPC = nil
		peer.AttachDC = nil
		peer.FilesConnected = false
		peer.FilesInitiating = false
	case pcKindScreen:
		toClose = peer.ScreenPC
		peer.ScreenPC = nil
		peer.ScreenDC = nil
		peer.ScreenAudioDC = nil
		peer.ScreenConnected = false
		peer.ScreenInitiating = false
	}
	pm.clearPendingICELocked(remotePubKey, kind)
	if peerEmpty(peer) {
		delete(pm.peers, remotePubKey)
	}
	pm.mu.Unlock()

	if toClose != nil {
		_ = toClose.Close()
	}
}

// closeFilesPC — alias для удобства из OnConnectionStateChange callback'а.
func (pm *PeerManager) closeFilesPC(remotePubKey string) {
	pm.closePeerKind(remotePubKey, pcKindFiles)
}

// closeScreenPC — alias для удобства из OnConnectionStateChange callback'а.
func (pm *PeerManager) closeScreenPC(remotePubKey string) {
	pm.closePeerKind(remotePubKey, pcKindScreen)
}

// CloseScreenPC — публичный wrapper для остановки screen PC из ScreenShareService.
func (pm *PeerManager) CloseScreenPC(remotePubKey string) {
	pm.closePeerKind(remotePubKey, pcKindScreen)
}

// fullReconnect закрывает текущий PC и инициирует новое соединение с пиром.
// Используется когда ICE restart невозможен (PC застрял в нестабильном signaling state).
// Защита от race: если peer.PC уже не равен expectedPC — значит другая горутина уже
// заменила соединение, ничего не делаем.
func (pm *PeerManager) fullReconnect(remotePubKey string, expectedPC *webrtc.PeerConnection) {
	pm.mu.Lock()
	p, ok := pm.peers[remotePubKey]
	if !ok || p.PC != expectedPC {
		pm.mu.Unlock()
		return
	}
	// Чистим ТОЛЬКО media-слот. Раньше удалялся весь peer entry — живые
	// FilesPC/ScreenPC становились недостижимыми: не закрывались никогда,
	// утекали TURN-аллокации, а их ICE-трафик сыпался в новые entry
	// («ufrag doesn't match»).
	clearSlot(p, pcKindMedia)
	p.Initiating = false
	pm.clearPendingICELocked(remotePubKey, pcKindMedia)
	if peerEmpty(p) {
		delete(pm.peers, remotePubKey)
	}

	// Троттлинг: цикл disconnected → restart → failed без backoff крутился
	// каждые ~10 сек бесконечно. При превышении частоты не инициируем сами —
	// NetworkService.scheduleReconnect ведёт свой backoff-план поверх.
	bo := pm.mediaBackoff[remotePubKey]
	if bo == nil {
		bo = &reconnectBackoff{}
		pm.mediaBackoff[remotePubKey] = bo
	}
	shift := bo.attempts
	if shift > 5 {
		shift = 5
	}
	minInterval := time.Duration(1<<shift) * 5 * time.Second // 5s → 160s
	throttled := !bo.last.IsZero() && time.Since(bo.last) < minInterval
	if !throttled {
		bo.attempts++
		bo.last = time.Now()
	}
	pm.mu.Unlock()

	// Закрываем старый PC за пределами lock (Close может блокироваться)
	_ = expectedPC.Close()

	if pm.onStateChange != nil {
		pm.onStateChange(remotePubKey, false)
	}

	if throttled {
		pm.log.Info("full reconnect: троттлинг, пропускаем попытку",
			slog.String("peer", safeShort(remotePubKey)),
			slog.Duration("min_interval", minInterval))
		return
	}

	pm.log.Info("full reconnect: пересоздаём P2P",
		slog.String("peer", safeShort(remotePubKey)))
	if err := pm.initiateConnection(remotePubKey, false, pcKindMedia); err != nil {
		if errors.Is(err, ErrSlotBusy) {
			pm.log.Debug("full reconnect: слот занят, handshake уже идёт",
				slog.String("peer", safeShort(remotePubKey)))
			return
		}
		pm.log.Warn("full reconnect: initiateConnection failed",
			slog.String("peer", safeShort(remotePubKey)),
			slog.String("error", err.Error()))
	}
}

// safeShort возвращает первые 16 символов строки (безопасно для коротких строк)
func safeShort(s string) string {
	if len(s) > 16 {
		return s[:16]
	}
	return s
}
