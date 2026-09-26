package services

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"vox_app/internal/audio"
	"vox_app/internal/audio/vad"
	"vox_app/internal/codec"
	"vox_app/internal/session"
	"vox_app/internal/storage"
	"os"
	"strconv"
	"sync"
	"time"
)

// VoiceParticipant — участник голосового канала
type VoiceParticipant struct {
	PeerID   string `json:"peerId"`
	Nickname string `json:"nickname"`
	Speaking bool   `json:"speaking"`
	Muted    bool   `json:"muted"`
}

// VoiceService — голосовые каналы + воспроизведение голосовых сообщений.
//
// INVARIANT: все поля ниже (кроме log, store, sess, network, groups)
// читаются и пишутся только под s.mu. Исключение: emitEvent
// вызывается после Unlock через deferredEvent snapshot.
type VoiceService struct {
	log     *slog.Logger
	store   *storage.Storage
	sess    *session.Session
	network *NetworkService
	groups  *GroupsService

	mu            sync.Mutex
	capture       *audio.Capture
	playback      *audio.Playback
	activeGroupID string

	// Отслеживание hot-swap audio устройств. audioWatcher polling'ом сечёт смену
	// default mic/speaker (юзер переключил наушники/колонки в системе) и шлёт
	// event'ы на канал. watchedMicIdx/watchedSpkIdx — что было выбрано юзером
	// при Join (-1 = default): если юзер взял конкретное устройство, мы не
	// рестартим его при смене default'а в системе.
	audioWatcher   *audio.DefaultDeviceWatcher
	watchedMicIdx  int
	watchedSpkIdx  int

	// dfnLoadWG — in-flight async-загрузки DFN3 модели. leaveInternal ждёт
	// завершения перед закрытием capture, иначе cgo df_create может писать в
	// уже освобождённый nx_capture.
	dfnLoadWG sync.WaitGroup

	// Neural NS: взаимоисключающий режим — либо FastEnhancer, либо GTCRN, либо ничего.
	// neuralMode: "off" (default) / "fe" / "gtcrn". Для мастера А/B — только один
	// активен за раз, чтобы слышать эффект конкретной модели без стэка.
	//
	// Загрузка ASYNC: NewFastEnhancer/NewGTCRN занимают 100-1500мс (парсинг ONNX
	// + ORT init + warm-up). Синхронно под s.mu блокировало audioLoop, capture
	// накапливал сэмплы и росла задержка. Решение: feLoadGen growing counter +
	// goroutine проверяет gen после загрузки и apply'ит только если gen совпадает
	// (юзер не передумал). feLoadWG — для leaveInternal чтобы не освободить ORT
	// раньше чем закончится in-flight загрузка.
	neuralMode string // "off" / "fe" / "gtcrn"

	fastEnhancer *audio.FastEnhancer
	feSize       audio.FastEnhancerSize // "t"/"b"/"s"/"m"/"l" (default "b")
	feMix        float32                // 0..1, wet/dry (default 1.0)

	gtcrn        *audio.GTCRN
	gtcrnVariant audio.GTCRNVariant // "dns3" (default) / "simple"
	gtcrnMix     float32            // 0..1 (default 1.0)

	// Кэш всех загруженных моделей (per-VoiceService session): после Join в
	// voice последовательно prewarm'им все 5 FE + 2 GTCRN в фоне. Переключение
	// режимов = мгновенный pointer swap из кэша. Это критично — иначе
	// ort.NewSession() во время активной audioLoop.Run() блокирует Run() через
	// внутренний синхронизатор ORT runtime → max_loop прыгает на сотни ms.
	// Кэш чистится в leaveInternal.
	feCache    map[audio.FastEnhancerSize]*audio.FastEnhancer
	gtcrnCache map[audio.GTCRNVariant]*audio.GTCRN
	feLoadGen  uint64 // generation counter для in-flight load (если кэш ещё не готов)
	feLoadWG   sync.WaitGroup
	participants  map[string]bool              // peerID → true (в канале, для audioLoop)
	channelPeers  map[string]map[string]bool   // groupID → set of peerIDs (глобальный трекинг)
	peerVolumes       map[string]float32 // peerID → volume
	masterVoiceVolume float32            // мастер-громкость голосов (0.0-1.4), множится поверх per-peer
	peerSpeaking      map[string]bool    // peerID → speaking (для UI индикатора)
	speakDecay        map[string]int     // peerID → frames left before speaking=false
	peerLastFrame     map[string]time.Time
	muted             bool
	deafened          bool
	mutedBeforeDeafen bool // запомнить состояние mute перед deafen
	stopCh            chan struct{}

	// audioWG — audioLoop + мониторы текущей сессии. leaveInternal ждёт их
	// завершения ПЕРЕД тем как освободить capture/playback (C-heap), иначе
	// StreamRead/pb.Write попадают в freed-память (use-after-free крэш).
	audioWG sync.WaitGroup

	// Opus codec: encoder один на сессию, decoder per-peer
	opusEncoder  *codec.Encoder
	opusDecoders map[string]*codec.Decoder

	// Wire format: per-peer состояние последнего принятого seq для FEC/PLC
	// recovery. Исходящий seq (txSeq) — локальная переменная audioLoop.
	peerRxSeq map[string]peerSeqState

	// Audio mixing: per-peer очередь фреймов для микширования в audioLoop
	mixBuf   map[string][][]byte // peerID → queue of decoded PCM frames (640 bytes each)
	mixBufTs map[string][]int64  // параллельная очередь: ns timestamps enqueue для dwell-time probe

	// --- Voice profiling counters ---
	prof struct {
		captureFrames   int64     // фреймов прочитано с микрофона
		sentFrames      int64     // фреймов отправлено (суммарно по всем пирам)
		sendErrors      int64     // ошибок отправки
		recvFrames      int64     // фреймов получено от пиров
		playbackWrites  int64     // записей реального микса в playback буфер
		playbackSilence int64     // tick'ов, на которых писали silence для поддержания ring gap
		dropNoPeer      int64     // отброшено: пир не в participants
		dropMuted       int64     // отброшено: пир заглушен
		maxLoopTime     time.Duration // максимальное время одной итерации audioLoop
		maxRecvTime     time.Duration // максимальное время обработки handleVoiceFrame
		lastRecvJitter  float64   // jitter между приходом фреймов (ms)
		recvIntervals   []float64 // последние 50 интервалов между фреймами (ms)
		lastRecvTime    time.Time // время получения предыдущего фрейма
		sessionStart    time.Time // начало сессии
		// --- RX diagnostic counters ---
		decodeErrors  int64 // opus_decode вернул ошибку (тихий return)
		seqSkips      int64 // VOX1 seq gap ≤ 0 — дубль/OOO фрейм отброшен
		// --- Wire-latency probe (DFNP header: магик + ns timestamp на send) ---
		latProbeCount   int64 // сколько пакетов с валидным header
		latProbeSumNs   int64 // сумма (recv_ns - send_ns)
		latProbeMaxNs   int64
		latProbeMinNs   int64 // -1 = не инициализирован
		// --- Mix queue dwell time (recv → mixer-write) ---
		mixDwellCount    int64
		mixDwellSumNs    int64
		mixDwellMaxNs    int64
		mixQueuePeak     int   // максимум len(mixBuf[peer]) за период
		mixOverflowDrops int   // фреймов отброшено из-за bound jitter buffer
		// --- SFU path observability ---
		// sfuSentFrames: +1 за каждый фрейм, отправленный в sfuConn.send (не per-peer).
		// sfuRecvFrames: +1 за каждый фрейм, полученный через sfuReadPump.
		// Если оба нулевые при sfuEnabled=true — голос идёт mesh'ом.
		sfuSentFrames int64
		sfuRecvFrames int64
		// --- Bandwidth counter ---
		// sentBytes: суммарные wire-байты (frameSize × успешные отправки).
		// В mesh это N×frameSize, в SFU — 1×frameSize на фрейм.
		// Нужен для сравнения режимов (без DTX / DTX / DTX+VAD) в UI.
		sentBytes int64
		// --- VAD counters ---
		vadSkipped      int64 // фреймов отброшено VAD'ом (с учётом pre-roll/hangover)
		vadInferenceErr int64
		vadMaxInferNs   int64
	}

	// Снапшот sentBytes для мгновенного kbps между последовательными
	// вызовами GetVoiceBitrate. Первый вызов возвращает avg за всю сессию.
	prevSentBytes int64
	prevSentTs    time.Time

	// Opus DTX (discontinuous transmission): энкодер не emit'ит пакеты на
	// полной тишине, экономит ~70% трафика. Применяется в JoinGroupVoice
	// после создания энкодера, hot-swap через SetOpusDTX во время сессии.
	opusDTX bool

	// --- VAD (Voice Activity Detection) — Silero v5 поверх Opus ---
	// Этап 3 трёхфазного rollout'а экономии трафика (Этап 1 baseline,
	// Этап 2 DTX — признан непригоден, Этап 3 — текущий). Гейтит своИ
	// исходящие фреймы, с pre-roll 100 ms и hangover 400 ms чтобы не
	// резать начала/концы слов. Все поля под s.mu, мутируются только
	// из audioLoop после snapshot и из UI-треда в SetVADEnabled.
	vadEnabled      bool
	vadThreshold    float32      // speech_prob >= threshold → is_speech. Default 0.35
	vadDetector     vad.Detector // nil пока не включали в текущей сессии
	vadRingBuf      [vad.WindowSize * 2]int16 // до 1024 sample: 512-окно + остаток после сдвига
	vadRingLen      int
	vadPreRoll      [vadPreRollSize][]byte // ring из последних 5 закодированных NXV-фреймов
	vadPreRollHead  int                    // write index
	vadPreRollCount int                    // 0..5
	vadHangover     int                    // декрементится каждый audioLoop tick
	vadLastProb     float32                // последний speech_prob для дампа

	// --- NS A/B statistics CSV log (debug build only) ---
	// Открывается в JoinGroupVoice (если voiceDebug=true), пишется каждые 200ms
	// из audioLoop, закрывается в leaveInternal. Файл рядом с vox-app.log,
	// ротация .1..4 (5 звонков max). См. voice_ns_stats.go.
	nsStatsLog       *os.File
	nsStatsLogStart  time.Time

	// --- SFU (Selective Forwarding Unit) ---
	// Все поля под s.mu. При sfuEnabled=false — все nil/zero, код SFU не выполняется.
	sfuEnabled   bool              // user setting: использовать SFU для больших групп
	sfuForce     bool              // принудительный SFU (для тестирования, игнорирует threshold)
	sfuThreshold int               // порог участников для переключения (default 4)
	sfuEndpoint  string            // от signal server auth_ok
	sfuSecret    string            // HMAC shared secret от signal server
	sfuConn      *sfuConn          // nil когда не подключен к SFU
	sfuActive    bool              // true = отправка идёт через SFU (не mesh)
	sfuPeerHash  map[string]uint32 // peerID → hash (для отправки)
	sfuHashPeer  map[uint32]string // hash → peerID (для приёма)
}

