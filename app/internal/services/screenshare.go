package services

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"log/slog"
	"net/url"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"vox_app/internal/codec/h264enc"
	"vox_app/internal/session"
	"vox_app/internal/storage"
)

// DisplayInfo — описание физического экрана для UI выбора в "Share Screen" диалоге.
type DisplayInfo struct {
	Index  int    `json:"index"`
	Name   string `json:"name"` // например "Display 1 (1920×1080)"
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// ScreenShareConfig — конфиг текущего сеанса демонстрации экрана.
type ScreenShareConfig struct {
	DisplayIdx int    // 0..N-1 индекс экрана
	Mode       string // "text" | "balanced" | "motion" (см. screenModes)
	Width      int    // целевая ширина кадра (даунскейл с сохранением пропорций)
	Height     int    // целевая высота (для 16:9; фактическая — от пропорций экрана)
	TargetFPS  int
	Bitrate    int // стартовый битрейт H.264, бит/с
	BitrateMin int
	BitrateMax int
	WithAudio  bool // звук системы (фаза 5e, screenshare_audio.go)
}

// screenMode — пресет режима демонстрации. Битрейты подобраны под TURN-путь
// (~2.5–4 Мбит/с до ноутбука по Wi-Fi, замер 2026-09-08): 720p30 в него
// влезает с запасом, 1080p60 — только на хорошем канале, контроллер сам
// опустится к BitrateMin.
type screenMode struct {
	Width, Height int
	FPS           int
	Bitrate       int
	BitrateMin    int
	BitrateMax    int
}

// Пресеты по смыслу (макет Dialogs, фаза 5b): «Текст» — резкий текст и код
// ценой плавности, «Сбалансированно» — по умолчанию, «Движение» — видео и игры.
const screenModeDefault = "balanced"

var screenModes = map[string]screenMode{
	"text":     {1920, 1080, 15, 3_000_000, 600_000, 5_000_000},
	"balanced": {1920, 1080, 30, 4_000_000, 800_000, 8_000_000},
	"motion":   {1280, 720, 60, 5_000_000, 1_000_000, 8_000_000},
}

// legacyScreenModes — названия режимов до фазы 5b: сохранённый выбор фронта
// и вызовы старых сборок сопоставляются новым пресетам.
var legacyScreenModes = map[string]string{
	"720p30":  "balanced",
	"1080p30": "balanced",
	"1080p60": "motion",
}

func screenCodecName(c uint8) string {
	if c == screenCodecH264 {
		return "h264"
	}
	return "jpeg"
}

// ScreenShareService управляет захватом экрана и фан-аутом закодированных
// кадров на screen DC всех участников активной группы.
//
// Архитектура:
//
//	captureLoop (1 goroutine) → encode → encodeScreenChunks
//	  → fan-out: для каждого участника группы — SendScreenChunk
//
// Receive: NetworkService.SetOnScreenMessage → s.handleScreenChunk →
// сборка целого кадра в reassembly buffer → emit "vox:screen-frame" в JS.
//
// Multi-sender в одной группе: каждый шарящий имеет свой ScreenPC с пирами.
// Receiver различает sender'ов по peerID (приходит от network) — кадры разных
// senders собираются в independent reassembly buffer'ы (key = peerID).
type ScreenShareService struct {
	log     *slog.Logger
	store   *storage.Storage
	sess    *session.Session
	network *NetworkService
	groups  *GroupsService
	voice   *VoiceService

	mu sync.Mutex

	// --- sender state ---
	active        bool
	activeGroupID string
	cfg           ScreenShareConfig
	stopCh        chan struct{}
	captureWG     sync.WaitGroup
	frameCounter  uint32

	// --- send mailbox (latest-frame-wins) ---
	// captureLoop кодирует кадр и кладёт его сюда, перезаписывая ещё не
	// отправленный предыдущий. senderLoop забирает ВСЕГДА самый свежий и
	// фанаутит его СЕРИЙНО (один кадр на проводе за раз). Это убирает две
	// болезни старого pipeline=2: (1) interleaving чанков двух кадров на одном
	// DC, из-за которого ресивер (держит 1 кадр на пира) сбрасывал буфер;
	// (2) накопление устаревших кадров в очереди при сетевом jitter.
	txMu         sync.Mutex
	pendingFrame *encodedScreenFrame
	pendingSig   chan struct{}

	// --- adaptive bitrate state (для статов, пишется captureLoop, читается statsLoop) ---
	curBitrate int32        // atomic: текущий целевой битрейт, kbps
	curFPS     int32        // atomic: текущий target fps
	curCodec   int32        // atomic: screenCodecH264 | screenCodecJPEG (fallback)
	curCapture atomic.Value // string: dxgi | bitblt — для screen share stats

	// pliPending — зритель попросил keyframe (control PLI по screen DC);
	// captureLoop снимает флаг и форсит IDR не чаще screenKeyframeMinInterval.
	pliPending atomic.Bool

	// ensureAt — троттлинг ленивого подъёма screen PC по пирам без открытого DC.
	// Доступ только из senderLoop, без локов.
	ensureAt map[string]time.Time

	// --- окна демонстрации (frontend/screen-view.html) ---
	// Окно на пира (макет «Несколько демонстраций»: окон может быть
	// несколько): демонстрация крупно, поверх всех окон. Окна создаёт фабрика
	// из main.go; страница окна получает кадры тем же vox:screen-frame
	// (app.Event.Emit идёт во все окна) и декодирует только своего пира.
	// Команды из окна (mute/leave/...) — в главное окно через ViewerCommand →
	// vox:screen-viewer-cmd: владелец состояния голоса остаётся один
	// (VoiceState главного окна).
	viewerMu      sync.Mutex
	viewers       map[string]*viewerEntry
	viewerGen     int
	viewerFactory func(peerID, title, query string, slot int, onClosed func()) ViewerWindow

	// --- живые превью экранов для диалога выбора (screenshare_thumbs.go) ---
	thumbMu    sync.Mutex
	thumbStop  chan struct{}
	thumbDone  chan struct{}
	thumbUntil time.Time

	// --- звук системы при демонстрации (screenshare_audio.go) ---
	audioSupportOnce sync.Once
	audioSupported   bool
	audioRxMu        sync.Mutex
	audioRx          map[string]*screenAudioRx // приём: по показывающему
	shareVolumes     map[string]float32        // громкость демонстрации пира для меня (0..2)

	// --- profiling ---
	prof struct {
		framesCaptured int64
		framesSent     int64
		bytesSent      int64
		captureErrors  int64
		captureSkipped int64 // кадр пропущен из-за adaptive throttle (конгестия)
		encodeErrors   int64
		sendErrors     int64
		lastCaptureMs  int64 // ms на capture+resize+cursor
		lastEncodeMs   int64 // ms на I420 + encode
		lastFanoutMs   int64 // ms на fanout (Send всем пирам)
		keyframes      int64 // отправлено IDR
		pliRx          int64 // получено PLI от зрителей
	}

	// --- receiver state: reassembly buffers ---
	// Key: peerID (один отправитель = один поток). При смене FrameID buffer
	// обнуляется, так что мы держим максимум один in-flight кадр на пира.
	rxMu  sync.Mutex
	rxBuf map[string]*screenReassembly
}

// screenReassembly — состояние сборки одного кадра от конкретного отправителя.
type screenReassembly struct {
	frameID    uint32
	codec      uint8
	flags      uint8 // keyframe-флаг (на любом chunk кадра)
	width      uint16
	height     uint16
	chunkTotal uint16
	chunks     [][]byte // index = chunkIdx, nil если ещё не получен
	received   uint16   // сколько chunks уже пришло
	startedAt  time.Time
}

// encodedScreenFrame — закодированный кадр, готовый к фан-ауту.
type encodedScreenFrame struct {
	chunks     [][]byte
	payloadLen int // длина JPEG payload (для kbps-метрики)
}

// Adaptive bitrate. Контроллер крутит целевой битрейт H.264 по обратной связи
// от SCTP-saturation, чтобы поток влезал в фактический транспорт (через
// TURN-relay ~2.5–4 Mbps, делённые с voice/files) и не душил голос
// DTLS-таймаутами. Watermark'и относительные (доля битрейта, см.
// adaptBitrate); константы ниже — нижние границы.
const (
	screenSatHighMin = 48 * 1024 // минимум high-WM (байт в буфере)
	screenSatLowMin  = 12 * 1024 // минимум low-WM
	// screenSatHighMillis — high-WM в миллисекундах потока при текущем
	// битрейте: столько задержки мы готовы копить в SCTP-буфере. Было 250 мс:
	// на 1080p60 (6 Мбит/с) это 187 КБ, и буфер жил у верхнего порога —
	// задержка движения заметно больше, чем на 1080p30 (Тим, 2026-09-08).
	screenSatHighMillis = 100
	// screenRaiseInterval — поднимать битрейт на 10 % не чаще, чем раз в
	// столько времени «здорового» канала (в кадрах = fps × секунды; раньше
	// была константа 60 кадров, т.е. на 60 fps вдвое чаще → осцилляции).
	screenRaiseInterval = 2 * time.Second
	// screenKeyframeMinInterval — не форсить IDR чаще (PLI-шторм от зрителей).
	screenKeyframeMinInterval = 500 * time.Millisecond
)

// NewScreenShareService — создание сервиса.
func NewScreenShareService(log *slog.Logger, store *storage.Storage, sess *session.Session, network *NetworkService, groups *GroupsService, voice *VoiceService) *ScreenShareService {
	fallbackLog = func(err error) {
		log.Warn("dxgi capture unavailable, fallback BitBlt", slog.String("error", err.Error()))
	}
	return &ScreenShareService{
		log:          log,
		store:        store,
		sess:         sess,
		network:      network,
		groups:       groups,
		voice:        voice,
		rxBuf:        make(map[string]*screenReassembly),
		audioRx:      make(map[string]*screenAudioRx),
		shareVolumes: make(map[string]float32),
	}
}

// InitCallbacks регистрирует receiver-side обработчик. Вызывать один раз при
// старте, после того как NetworkService готов.
func (s *ScreenShareService) InitCallbacks() {
	s.network.SetOnScreenMessage(s.handleScreenChunk)
	s.network.SetOnScreenClosed(s.handleScreenClosed)
	s.network.SetOnScreenAudio(s.handleScreenAudio)
}

// handleScreenClosed — screen DC пира закрылся (стоп демонстрации или смерть
// PC): то же, что control-фрейм Stop — фронт убирает плитку сразу. Раньше
// зритель держал замороженный кадр до TILE_TIMEOUT в screenshare.js.
func (s *ScreenShareService) handleScreenClosed(fromPubKey string) {
	s.rxMu.Lock()
	delete(s.rxBuf, fromPubKey)
	s.rxMu.Unlock()
	s.closeAudioRx(fromPubKey)
	s.network.emitEvent("vox:screen-share-ended", map[string]any{"peerId": fromPubKey})

	// Страховка к finish() страницы окна: та сама закрывается через 1.5 с с
	// надписью «Демонстрация завершена». Поколение — чтобы не закрыть окно
	// новой демонстрации того же пира, открытое за это время.
	s.viewerMu.Lock()
	if e := s.viewers[fromPubKey]; e != nil {
		gen := e.gen
		time.AfterFunc(3*time.Second, func() {
			s.viewerMu.Lock()
			defer s.viewerMu.Unlock()
			if e := s.viewers[fromPubKey]; e != nil && e.gen == gen {
				s.closeViewerLocked(fromPubKey, true)
			}
		})
	}
	s.viewerMu.Unlock()
}

// ViewerWindow — окно демонстрации (реализация в main.go поверх Wails).
type ViewerWindow interface {
	Show()
	Close()
	Focus()
	SetAlwaysOnTop(bool)
	ToggleFullscreen()
	Minimise()
	ToggleMaximise()
}

// viewerEntry — открытое окно демонстрации пира. gen отличает закрытие окна
// пользователем от нашего Close() и от окна прошлой демонстрации.
type viewerEntry struct {
	win ViewerWindow
	gen int
}

// SetViewerWindowFactory — фабрика окна демонстрации. query — параметры для
// /screen-view.html; slot — сколько окон уже открыто (каскад); onClosed
// вызывается при закрытии окна любым путём.
func (s *ScreenShareService) SetViewerWindowFactory(fn func(peerID, title, query string, slot int, onClosed func()) ViewerWindow) {
	s.viewerMu.Lock()
	defer s.viewerMu.Unlock()
	s.viewerFactory = fn
}

// OpenViewerWindow — показать демонстрацию пира в отдельном окне (Wails-метод,
// из главного окна). Окно на пира: повторный вызов поднимает открытое, окна
// других пиров не трогает. title — подпись шарера, session — имя
// группы/собеседника, startedAt — начало сессии (unix ms) для таймера,
// theme — light|dark.
func (s *ScreenShareService) OpenViewerWindow(peerID, title, session string, startedAt int64, theme string) error {
	if peerID == "" {
		return fmt.Errorf("peerID required")
	}
	s.viewerMu.Lock()
	if e := s.viewers[peerID]; e != nil {
		win := e.win
		s.viewerMu.Unlock()
		win.Focus()
		return nil
	}
	factory := s.viewerFactory
	if factory == nil {
		s.viewerMu.Unlock()
		return fmt.Errorf("viewer window factory not set")
	}
	if s.viewers == nil {
		s.viewers = make(map[string]*viewerEntry)
	}
	slot := len(s.viewers)
	s.viewerGen++
	gen := s.viewerGen
	q := url.Values{}
	q.Set("peer", peerID)
	q.Set("title", title)
	q.Set("session", session)
	q.Set("startedAt", fmt.Sprint(startedAt))
	q.Set("theme", theme)
	win := factory(peerID, title, q.Encode(), slot, func() { s.onViewerClosed(peerID, gen) })
	if win == nil {
		s.viewerMu.Unlock()
		return fmt.Errorf("viewer window not created")
	}
	s.viewers[peerID] = &viewerEntry{win: win, gen: gen}
	s.viewerMu.Unlock()
	win.Show()
	s.log.Info("screen viewer window opened", slog.String("peer", safeShort(peerID)), slog.Int("slot", slot))
	return nil
}

// CloseViewerWindow — закрыть окно демонстрации пира (Wails-метод, из главного
// окна или по «Вернуть в приложение»). Главному окну уйдёт vox:screen-viewer-closed.
func (s *ScreenShareService) CloseViewerWindow(peerID string) error {
	s.viewerMu.Lock()
	defer s.viewerMu.Unlock()
	s.closeViewerLocked(peerID, true)
	return nil
}

// CloseAllViewerWindows — все окна демонстрации (выход из звонка / голоса).
func (s *ScreenShareService) CloseAllViewerWindows() error {
	s.viewerMu.Lock()
	defer s.viewerMu.Unlock()
	for peer := range s.viewers {
		s.closeViewerLocked(peer, true)
	}
	return nil
}

func (s *ScreenShareService) closeViewerLocked(peer string, notify bool) {
	e := s.viewers[peer]
	if e == nil {
		return
	}
	// Запись убираем до Close(): WindowClosing от нашего Close() найдёт пусто
	// (или окно новой демонстрации с другим поколением) и ничего не сделает.
	delete(s.viewers, peer)
	func() {
		defer func() { recover() }() // окно могло быть уже закрыто
		e.win.Close()
	}()
	if notify {
		s.network.emitEvent("vox:screen-viewer-closed", map[string]any{"peerId": peer})
	}
}

// onViewerClosed — окно закрыл пользователь (крестик / Alt+F4).
func (s *ScreenShareService) onViewerClosed(peer string, gen int) {
	s.viewerMu.Lock()
	e := s.viewers[peer]
	if e == nil || e.gen != gen {
		s.viewerMu.Unlock()
		return
	}
	delete(s.viewers, peer)
	s.viewerMu.Unlock()
	s.network.emitEvent("vox:screen-viewer-closed", map[string]any{"peerId": peer})
}

// ViewerAction — действия окна демонстрации над самим собой (из screen-view.html):
// pin | unpin | fullscreen | minimise | maximise | close.
func (s *ScreenShareService) ViewerAction(peerID, action string) error {
	if action == "close" {
		return s.CloseViewerWindow(peerID)
	}
	s.viewerMu.Lock()
	var win ViewerWindow
	if e := s.viewers[peerID]; e != nil {
		win = e.win
	}
	s.viewerMu.Unlock()
	if win == nil {
		return fmt.Errorf("viewer window not open")
	}
	switch action {
	case "pin":
		win.SetAlwaysOnTop(true)
	case "unpin":
		win.SetAlwaysOnTop(false)
	case "fullscreen":
		win.ToggleFullscreen()
	case "minimise":
		win.Minimise()
	case "maximise":
		win.ToggleMaximise()
	default:
		return fmt.Errorf("unknown viewer action %q", action)
	}
	return nil
}

// ViewerCommand — команда из окна демонстрации пира главному окну (mute |
// deafen | leave | back). Ретранслируется событием vox:screen-viewer-cmd:
// состояние голоса и звонка живёт в главном окне; peerId — для back.
func (s *ScreenShareService) ViewerCommand(peerID, cmd string) error {
	switch cmd {
	case "mute", "deafen", "leave", "back":
	default:
		return fmt.Errorf("unknown viewer command %q", cmd)
	}
	s.network.emitEvent("vox:screen-viewer-cmd", map[string]any{"cmd": cmd, "peerId": peerID})
	return nil
}

// ViewerState — общее для всех окон демонстрации состояние из главного окна
// (muted, deafened, session, startedAt, ended). Подпись окна — своя у
// каждого, из URL. Ретранслируется событием vox:screen-viewer-state.
func (s *ScreenShareService) ViewerState(state map[string]any) error {
	s.viewerMu.Lock()
	open := len(s.viewers) > 0
	s.viewerMu.Unlock()
	if !open {
		return nil
	}
	s.network.emitEvent("vox:screen-viewer-state", state)
	return nil
}

// IsActive — true если мы сейчас отправляем экран.
func (s *ScreenShareService) IsActive() (bool, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active, s.activeGroupID, nil
}

// ListDisplays — экраны для диалога выбора (Wails-метод).
func (s *ScreenShareService) ListDisplays() ([]DisplayInfo, error) {
	return listScreenDisplays(), nil
}

// RequestScreenKeyframe — зритель просит у отправителя keyframe (PLI): после
// ошибки декодера, при старте с середины потока, после reconnect screen PC.
// Идёт control-фреймом по тому же screen DC (он двунаправленный).
func (s *ScreenShareService) RequestScreenKeyframe(peerID string) error {
	if peerID == "" {
		return fmt.Errorf("peerID required")
	}
	if !s.network.IsScreenDCOpen(peerID) {
		return fmt.Errorf("screen DC с %s не открыт", safeShort(peerID))
	}
	return s.network.SendScreenChunk(peerID, encodeScreenControl(screenCtrlPLI))
}

// StartScreenShare запускает захват и фан-аут кадров на всех участников
// текущей голосовой группы. Если уже активен — ошибка.
//
// displayIdx: индекс экрана из ListDisplays (0 для primary).
// mode: "balanced" (по умолчанию) | "text" | "motion" — см. screenModes;
// старые "720p30"/"1080p30"/"1080p60" принимаются как синонимы.
// Битрейт из пресета — лишь стартовая точка; adaptive-контроллер в captureLoop
// крутит его в [BitrateMin..BitrateMax] по обратной связи от SCTP-saturation.
func (s *ScreenShareService) StartScreenShare(displayIdx int, mode string, withAudio bool) error {
	groupID, _ := s.voice.GetActiveVoiceGroup()
	if groupID == "" {
		return fmt.Errorf("нет активного голосового канала — зайдите в voice и попробуйте снова")
	}
	if mode == "" {
		mode = screenModeDefault
	}
	if m, ok := legacyScreenModes[mode]; ok {
		mode = m
	}
	preset, ok := screenModes[mode]
	if !ok {
		return fmt.Errorf("неизвестный режим демонстрации %q", mode)
	}
	// Звук системы — только если его можно захватить без звука самого Vox.
	audioOn := withAudio && s.ScreenAudioSupported()

	s.mu.Lock()
	if s.active {
		s.mu.Unlock()
		return fmt.Errorf("screen share уже активен")
	}
	s.cfg = ScreenShareConfig{
		DisplayIdx: displayIdx,
		Mode:       mode,
		Width:      preset.Width,
		Height:     preset.Height,
		TargetFPS:  preset.FPS,
		Bitrate:    preset.Bitrate,
		BitrateMin: preset.BitrateMin,
		BitrateMax: preset.BitrateMax,
		WithAudio:  audioOn,
	}
	s.active = true
	s.activeGroupID = groupID
	s.stopCh = make(chan struct{})
	s.frameCounter = 0
	s.pendingFrame = nil
	s.pendingSig = make(chan struct{}, 1)
	s.ensureAt = make(map[string]time.Time)
	s.pliPending.Store(false)
	atomic.StoreInt32(&s.curBitrate, int32(preset.Bitrate/1000))
	atomic.StoreInt32(&s.curFPS, int32(preset.FPS))
	s.mu.Unlock()

	// Проактивно поднимаем screen PC со всеми участниками голосовой группы,
	// чтобы к первому кадру DC уже были открыты и fanout не ждал подключения.
	s.ensureAllMembers(groupID)

	s.captureWG.Add(1)
	go s.captureLoop()
	s.captureWG.Add(1)
	go s.senderLoop()
	s.captureWG.Add(1)
	go s.statsLoop()
	if audioOn {
		s.captureWG.Add(1)
		go s.audioLoop()
	}

	s.network.emitEvent("vox:screen-share-started", map[string]any{
		"groupId":    groupID,
		"displayIdx": displayIdx,
		"mode":       mode,
		"fps":        preset.FPS,
		"audio":      audioOn,
	})
	s.log.Info("screen share start",
		slog.String("group", safeShort8(groupID)),
		slog.Int("display", displayIdx),
		slog.String("mode", mode))
	return nil
}

// viewerIDs — кому слать экран: пиры, которые СЕЙЧАС в голосовом канале
// группы (VoiceService.channelPeers), без нас. Раньше брались все участники
// группы (GetGroupMemberIDs): демонстрация уезжала и тем, кто в канал не
// заходил (Тим, 2026-09-08: ноутбук получил экран, не входя в группу).
func (s *ScreenShareService) viewerIDs(groupID string) []string {
	if s.voice == nil || groupID == "" {
		return nil
	}
	myID, _ := s.sess.PeerID()
	all := s.voice.VoicePeerIDs(groupID)
	out := all[:0]
	for _, pid := range all {
		if pid != myID {
			out = append(out, pid)
		}
	}
	return out
}

// ensureAllMembers лениво (асинхронно, без блокировки) поднимает screen PC со
// всеми участниками голосового канала, у кого ещё нет открытого screen DC.
func (s *ScreenShareService) ensureAllMembers(groupID string) {
	if groupID == "" {
		return
	}
	for _, pid := range s.viewerIDs(groupID) {
		if s.network.IsScreenDCOpen(pid) {
			continue
		}
		pidCopy := pid
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = s.network.EnsureScreenPCReady(ctx, pidCopy)
		}()
	}
}