// peerSeqState — состояние последнего принятого фрейма per-peer для FEC/PLC
// recovery. hasLast = false до первого пакета.
type peerSeqState struct {
	lastSeq uint16
	hasLast bool
}

// maxJitterFrames — верхняя граница mixBuf per-peer. 8 × 20 ms = 160 ms — покрывает
// типичный jitter через TURN, выше обрезаем (дропаем старые фреймы). До фикса
// очередь росла до 16+ кадров (320+ ms), что накапливало latency на каждый burst.
// Opus PLC восстановит дропы ≤ 5 фреймов без явных glitches.
const maxJitterFrames = 8

// mixTargetFrames — целевая глубина очереди mixer'а per-peer (60 ms при
// 20-ms фреймах). audioLoop drop'ит старейшие кадры если очередь распухла
// выше: в normal flow recv ≈ play (50/sec), очередь стационарна на 1-2,
// но после single блокирующего max_recv пика она может скопить 5-8 кадров
// которые никогда не разгрузятся — постоянная задержка для этого peer'а
// и рассинхрон с другими. См. voice_loop.go adaptive catch-up.
//
// Меньше — короче latency но больше PLC-fill при сетевом jitter.
// Больше — глаже звук но толерантнее к накоплению лагов.
// 3 кадра = 60 ms — компромисс между Discord-уровнем latency и стабильностью.
const mixTargetFrames = 3