// StopScreenShare останавливает захват и закрывает screen PC со всеми пирами
// текущей группы.
func (s *ScreenShareService) StopScreenShare() error {
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return nil
	}
	close(s.stopCh)
	s.active = false
	groupID := s.activeGroupID
	s.activeGroupID = ""
	s.mu.Unlock()

	s.captureWG.Wait()

	// Закрываем screen PC со всеми пирами группы — DC сами лопнут на их стороне.
	// Асинхронно: pc.Close() с DTLS через TURN занимает секунды на пира, а
	// вызов идёт из UI (кнопка «Стоп», отбой звонка) — Тим видел задержку.
	if groupID != "" {
		stopFrame := encodeScreenControl(screenCtrlStop)
		for _, pid := range s.viewerIDs(groupID) {
			// «Стоп» зрителю до закрытия PC: иначе его тайл живёт до таймаута
			// неактивности (4 с) — Тим видел «окно долго висит».
			if s.network.IsScreenDCOpen(pid) {
				_ = s.network.SendScreenChunk(pid, stopFrame)
			}
			go s.network.CloseScreenPC(pid)
		}
	}

	s.network.emitEvent("vox:screen-share-stopped", map[string]any{
		"groupId": groupID,
	})
	s.log.Info("screen share stop", slog.String("group", safeShort8(groupID)))
	return nil
}

// captureLoop — capture → I420 → H.264 (fallback JPEG) → chunks → mailbox.
// Монотонный планировщик: nextDeadline строится от prev+interval, при
// опоздании НЕ спим (пропускаем «догон» вместо накопления отставания).
// Закодированный кадр кладётся в mailbox (deposit), откуда его серийно
// забирает senderLoop; параллелизм с сетью — senderLoop шлёт кадр N, пока мы
// кодируем N+1. Без второго encode-в-полёте нет interleaving чанков двух
// кадров на одном DC.
//
// DXGI/COM привязаны к потоку — захват (и создание D3D-устройства) живёт
// только в этой горутине, поэтому LockOSThread.
func (s *ScreenShareService) captureLoop() {
	defer s.captureWG.Done()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	s.mu.Lock()
	cfg := s.cfg
	stop := s.stopCh
	s.mu.Unlock()

	fail := func(msg string, err error) {
		s.log.Warn(msg, slog.Int("display", cfg.DisplayIdx), slog.String("error", err.Error()))
		s.network.emitEvent("vox:screen-share-error", map[string]any{"error": msg + ": " + err.Error()})
		s.mu.Lock()
		s.active = false
		s.mu.Unlock()
	}

	cap, backend, err := openBestScreenCapture(cfg.DisplayIdx, cfg.Width)
	if err != nil {
		fail("screen capture open failed", err)
		return
	}
	// Замыкания, а не defer cap.Close(): при потере DXGI захват и энкодер
	// меняются посреди показа (см. ниже), закрыть надо последние.
	defer func() { cap.Close() }()
	w, h := cap.Size()
	w &^= 1
	h &^= 1
	if w <= 0 || h <= 0 {
		fail("screen capture", fmt.Errorf("пустой кадр %dx%d", w, h))
		return
	}
	s.curCapture.Store(backend)

	codec := uint8(screenCodecH264)
	var enc *h264enc.Encoder
	var yuv *image.YCbCr
	defer func() {
		if enc != nil {
			enc.Close()
		}
	}()
	// openEnc — энкодер под размер кадра (при старте и при смене backend'а).
	openEnc := func(w, h, fps, bitrate int) {
		if enc != nil {
			enc.Close()
			enc = nil
		}
		yuv = nil
		codec = screenCodecH264
		e, err := h264enc.New(h264enc.Config{Width: w, Height: h, FPS: fps, Bitrate: bitrate})
		if err != nil {
			// Нет статической libopenh264 на платформе или отказ кодека — JPEG
			// как раньше, ресивер умеет оба кодека.
			s.log.Warn("h264 encoder unavailable, fallback JPEG", slog.String("error", err.Error()))
			codec = screenCodecJPEG
		} else {
			enc = e
			yuv = image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
		}
		atomic.StoreInt32(&s.curCodec, int32(codec))
	}
	openEnc(w, h, cfg.TargetFPS, cfg.Bitrate)
	s.log.Info("screen share pipeline",
		slog.String("capture", backend),
		slog.String("codec", screenCodecName(codec)),
		slog.String("mode", cfg.Mode),
		slog.Int("w", w), slog.Int("h", h),
		slog.Int("fps", cfg.TargetFPS),
		slog.Int("bitrate_kbps", cfg.Bitrate/1000))

	myID, _ := s.sess.PeerID()

	// Adaptive state. Изменяется только в этой горутине; в curBitrate/curFPS
	// дублируется атомарно для statsLoop.
	bitrate := cfg.Bitrate
	prevBitrate := bitrate
	fps := cfg.TargetFPS
	goodTicks := 0
	raiseTicks := int(screenRaiseInterval.Seconds() * float64(fps))
	atomic.StoreInt32(&s.curBitrate, int32(bitrate/1000))
	atomic.StoreInt32(&s.curFPS, int32(fps))
	const jpegFallbackQuality = 40
	var lastKey time.Time
	var lostSince time.Time // первая потеря доступа DXGI после удачного кадра

	nextDeadline := time.Now()
	for {
		interval := time.Second / time.Duration(fps)
		now := time.Now()
		if d := nextDeadline.Sub(now); d > 0 {
			select {
			case <-stop:
				return
			case <-time.After(d):
			}
		} else {
			select {
			case <-stop:
				return
			default:
			}
		}
		nextDeadline = time.Now().Add(interval)

		// --- adaptive bitrate по обратной связи от SCTP-saturation ---
		var skip bool
		bitrate, goodTicks, skip = adaptBitrate(s.network.ScreenDCSaturation(), bitrate, cfg.BitrateMin, cfg.BitrateMax, goodTicks, raiseTicks)
		if enc != nil && bitrate != prevBitrate {
			enc.SetBitrate(bitrate)
			prevBitrate = bitrate
		}
		atomic.StoreInt32(&s.curBitrate, int32(bitrate/1000))
		if skip {
			// Канал переполнен — пропускаем capture, даём буферу разгрузиться.
			s.prof.captureSkipped++
			continue
		}

		started := time.Now()
		frame, err := cap.Capture()
		if err != nil {
			s.prof.captureErrors++
			s.log.Debug("screen capture error", slog.String("error", err.Error()))
			if errors.Is(err, errDXGIAccessLost) && lostSince.IsZero() {
				lostSince = time.Now()
			}
			if shouldFallbackDXGI(lostSince, time.Now(), backend) {
				// DXGI не отдаёт кадры (полноэкранная игра и т.п.), а BitBlt этот
				// экран снимает. Обратно до конца показа не переключаемся.
				lostSince = time.Time{}
				bb, berr := openScreenCapture(cfg.DisplayIdx, cfg.Width)
				if berr != nil {
					s.log.Warn("screen capture: BitBlt fallback failed", slog.String("error", berr.Error()))
					continue
				}
				s.log.Warn("screen capture: DXGI access lost, fallback BitBlt", slog.Int("display", cfg.DisplayIdx))
				cap.Close()
				cap, backend = bb, "bitblt"
				s.curCapture.Store(backend)
				if nw, nh := cap.Size(); nw&^1 != w || nh&^1 != h {
					w, h = nw&^1, nh&^1
					openEnc(w, h, fps, bitrate)
					prevBitrate = bitrate
				}
			}
			continue
		}
		lostSince = time.Time{}
		s.prof.framesCaptured++
		captureMs := time.Since(started).Milliseconds()

		s.frameCounter++
		fid := s.frameCounter

		encStart := time.Now()
		var payload []byte
		key := true
		if enc != nil {
			if frame.Width != w || frame.Height != h {
				s.prof.encodeErrors++
				continue
			}
			h264enc.RGBAToI420(frame.Img, yuv)
			forceKey := false
			if s.pliPending.Swap(false) && time.Since(lastKey) > screenKeyframeMinInterval {
				forceKey = true
			}
			payload, key, err = enc.Encode(yuv, forceKey)
			if err != nil {
				s.prof.encodeErrors++
				s.log.Debug("screen encode error", slog.String("error", err.Error()))
				continue
			}
			if len(payload) == 0 {
				continue // кодек пропустил кадр
			}
			if key {
				lastKey = time.Now()
				s.prof.keyframes++
			}
		} else {
			payload, err = encodeJPEG(frame, jpegFallbackQuality)
			if err != nil {
				s.prof.encodeErrors++
				continue
			}
		}
		s.prof.lastEncodeMs = time.Since(encStart).Milliseconds()
		s.prof.lastCaptureMs = captureMs

		var flags uint8
		if key {
			flags |= screenFlagKeyframe
		}
		header := screenFrameHeader{
			Version: screenWireVersion,
			Flags:   flags,
			Codec:   codec,
			FrameID: fid,
			Width:   uint16(frame.Width),
			Height:  uint16(frame.Height),
		}
		chunks := encodeScreenChunks(payload, header)

		// Кладём в mailbox самый свежий кадр (перезаписывая ещё не отправленный).
		s.deposit(&encodedScreenFrame{chunks: chunks, payloadLen: len(payload)})

		// Превью у отправителя: тот же поток в свой JS-декодер (аппаратный).
		if myID != "" {
			s.emitFrameEvent(myID, fid, codec, key, uint16(frame.Width), uint16(frame.Height), payload)
		}
	}
}