// speakingRMSThreshold — порог RMS (на 16-bit int16 шкале) для детекции
// "этот пир сейчас говорит". Используется только для UI-индикатора (подсветка
// аватара), не влияет на дроп/отправку фреймов. Ниже порога → speaking=false
// с decay 15 frames (300ms), выше → speaking=true.
const speakingRMSThreshold = 500.0

// VAD tunables. Safety nets — обязательны по требованию: никогда не резать
// начала слов или тихие согласные (см. feedback_vad_no_gate.md).
const (
	vadPreRollSize    = 5  // 5 фреймов × 20 ms = 100 ms pre-roll
	vadHangoverFrames = 20 // 20 фреймов × 20 ms = 400 ms hangover
	vadDefaultThresh  = 0.35
)

// Wire-протоколы VOX1/DFNP вынесены в voice_protocol.go.

func NewVoiceService(log *slog.Logger, store *storage.Storage, sess *session.Session, network *NetworkService, groups *GroupsService) *VoiceService {
	s := &VoiceService{
		log:          log,
		store:        store,
		sess:         sess,
		network:      network,
		groups:       groups,
		participants:  make(map[string]bool),
		channelPeers: make(map[string]map[string]bool),
		peerVolumes:  make(map[string]float32),
		masterVoiceVolume: 1.0,
		peerSpeaking: make(map[string]bool),
		speakDecay:   make(map[string]int),
		peerLastFrame: make(map[string]time.Time),
		opusDecoders:  make(map[string]*codec.Decoder),
		peerRxSeq:     make(map[string]peerSeqState),
		mixBuf:        make(map[string][][]byte),
		mixBufTs:      make(map[string][]int64),
		sfuThreshold:  4, // default: SFU при 4+ участниках
		vadThreshold:  vadDefaultThresh,
		// По итогам тестирования (2026-04-22): default = GTCRN/DNS3 как
		// пост-обработка поверх DFN. Цепочка: C ns (Deep/DFN) → audioLoop →
		// gtcrn.Process. DFN снимает основной шум, GTCRN сглаживает остаточные
		// артефакты речи. CPU нагрузка GTCRN <1% на любом железе.
		// Существующие установки сохраняют свой выбор (LoadNeuralNSSettings
		// перекрывает default из БД).
		neuralMode:    "gtcrn",
		feSize:        audio.FEBase,
		feMix:         1.0,
		gtcrnVariant:  audio.GTCRNDNS3,
		gtcrnMix:      1.0,
		feCache:       make(map[audio.FastEnhancerSize]*audio.FastEnhancer),
		gtcrnCache:    make(map[audio.GTCRNVariant]*audio.GTCRN),
	}
	s.loadMasterVoiceVolume()
	return s
}

// Live NS controls (ApplyNoiseSuppression, ApplyDFNStrength) вынесены в voice_ns.go.

// InitVoiceCallbacks регистрирует обработчик входящих voice фреймов
func (s *VoiceService) InitVoiceCallbacks() {
	s.network.SetOnVoiceFrame(s.handleVoiceFrame)
	s.network.SetOnVoicePeerConnected(s.onPeerConnected)
	s.network.SetOnVoicePeerDisconnected(s.onPeerDisconnected)
}

// onPeerConnected — при подключении пира: если мы в голосовом канале, отправляем voice_join
// Не проверяем group_members — источник истины для voice это activeGroupID + channelPeers.
// Другая сторона сама отфильтрует по msg.GroupID == s.activeGroupID.
func (s *VoiceService) onPeerConnected(pubKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.activeGroupID == "" {
		return
	}

	myID, _ := s.sess.PeerID()
	msg := P2PMessage{
		Type:     "voice_join",
		GroupID:  s.activeGroupID,
		Content:  myID,
		Nickname: s.network.MyNickname(),
	}
	data, _ := json.Marshal(msg)
	if err := s.network.SendP2PData(pubKey, data); err != nil {
		s.log.Debug("🎧 RE-ANNOUNCE voice skipped (DC not ready)",
			slog.String("peer", safeShort(pubKey)),
			slog.String("group", safeShort8(s.activeGroupID)),
			slog.String("error", err.Error()))
		return
	}
	s.log.Info("🎧 RE-ANNOUNCE voice to reconnected peer",
		slog.String("peer", safeShort(pubKey)),
		slog.String("group", safeShort8(s.activeGroupID)))
}

// onPeerDisconnected — очистка при разрыве P2P соединения
func (s *VoiceService) onPeerDisconnected(pubKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	wasParticipant := s.participants[pubKey]
	delete(s.participants, pubKey)

	// Cleanup opus decoder + rx seq state
	if dec, ok := s.opusDecoders[pubKey]; ok {
		dec.Close()
		delete(s.opusDecoders, pubKey)
	}
	delete(s.peerRxSeq, pubKey)

	// Удаляем из глобального трекинга channelPeers
	for groupID, peers := range s.channelPeers {
		if peers[pubKey] {
			delete(peers, pubKey)
			if len(peers) == 0 {
				delete(s.channelPeers, groupID)
			}
		}
	}

	delete(s.peerVolumes, pubKey)
	delete(s.peerSpeaking, pubKey)
	delete(s.speakDecay, pubKey)

	if wasParticipant {
		s.log.Info("🎧 пир отключился, удалён из голосового канала",
			slog.String("peer", safeShort(pubKey)))
		nick := s.network.PeerNickname(pubKey)
		s.network.EmitEvent("vox:voice-leave", map[string]any{
			"peerId":   pubKey,
			"groupId":  s.activeGroupID,
			"nickname": nick,
		})
	}
}