// adaptBitrate — один шаг контроллера битрейта по обратной связи от
// SCTP-saturation (BufferedAmount худшего screen DC, байт). Watermark'и
// считаются от текущего битрейта: high = screenSatHighMillis потока, low =
// high/4 (но не ниже screenSatHighMin/LowMin, иначе на низких битрейтах
// пороги вырождаются в единицы КБ). Выше high — режем −20 % и пропускаем
// capture; ниже low raiseTicks тиков подряд — +10 %. Асимметрия «вниз
// быстро, вверх медленно» защищает от осцилляций вокруг порога.
func adaptBitrate(sat uint64, bitrate, minB, maxB, goodTicks, raiseTicks int) (newBitrate, newGoodTicks int, skip bool) {
	if minB <= 0 {
		minB = 1
	}
	if maxB < minB {
		maxB = minB
	}
	if bitrate < minB {
		bitrate = minB
	}
	if bitrate > maxB {
		bitrate = maxB
	}
	if raiseTicks <= 0 {
		raiseTicks = 1
	}
	high := uint64(bitrate) / 8 * screenSatHighMillis / 1000
	if high < screenSatHighMin {
		high = screenSatHighMin
	}
	low := high / 4
	if low < screenSatLowMin {
		low = screenSatLowMin
	}
	if sat > high {
		bitrate = bitrate * 4 / 5
		if bitrate < minB {
			bitrate = minB
		}
		return bitrate, 0, true
	}
	if sat < low {
		goodTicks++
		if goodTicks >= raiseTicks {
			goodTicks = 0
			bitrate = bitrate * 11 / 10
			if bitrate > maxB {
				bitrate = maxB
			}
		}
	} else {
		goodTicks = 0
	}
	return bitrate, goodTicks, false
}