// --- Голосовые сообщения ---

// GetVoiceData возвращает base64-encoded WAV аудио по ID сообщения
func (s *VoiceService) GetVoiceData(msgID string) (string, error) {
	const op = "VoiceService.GetVoiceData"

	var audioData []byte
	err := s.store.DB().QueryRow(
		"SELECT audio_data FROM messages WHERE id = ? AND type = 'voice'", msgID).Scan(&audioData)
	if err != nil || len(audioData) == 0 {
		err = s.store.DB().QueryRow(
			"SELECT audio_data FROM group_messages WHERE id = ? AND type = 'voice'", msgID).Scan(&audioData)
	}
	if err != nil || len(audioData) == 0 {
		return "", fmt.Errorf("%s: аудио не найдено для msg %s", op, msgID)
	}

	s.log.Debug("🔊 voice data загружено",
		slog.String("msg_id", msgID),
		slog.Int("size", len(audioData)))

	return base64.StdEncoding.EncodeToString(audioData), nil
}

// --- Голосовые каналы ---

// PresetActiveGroup устанавливает activeGroupID заранее,
// чтобы onPeerConnected мог отправить voice_join до завершения JoinGroupVoice.
func (s *VoiceService) PresetActiveGroup(groupID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeGroupID = groupID
}

// JoinGroupVoice подключается к голосовому каналу группы
func (s *VoiceService) JoinGroupVoice(groupID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Повторный Join в ТУ ЖЕ группу при живой сессии — no-op. Иначе код ниже
	// пересоздал бы capture/stopCh/audioLoop поверх работающих: старый audioLoop
	// не получал stop (оба уходили в новый stopCh), два цикла делили один
	// StreamRead и слали звук «пополам» + дублировали seq.
	if s.activeGroupID == groupID && s.capture != nil {
		return nil
	}

	if s.activeGroupID != "" && s.activeGroupID != groupID {
		s.leaveInternal()
	}

	myID, err := s.sess.PeerID()
	if err != nil {
		return err
	}

	// Проверяем что voice включён
	var voiceEnabled int
	s.store.DB().QueryRow("SELECT voice_enabled FROM groups WHERE group_id = ?", groupID).Scan(&voiceEnabled)
	if voiceEnabled == 0 {
		return fmt.Errorf("голосовой канал отключён")
	}

	// Запускаем capture
	s.capture = audio.NewCapture()
	var micIdx int = -1
	var micStr string
	s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'microphone'").Scan(&micStr)
	if micStr != "" {
		fmt.Sscanf(micStr, "%d", &micIdx)
	}
	if err := s.capture.Start(micIdx); err != nil {
		s.capture = nil
		return fmt.Errorf("не удалось запустить микрофон: %w", err)
	}
	s.capture.StreamStart()

	// Применяем сохранённый gain
	micGain := 1.0
	var gainStr string
	s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'mic_gain'").Scan(&gainStr)
	if g, err := strconv.ParseFloat(gainStr, 32); err == nil && g > 0 {
		micGain = g
		s.capture.SetGain(float32(g))
	}

	// Применяем уровень шумоподавления. Приоритет дефолта: DFN3 > RNN > Light.
	// См. defaultNSLevel в services/audio.go (держать синхронно с ним).
	nsLevel := audio.NSLight
	if audio.DFNAvailable() {
		nsLevel = audio.NSDeep
	} else if audio.RNNoiseAvailable() {
		nsLevel = audio.NSRNN
	}
	var nsStr string
	s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'noise_suppression'").Scan(&nsStr)
	if nsStr != "" {
		if n, err := strconv.Atoi(nsStr); err == nil && n >= 0 && n <= int(audio.NSDeep) {
			nsLevel = audio.NoiseSuppressionLevel(n)
		}
	}
	// Читаем сохранённую силу DFN3 (проценты 10..100, маппится 1:1 в atten_lim dB).
	// Дефолт 100% = unlimited DFN3 (native libDF поведение, речь не портится).
	dfnStrength := float32(100.0)
	var dsStr string
	s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'dfn_strength'").Scan(&dsStr)
	if dsStr != "" {
		if n, err := strconv.Atoi(dsStr); err == nil && n >= 10 && n <= 100 {
			dfnStrength = float32(n)
		}
	}

	if nsLevel == audio.NSDeep && !s.capture.DFModelLoaded() {
		// Async-загрузка чтобы не блокировать JoinGroupVoice на ~450 мс.
		// До завершения level=NSDeep делает RNN-fallback в C callback.
		// После загрузки сразу применяем сохранённую dfnStrength к свежей модели
		// (df_create имеет свой дефолт 30 dB, но settings может быть другим).
		captureRef := s.capture
		strengthRef := dfnStrength
		s.dfnLoadWG.Add(1)
		go func() {
			defer s.dfnLoadWG.Done()
			start := time.Now()
			loadDFModelIfPossible(s.log, captureRef)
			if captureRef.DFModelLoaded() {
				captureRef.SetDFAttenLim(strengthRef)
			}
			s.log.Info("DFN3 async load complete (on join)",
				slog.Duration("took", time.Since(start)),
				slog.Bool("dfn_loaded", captureRef.DFModelLoaded()),
				slog.Float64("strength_db", float64(strengthRef)))
		}()
	} else if nsLevel == audio.NSDeep && s.capture.DFModelLoaded() {
		// Модель уже загружена (переход между каналами) — обновляем strength сразу.
		s.capture.SetDFAttenLim(dfnStrength)
	}
	s.capture.SetNoiseSuppression(nsLevel)

	s.log.Info("voice capture started",
		slog.Int("mic", micIdx),
		slog.Float64("mic_gain", micGain),
		slog.Int("ns_level", int(nsLevel)),
		slog.Bool("rnnoise_available", audio.RNNoiseAvailable()),
		slog.Bool("dfn_available", audio.DFNAvailable()),
		slog.Bool("dfn_loaded", s.capture.DFModelLoaded()))

	// Запускаем playback с сохранённым speaker (или default если не выбран)
	spkIdx := -1
	var spkStr string
	if err := s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'speaker'").Scan(&spkStr); err != nil && err != sql.ErrNoRows {
		s.log.Warn("speaker setting query failed", slog.String("error", err.Error()))
	}
	if spkStr != "" {
		if n, err := strconv.Atoi(spkStr); err == nil {
			spkIdx = n
		}
	}
	s.playback = audio.NewPlayback()
	if err := s.playback.Start(spkIdx); err != nil {
		s.log.Warn("не удалось запустить динамик",
			slog.Int("device_idx", spkIdx),
			slog.String("error", err.Error()))
	} else {
		// Pre-fill ring buffer 40ms silence (2 фрейма). Создаёт headroom
		// между writer (audioLoop, 320 samples/20ms tick) и reader (miniaudio
		// callback, 16000 samples/sec real-time) — без него gap стартует с
		// нуля и любой ticker jitter (Windows timer resolution ~15ms) пропускает
		// один tick → reader читает silence из underrun.
		// 40ms = 1 tick budget + ticker jitter headroom. Больший pre-fill =
		// больше mouth-to-ear latency во всех сценариях (включая TURN relay,
		// где уже есть RTT 100-400ms). 40ms — минимум при котором живёт direct
		// LAN. Spike'и от тяжёлого NS отдельно отфильтрованы hard-budget
		// bypass'ом в audio_gtcrn.go / audio_fastenhancer.go.
		silenceFrame := make([]byte, codec.FrameSize*2)
		for range 2 {
			s.playback.Write(silenceFrame)
		}
		s.log.Info("voice playback started", slog.Int("device_idx", spkIdx))
	}

	s.activeGroupID = groupID
	s.muted = false
	s.stopCh = make(chan struct{})

	// Запоминаем выбранные idx чтобы при default device change знать —
	// тригерить ли Restart (idx==-1) или нет (юзер выбрал конкретное).
	s.watchedMicIdx = micIdx
	s.watchedSpkIdx = spkIdx
	s.audioWatcher = audio.NewDefaultDeviceWatcher(2 * time.Second)
	go s.handleDeviceChanges(s.audioWatcher, s.stopCh)

	// NS A/B stats CSV log — diagnostic only (voiceDebug build tag).
	// Открываем ПОСЛЕ установки активной группы и nsLevel — чтобы файл
	// существовал к моменту первой записи из audioLoop.
	if voiceDebug {
		s.nsStatsLog, s.nsStatsLogStart = openNSStatsLog(s.log, nsLevel)
	}

	// Opus encoder для сжатия голоса (~10x: 640 bytes PCM → ~60 bytes Opus)
	if enc, err := codec.NewEncoder(); err != nil {
		s.log.Warn("opus encoder failed, sending raw PCM", slog.String("error", err.Error()))
	} else {
		s.opusEncoder = enc
		// Применяем сохранённый DTX-флаг. LoadOpusSettings() на старте кладёт
		// значение в s.opusDTX, здесь только передаём в кодек.
		if s.opusDTX {
			if err := enc.SetDTX(true); err != nil {
				s.log.Warn("opus DTX enable failed", slog.String("error", err.Error()))
			} else {
				s.log.Info("opus DTX enabled")
			}
		}
	}
	s.prof = struct {
		captureFrames   int64
		sentFrames      int64
		sendErrors      int64
		recvFrames      int64
		playbackWrites  int64
		playbackSilence int64
		dropNoPeer      int64
		dropMuted       int64
		maxLoopTime     time.Duration
		maxRecvTime     time.Duration
		lastRecvJitter  float64
		recvIntervals   []float64
		lastRecvTime    time.Time
		sessionStart    time.Time
		decodeErrors    int64
		seqSkips        int64
		latProbeCount   int64
		latProbeSumNs   int64
		latProbeMaxNs   int64
		latProbeMinNs   int64
		mixDwellCount    int64
		mixDwellSumNs    int64
		mixDwellMaxNs    int64
		mixQueuePeak     int
		mixOverflowDrops int
		sfuSentFrames    int64
		sfuRecvFrames   int64
		sentBytes       int64
		vadSkipped      int64
		vadInferenceErr int64
		vadMaxInferNs   int64
	}{sessionStart: time.Now(), latProbeMinNs: -1}
	s.prevSentBytes = 0
	s.prevSentTs = time.Now()
	// Reset VAD state для новой сессии (pre-roll ring, ring-buf, hangover).
	// Сам детектор остаётся живым между сессиями (lazy-create в SetVADEnabled).
	s.vadRingLen = 0
	s.vadPreRollHead = 0
	s.vadPreRollCount = 0
	s.vadHangover = 0
	s.vadLastProb = 0
	if s.vadDetector != nil {
		s.vadDetector.Reset()
	}

	// Подтягиваем уже известных участников из channelPeers (кроме себя)
	for peerID := range s.channelPeers[groupID] {
		if peerID != myID {
			s.participants[peerID] = true
		}
	}

	// Auto-connect ко всем участникам группы (не-друзья получат CallOnly P2P)
	if s.groups != nil {
		members := s.groups.GetGroupMemberIDs(groupID)
		for _, pid := range members {
			if pid != myID {
				s.network.ConnectToGroupPeer(pid)
			}
		}

		// Broadcast voice_join всем подключённым участникам
		msg := P2PMessage{
			Type:     "voice_join",
			GroupID:  groupID,
			Content:  myID,
			Nickname: s.network.MyNickname(),
		}
		data, _ := json.Marshal(msg)
		var sentTo, offlineMembers int
		for _, pid := range members {
			if pid == myID {
				continue
			}
			if s.network.IsP2PConnected(pid) {
				if err := s.network.SendP2PData(pid, data); err != nil {
					s.log.Warn("🎧 voice_join broadcast FAILED",
						slog.String("peer", safeShort(pid)),
						slog.String("error", err.Error()))
				} else {
					sentTo++
				}
			} else {
				offlineMembers++
			}
		}
		s.log.Info("🎧 voice_join broadcast",
			slog.String("group", safeShort8(groupID)),
			slog.Int("members", len(members)),
			slog.Int("sent_to", sentTo),
			slog.Int("connecting", offlineMembers))
	}

	// VAD: если включён и модель ещё не загружена — загружаем асинхронно.
	// До завершения загрузки audioLoop работает в pass-through режиме
	// (vadDetector == nil → ни один фрейм не дропается).
	if s.vadEnabled && s.vadDetector == nil {
		go s.loadVADDetectorAsync()
	}

	// Neural NS: prewarm уже сделан при старте сервиса (LoadNeuralNSSettings),
	// поэтому активация любой модели здесь — мгновенный pointer swap без
	// блокировки ORT runtime. Если кэш ещё не успел дозагрузиться (race) —
	// стартуем async load с fallback'ом на bypass до готовности.
	if audio.FastEnhancerAvailable() && s.capture != nil {
		switch s.neuralMode {
		case "fe":
			if s.fastEnhancer == nil {
				s.startFastEnhancerLoadLocked()
			}
		case "gtcrn":
			if s.gtcrn == nil {
				s.startGTCRNLoadLocked()
			}
		}
	}

	// Запускаем audio loop. audioWG учитывает audioLoop + мониторы —
	// leaveInternal ждёт их выхода перед освобождением capture/playback.
	s.audioWG.Add(1)
	go s.audioLoop()

	// Bitrate monitor: 2s push-event для UI-readout (Settings → Voice).
	// Раньше был setInterval(2000) на фронте, что грузило bridge даже когда
	// вкладка закрыта. Push с бэка живёт только пока есть voice-сессия.
	s.audioWG.Add(1)
	go s.bitrateMonitor()

	// DFN perf-monitor: раз в 5 секунд снимает счётчики и логирует среднее/
	// max время инференции + over_budget_10ms. Только в debug-сборке
	// (nx_voicedebug), иначе compile-time eliminated.
	if voiceDebug && nsLevel == audio.NSDeep {
		s.audioWG.Add(1)
		go s.dfnPerfMonitor()
	}

	s.network.emitEvent("vox:voice-join", map[string]string{
		"groupId": groupID,
		"peerId":  myID,
	})

	// SFU: подключиться если включён и достаточно участников (или force)
	if s.sfuEnabled && s.sfuEndpoint != "" {
		participantCount := len(s.participants) + 1 // +1 за себя
		if s.sfuForce || participantCount >= s.sfuThreshold {
			go s.connectSFU(groupID)
		}
	}

	s.log.Info("🎧 VOICE JOIN", slog.String("group", safeShort8(groupID)))
	return nil
}