// deposit кладёт кадр в mailbox latest-frame-wins и будит senderLoop.
func (s *ScreenShareService) deposit(f *encodedScreenFrame) {
	s.txMu.Lock()
	s.pendingFrame = f
	s.txMu.Unlock()
	select {
	case s.pendingSig <- struct{}{}:
	default:
	}
}

// senderLoop — единственный отправитель. Забирает самый свежий кадр из mailbox
// и фанаутит его серийно (один кадр на проводе за раз → нет interleaving чанков
// двух кадров на одном DC). Между кадрами берёт всегда последний, так что
// устаревшие кадры тихо вытесняются, а не копятся в очереди.
func (s *ScreenShareService) senderLoop() {
	defer s.captureWG.Done()
	s.mu.Lock()
	stop := s.stopCh
	s.mu.Unlock()
	if stop == nil {
		return
	}
	for {
		select {
		case <-stop:
			return
		case <-s.pendingSig:
		}
		s.txMu.Lock()
		f := s.pendingFrame
		s.pendingFrame = nil
		s.txMu.Unlock()
		if f == nil {
			continue
		}
		s.sendFrame(f)
	}
}

// statsLoop — периодически (раз в 2 сек) дампит счётчики в лог. Когда
// демонстрация лагает у пользователя, по этому логу видно где боттлнек:
// — quality/fps_target упали к полу: канал узкий, adaptive дросселит битрейт
// — capture_skipped растёт: конгестия, буфер screen DC выше high-watermark
// — fan_ms высокий: encode/сеть медленные; send_errors: SCTP backpressure/DC закрыт
func (s *ScreenShareService) statsLoop() {
	defer s.captureWG.Done()
	s.mu.Lock()
	stop := s.stopCh
	s.mu.Unlock()
	if stop == nil {
		return
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var prevSent, prevBytes int64
	prevTs := time.Now()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		now := time.Now()
		dt := now.Sub(prevTs).Seconds()
		framesSent := atomic.LoadInt64(&s.prof.framesSent)
		bytesSent := atomic.LoadInt64(&s.prof.bytesSent)
		dSent := framesSent - prevSent
		dBytes := bytesSent - prevBytes
		prevSent = framesSent
		prevBytes = bytesSent
		prevTs = now

		fps := float64(dSent) / dt
		kbps := float64(dBytes) * 8 / 1000 / dt
		s.log.Info("screen share stats",
			slog.Float64("fps_out", fps),
			slog.Float64("kbps", kbps),
			slog.String("codec", screenCodecName(uint8(atomic.LoadInt32(&s.curCodec)))),
			slog.Any("capture", s.curCapture.Load()),
			slog.Int("bitrate_kbps", int(atomic.LoadInt32(&s.curBitrate))),
			slog.Int("fps_target", int(atomic.LoadInt32(&s.curFPS))),
			slog.Int64("keyframes", s.prof.keyframes),
			slog.Int64("pli_rx", atomic.LoadInt64(&s.prof.pliRx)),
			slog.Int64("frames_captured", s.prof.framesCaptured),
			slog.Int64("frames_sent", framesSent),
			slog.Int64("capture_skipped", s.prof.captureSkipped),
			slog.Int64("capture_errors", s.prof.captureErrors),
			slog.Int64("encode_errors", s.prof.encodeErrors),
			slog.Int64("send_errors", atomic.LoadInt64(&s.prof.sendErrors)),
			slog.Int64("cap_ms", s.prof.lastCaptureMs),
			slog.Int64("enc_ms", s.prof.lastEncodeMs),
			slog.Int64("fan_ms", s.prof.lastFanoutMs))
	}
}

// sendFrame рассылает chunks одного кадра участникам группы. На пиры с уже
// открытым screen DC — ПАРАЛЛЕЛЬНО (по горутине на пира; Send'ы внутри одного
// пира серийны — инвариант SCTP-фрагментации в pion). На пиры без открытого DC
// — НЕ блокируемся: просто пропускаем кадр и асинхронно (троттлинг) поднимаем им
// screen PC. Старый код ждал EnsureScreenPCReady(100ms) на КАЖДОМ кадре для
// каждого оффлайн-пира, и wg.Wait() упирался в этот таймаут → fps_out падал до
// ~8 даже на быстром канале (см. логи 2026-05-02: fan_ms≡100, пир 567de3ef
// «не в сети»). Теперь оффлайн-пир стоит ~0 ms.
func (s *ScreenShareService) sendFrame(f *encodedScreenFrame) {
	s.mu.Lock()
	groupID := s.activeGroupID
	s.mu.Unlock()
	if groupID == "" || len(f.chunks) == 0 {
		return
	}
	members := s.viewerIDs(groupID)

	fanStart := time.Now()
	var (
		wg        sync.WaitGroup
		delivered int64
	)
	for _, pid := range members {
		if !s.network.IsScreenDCOpen(pid) {
			s.maybeEnsureScreenPC(pid)
			continue
		}
		wg.Add(1)
		go func(pid string) {
			defer wg.Done()
			for _, c := range f.chunks {
				if err := s.network.SendScreenChunk(pid, c); err != nil {
					atomic.AddInt64(&s.prof.sendErrors, 1)
					return
				}
			}
			atomic.AddInt64(&delivered, 1)
		}(pid)
	}
	wg.Wait()

	s.prof.lastFanoutMs = time.Since(fanStart).Milliseconds()
	atomic.AddInt64(&s.prof.framesSent, 1)
	// Считаем только реально доставленные байты — kbps в логах = network use,
	// а не intent.
	atomic.AddInt64(&s.prof.bytesSent, int64(f.payloadLen)*delivered)
}