// bitrateMonitor — push-event фронту с актуальным трафиком voice-сессии.
// Живёт пока есть activeGroupID; завершается через захваченный stopCh.
// Заменил собой setInterval(2000) в settings.js: фронт теперь только слушает
// vox:voice-bitrate и не нагружает Wails-bridge когда вкладка закрыта.
func (s *VoiceService) bitrateMonitor() {
	defer s.audioWG.Done()
	// Захватываем stopCh ОДИН раз. leaveInternal обнуляет s.stopCh,
	// но закрытие захваченного канала разблокирует наш select.
	s.mu.Lock()
	stop := s.stopCh
	s.mu.Unlock()
	if stop == nil {
		return
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			br, err := s.GetVoiceBitrate()
			if err != nil || !br.Active {
				continue
			}
			s.network.emitEvent("vox:voice-bitrate", map[string]any{
				"active":      br.Active,
				"totalBytes":  br.TotalBytes,
				"instantKbps": br.InstantKbps,
				"avgKbps":     br.AvgKbps,
				"sessionSec":  br.SessionSec,
			})
		}
	}
}

// dfnPerfMonitor — периодически считывает stats DFN3 процессинга и пишет в лог.
// Помогает диагностировать RT-бюджет: avg должен быть < 1 мс, max < 10 мс,
// over_budget == 0. Если over_budget растёт — DFN не успевает, накапливается
// latency.
func (s *VoiceService) dfnPerfMonitor() {
	defer s.audioWG.Done()
	// Захватываем stopCh один раз под локом (как bitrateMonitor): чтение
	// s.stopCh без лока гонялось с обнулением в leaveInternal.
	s.mu.Lock()
	stop := s.stopCh
	s.mu.Unlock()
	if stop == nil {
		return
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			// Доступ к capture под локом: leaveInternal ждёт audioWG перед
			// Close(), но подстрахуемся снапшотом под тем же mutex.
			s.mu.Lock()
			cap := s.capture
			s.mu.Unlock()
			if cap == nil {
				return
			}
			st := cap.TakeDFNStats()
			if st.Frames == 0 {
				continue
			}
			s.log.Info("dfn perf",
				slog.Int64("frames", st.Frames),
				slog.Float64("avg_us", float64(st.AvgNs)/1000),
				slog.Float64("max_us", float64(st.MaxNs)/1000),
				slog.Int64("over_budget_10ms", st.OverBudget))
		}
	}
}