// maybeEnsureScreenPC лениво и НЕблокирующе поднимает screen PC пиру без
// открытого DC, не чаще раза в 2 секунды на пира. Вызывается только из
// senderLoop → доступ к ensureAt без локов.
func (s *ScreenShareService) maybeEnsureScreenPC(pid string) {
	now := time.Now()
	if last, ok := s.ensureAt[pid]; ok && now.Sub(last) < 2*time.Second {
		return
	}
	s.ensureAt[pid] = now
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.network.EnsureScreenPCReady(ctx, pid)
	}()
}

// assembledScreenFrame — полностью собранный кадр, готовый к отправке в JS.
type assembledScreenFrame struct {
	peerID   string
	frameID  uint32
	codec    uint8
	keyframe bool
	width    uint16
	height   uint16
	data     []byte
}

// handleScreenChunk — receiver hook от NetworkService. Собирает chunks одного
// кадра; когда все собраны — emit'ит целый кадр в JS.
func (s *ScreenShareService) handleScreenChunk(fromPubKey string, data []byte) {
	if s.network.IsBlocked(fromPubKey) {
		return // заблокированный (blocklist.go)
	}
	// Control-фреймы (PLI от зрителя) — до reassembly: у них нет payload кадра.
	if op, ok := decodeScreenControl(data); ok {
		switch op {
		case screenCtrlPLI:
			atomic.AddInt64(&s.prof.pliRx, 1)
			s.pliPending.Store(true)
		case screenCtrlStop:
			// Отправитель закончил — фронт убирает тайл сразу.
			s.rxMu.Lock()
			delete(s.rxBuf, fromPubKey)
			s.rxMu.Unlock()
			s.closeAudioRx(fromPubKey)
			s.network.emitEvent("vox:screen-share-ended", map[string]any{"peerId": fromPubKey})
		}
		return
	}
	f, ok := s.ingestChunk(fromPubKey, data)
	if !ok {
		return
	}
	s.emitFrameEvent(f.peerID, f.frameID, f.codec, f.keyframe, f.width, f.height, f.data)
}

// emitFrameEvent — целый кадр в JS (vox:screen-frame). Для входящих кадров
// peerID — отправитель; для превью собственной демонстрации — свой peerID.
func (s *ScreenShareService) emitFrameEvent(peerID string, frameID uint32, codec uint8, keyframe bool, width, height uint16, data []byte) {
	s.network.emitEvent("vox:screen-frame", map[string]any{
		"peerId":    peerID,
		"frameId":   frameID,
		"codec":     screenCodecName(codec),
		"keyframe":  keyframe,
		"width":     width,
		"height":    height,
		"data":      base64.StdEncoding.EncodeToString(data),
		"sizeBytes": len(data),
	})
}

// ingestChunk — чистая reassembly-логика (без сети, тестируется напрямую).
// Возвращает собранный кадр и true, когда все chunks кадра получены; иначе nil,false.
func (s *ScreenShareService) ingestChunk(fromPubKey string, data []byte) (*assembledScreenFrame, bool) {
	header, payload, err := decodeScreenHeader(data)
	if err != nil {
		s.log.Debug("screen frame decode error",
			slog.String("from", safeShort(fromPubKey)),
			slog.String("error", err.Error()))
		return nil, false
	}
	if header.Flags&screenFlagControl != 0 {
		return nil, false // control-фреймы разбирает handleScreenChunk
	}

	s.rxMu.Lock()
	r, ok := s.rxBuf[fromPubKey]
	if ok && header.FrameID < r.frameID {
		// Устаревший straggler от предыдущего кадра (unordered DC может доставить
		// чанк старого кадра после того как мы уже начали собирать новый). НЕ
		// сбрасываем буфер назад — просто игнорируем, иначе теряем прогресс
		// текущего, более свежего кадра.
		s.rxMu.Unlock()
		return nil, false
	}
	if !ok || r.frameID != header.FrameID {
		// Новый (более свежий) кадр — заводим/перезаводим reassembly. Старый
		// незаконченный кадр от этого пира выкидываем (видеопоток real-time).
		r = &screenReassembly{
			frameID:    header.FrameID,
			codec:      header.Codec,
			flags:      header.Flags,
			width:      header.Width,
			height:     header.Height,
			chunkTotal: header.ChunkTotal,
			chunks:     make([][]byte, header.ChunkTotal),
			startedAt:  time.Now(),
		}
		s.rxBuf[fromPubKey] = r
	}
	if int(header.ChunkIdx) >= len(r.chunks) {
		s.rxMu.Unlock()
		return nil, false // повреждённый header
	}
	if r.chunks[header.ChunkIdx] == nil {
		r.chunks[header.ChunkIdx] = append([]byte(nil), payload...)
		r.received++
	}
	if header.IsKeyframe() {
		r.flags |= screenFlagKeyframe
	}
	if r.received != r.chunkTotal {
		s.rxMu.Unlock()
		return nil, false
	}
	delete(s.rxBuf, fromPubKey)
	s.rxMu.Unlock()

	// Склеиваем chunks в полный кадр.
	totalLen := 0
	for _, c := range r.chunks {
		totalLen += len(c)
	}
	full := make([]byte, 0, totalLen)
	for _, c := range r.chunks {
		full = append(full, c...)
	}
	return &assembledScreenFrame{
		peerID:   fromPubKey,
		frameID:  r.frameID,
		codec:    r.codec,
		keyframe: r.flags&screenFlagKeyframe != 0,
		width:    r.width,
		height:   r.height,
		data:     full,
	}, true
}

// CloseScreenPC оборачивает PeerManager.closePeerKind для использования
// из вне сервиса (требуется в Stop). Не экспортируем напрямую — оборачиваем
// чтобы не плодить публичных хелперов в peerMgr.
//
// (Эта функция объявлена в network.go рядом с другими Close* для PC.)

// safeShort8 / emitEvent уже определены в services package.