// LeaveGroupVoice отключается от голосового канала
func (s *VoiceService) LeaveGroupVoice() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leaveInternal()
	return nil
}

// leaveInternal — нормальный teardown голосовой сессии. Вызывается с s.mu
// удержанным. Ждёт выхода audioLoop/мониторов ПЕРЕД освобождением C-ресурсов.
func (s *VoiceService) leaveInternal() {
	s.leaveCommon(true)
}

// leaveInternalNoWait — teardown из паники audioLoop. audioLoop уже сделал
// audioWG.Done() и является вызывающей горутиной, поэтому audioWG.Wait() всё
// равно безопасен (ждёт только мониторы) — но имя оставлено явным для ясности.
func (s *VoiceService) leaveInternalNoWait() {
	s.leaveCommon(true)
}

// leaveCommon выполняет полный teardown по паттерну «snapshot под локом →
// освобождение C-heap после Wait»:
//   1. Отвязываем capture/playback/encoder/… в локальные переменные и сбрасываем
//      состояние сессии (activeGroupID="") ЕЩЁ ПОД ЛОКОМ — конкурентный
//      JoinGroupVoice сразу видит чистый слот и не гоняется за нашими ресурсами.
//   2. Отпускаем лок, ждём audioWG (audioLoop + мониторы) и in-flight загрузки
//      моделей — только после этого никто не читает старый capture/playback.
//   3. Освобождаем C-ресурсы из локальных переменных (StreamStop/Stop/Close).
// Это устраняло: утечку 57.6 МБ C-heap на каждый leave (Close не звался вообще),
// UAF playback (pb.Write из audioLoop в уже freed буфер) и гонку teardown с
// повторным Join.
func (s *VoiceService) leaveCommon(waitLoop bool) {
	if s.activeGroupID == "" {
		return
	}

	// SFU: отключиться если подключены
	if s.sfuConn != nil {
		conn := s.sfuConn
		s.sfuConn = nil
		s.sfuActive = false
		s.sfuPeerHash = nil
		s.sfuHashPeer = nil
		go conn.close()
	}

	myID, _ := s.sess.PeerID()
	groupID := s.activeGroupID

	// Neural NS: только обнуляем активные указатели — кэш живёт всю жизнь
	// процесса (прогрет в LoadNeuralNSSettings). Кэш Close'ится в Shutdown.
	s.fastEnhancer = nil
	s.gtcrn = nil

	// Сигнал audioLoop/мониторам завершиться.
	if s.stopCh != nil {
		close(s.stopCh)
		s.stopCh = nil
	}

	// (1) Отвязываем ресурсы в локальные переменные и чистим состояние — под локом.
	capToStop := s.capture
	pbToStop := s.playback
	watcherToStop := s.audioWatcher
	encToClose := s.opusEncoder
	decodersToClose := s.opusDecoders
	vadToClose := s.vadDetector
	nsLogToClose := s.nsStatsLog

	s.capture = nil
	s.playback = nil
	s.audioWatcher = nil
	s.opusEncoder = nil
	s.opusDecoders = make(map[string]*codec.Decoder)
	s.vadDetector = nil
	s.nsStatsLog = nil

	for i := range s.vadPreRoll {
		s.vadPreRoll[i] = nil
	}
	s.vadRingLen = 0
	s.vadPreRollHead = 0
	s.vadPreRollCount = 0
	s.vadHangover = 0
	s.peerRxSeq = make(map[string]peerSeqState)
	s.mixBuf = make(map[string][][]byte)
	s.mixBufTs = make(map[string][]int64)

	s.activeGroupID = ""
	s.participants = make(map[string]bool)
	s.peerSpeaking = make(map[string]bool)
	s.speakDecay = make(map[string]int)
	s.peerLastFrame = make(map[string]time.Time)
	s.muted = false
	s.deafened = false
	s.mutedBeforeDeafen = false

	// (2) Ждём выхода audioLoop/мониторов и in-flight загрузок моделей вне лока.
	// Только после этого гарантированно никто не держит capToStop/pbToStop.
	if waitLoop {
		s.mu.Unlock()
		s.audioWG.Wait()
		s.dfnLoadWG.Wait()
		s.feLoadWG.Wait()
		s.mu.Lock()
	}

	// (3) Освобождаем C-ресурсы из локальных переменных.
	if capToStop != nil {
		capToStop.StreamStop()
		capToStop.Stop()
		capToStop.Close() // ← nx_capture_free: освобождает rec_buf (57.6 МБ) + ns_df
	}
	if pbToStop != nil {
		pbToStop.Stop()
		pbToStop.Close()
	}
	if watcherToStop != nil {
		watcherToStop.Stop()
	}
	if encToClose != nil {
		encToClose.Close()
	}
	for _, dec := range decodersToClose {
		dec.Close()
	}
	if vadToClose != nil {
		vadToClose.Close()
	}
	if nsLogToClose != nil {
		closeNSStatsLog(nsLogToClose)
	}

	// Broadcast voice_leave
	if s.groups != nil {
		members := s.groups.GetGroupMemberIDs(groupID)
		msg := P2PMessage{
			Type:    "voice_leave",
			GroupID: groupID,
			Content: myID,
		}
		data, _ := json.Marshal(msg)
		for _, pid := range members {
			if pid != myID && s.network.IsP2PConnected(pid) {
				s.network.SendP2PData(pid, data)
			}
		}
	}

	s.network.emitEvent("vox:voice-leave", map[string]string{
		"groupId": groupID,
		"peerId":  myID,
	})

	// Финальный idle-snapshot: settings.applyBitrateSnapshot покажет "—",
	// иначе UI зависнет на последнем числе после выхода из канала.
	s.network.emitEvent("vox:voice-bitrate", map[string]any{"active": false})

	s.log.Info("🎧 VOICE LEAVE", slog.String("group", safeShort8(groupID)))
}

// Shutdown корректно завершает голосовой сервис при выходе из приложения
func (s *VoiceService) Shutdown() {
	s.mu.Lock()
	s.log.Info("завершение VoiceService")
	s.leaveInternal()

	// Neural NS: жизненный цикл кэша = вся жизнь процесса. Close моделей
	// делается ТОЛЬКО на Shutdown (а не на каждом leaveInternal). Перед Close
	// ждём in-flight prewarm goroutine — иначе race с ORT runtime ресурсами.
	feCacheSnap := s.feCache
	gtcrnCacheSnap := s.gtcrnCache
	s.feCache = make(map[audio.FastEnhancerSize]*audio.FastEnhancer)
	s.gtcrnCache = make(map[audio.GTCRNVariant]*audio.GTCRN)
	s.feLoadGen++
	s.mu.Unlock()
	s.feLoadWG.Wait()
	for _, fe := range feCacheSnap {
		if fe != nil {
			fe.Close()
		}
	}
	for _, g := range gtcrnCacheSnap {
		if g != nil {
			g.Close()
		}
	}
}

// VoicePeerIDs — пиры (без нас), которые сейчас в голосовом канале groupID.
// Дёшево (без БД): вызывается ScreenShareService на каждый кадр демонстрации,
// чтобы слать экран только тем, кто в канале, а не всем участникам группы.
func (s *VoiceService) VoicePeerIDs(groupID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	peers := s.channelPeers[groupID]
	if len(peers) == 0 {
		return nil
	}
	out := make([]string, 0, len(peers))
	for peerID := range peers {
		out = append(out, peerID)
	}
	return out
}

// GetVoiceParticipants возвращает список участников голосового канала
func (s *VoiceService) GetVoiceParticipants(groupID string) ([]VoiceParticipant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var result []VoiceParticipant
	added := make(map[string]bool)

	// Добавляем себя если мы в канале
	if s.activeGroupID == groupID {
		myID, _ := s.sess.PeerID()
		result = append(result, VoiceParticipant{
			PeerID:   myID,
			Nickname: s.network.MyNickname(),
			Muted:    s.muted,
		})
		added[myID] = true
	}

	// Добавляем пиров из глобального трекинга channelPeers
	for peerID := range s.channelPeers[groupID] {
		if added[peerID] {
			continue
		}
		nick := s.network.PeerNickname(peerID)
		result = append(result, VoiceParticipant{
			PeerID:   peerID,
			Nickname: nick,
			Speaking: s.peerSpeaking[peerID],
		})
		added[peerID] = true
	}

	return result, nil
}

// GetActiveVoiceGroup возвращает ID активного голосового канала
func (s *VoiceService) GetActiveVoiceGroup() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeGroupID, nil
}

// Mute/Deafen API вынесены в voice_state.go.

// broadcastVoiceStatus отправляет voice_status всем участникам текущего канала
func (s *VoiceService) broadcastVoiceStatus() {
	s.mu.Lock()
	groupID := s.activeGroupID
	muted := s.muted
	deafened := s.deafened
	s.mu.Unlock()

	if groupID == "" {
		return
	}

	myID, err := s.sess.PeerID()
	if err != nil {
		return
	}

	status := P2PMessage{
		Type:    "voice_status",
		GroupID: groupID,
		Content: fmt.Sprintf(`{"muted":%t,"deafened":%t}`, muted, deafened),
	}
	data, _ := json.Marshal(status)

	s.mu.Lock()
	peers := make([]string, 0, len(s.participants))
	for pid := range s.participants {
		if pid != myID {
			peers = append(peers, pid)
		}
	}
	s.mu.Unlock()

	for _, pid := range peers {
		s.network.SendP2PData(pid, data)
	}
}

// SetPeerVolume устанавливает громкость конкретного пира (0.0-1.4)
func (s *VoiceService) SetPeerVolume(peerID string, volume float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.peerVolumes[peerID] = float32(volume)
	return nil
}

// SetMasterVoiceVolume устанавливает мастер-громкость для всех голосов собеседников.
// Множится поверх per-peer volume в handleVoiceFrame. Диапазон 0.0-1.4.
// Сохраняется в settings table и подгружается на старте.
func (s *VoiceService) SetMasterVoiceVolume(volume float64) error {
	if volume < 0 {
		volume = 0
	}
	if volume > 1.4 {
		volume = 1.4
	}
	s.mu.Lock()
	s.masterVoiceVolume = float32(volume)
	s.mu.Unlock()
	if s.store != nil {
		_, err := s.store.DB().Exec(
			"INSERT OR REPLACE INTO settings (key, value) VALUES ('master_voice_volume', ?)",
			fmt.Sprintf("%.4f", volume))
		if err != nil {
			s.log.Warn("master voice volume save failed", slog.String("error", err.Error()))
		}
	}
	return nil
}

// GetMasterVoiceVolume возвращает текущую мастер-громкость голосов (0.0-1.4).
func (s *VoiceService) GetMasterVoiceVolume() (float64, error) {
	s.mu.Lock()
	v := s.masterVoiceVolume
	s.mu.Unlock()
	return float64(v), nil
}

// loadMasterVoiceVolume подгружает сохранённое значение из settings (вызывается при старте).
func (s *VoiceService) loadMasterVoiceVolume() {
	if s.store == nil {
		return
	}
	var raw string
	err := s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'master_voice_volume'").Scan(&raw)
	if err != nil {
		return
	}
	v, perr := strconv.ParseFloat(raw, 32)
	if perr != nil || v < 0 || v > 1.4 {
		return
	}
	s.mu.Lock()
	s.masterVoiceVolume = float32(v)
	s.mu.Unlock()
	s.log.Info("master voice volume loaded", slog.Float64("volume", v))
}

// MutePeer полностью заглушает пира
func (s *VoiceService) MutePeer(peerID string) error {
	return s.SetPeerVolume(peerID, 0)
}

// audioLoop вынесен в voice_loop.go.

// handleVoiceFrame + voiceFramesReceived вынесены в voice_inbound.go.

// --- P2P voice join/leave handling ---

// HandleVoiceP2P обрабатывает voice_join / voice_leave от пиров
// HandleVoiceP2P + handleVoiceJoinLocked / handleVoiceLeaveLocked / handleVoiceStatusLocked /
// handleVoiceMembersLocked — вынесены в voice_p2p.go.
