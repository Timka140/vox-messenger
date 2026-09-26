package services

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"vox_app/internal/audio"
	"vox_app/internal/session"
	"vox_app/internal/storage"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
)

// AudioService — запись и отправка голосовых сообщений через miniaudio
type AudioService struct {
	log     *slog.Logger
	sess    *session.Session
	store   *storage.Storage
	network *NetworkService
	groups  *GroupsService
	voice   *VoiceService // back-ref для проксирования NS на live voice capture

	mu      sync.Mutex
	capture *audio.Capture

	// Проверка микрофона и «Слушать себя» (audio_monitor.go).
	micTesting bool
	monitorOn  bool
	monCap     *audio.Capture
	monPB      *audio.Playback
	monStop    chan struct{}
	monDone    chan struct{}
}

// NewAudioService конструирует сервис записи/отправки голосовых сообщений.
// voice — для проксирования NS-изменений из UI Settings на live voice capture.
// groups — для отправки групповых голосовых сообщений.
func NewAudioService(log *slog.Logger, sess *session.Session, store *storage.Storage, network *NetworkService, voice *VoiceService, groups *GroupsService) *AudioService {
	return &AudioService{
		log:     log,
		sess:    sess,
		store:   store,
		network: network,
		voice:   voice,
		groups:  groups,
	}
}

// --- Device enumeration ---

func (s *AudioService) GetMicrophones() ([]audio.DeviceInfo, error) {
	return audio.ListDevices(audio.DeviceCapture), nil
}

func (s *AudioService) GetSpeakers() ([]audio.DeviceInfo, error) {
	return audio.ListDevices(audio.DevicePlayback), nil
}

func (s *AudioService) SetMicrophone(idx int) error {
	_, err := s.store.DB().Exec(
		"INSERT OR REPLACE INTO settings (key, value) VALUES ('microphone', ?)", strconv.Itoa(idx))
	return err
}

func (s *AudioService) GetSelectedMicrophone() (string, error) {
	var idx string
	s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'microphone'").Scan(&idx)
	return idx, nil
}

// SetSpeaker сохраняет выбранный playback-device в settings. Применяется при
// следующем JoinGroupVoice — в текущий активный playback изменение на лету не
// применяется (требовало бы пересоздания miniaudio device, что может дать
// кратковременный провал звука).
func (s *AudioService) SetSpeaker(idx int) error {
	_, err := s.store.DB().Exec(
		"INSERT OR REPLACE INTO settings (key, value) VALUES ('speaker', ?)", strconv.Itoa(idx))
	if err != nil {
		return err
	}
	s.log.Info("speaker selection saved", slog.Int("idx", idx))
	return nil
}

func (s *AudioService) GetSelectedSpeaker() (string, error) {
	var idx string
	s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'speaker'").Scan(&idx)
	return idx, nil
}

// --- Noise suppression ---
//
// Уровни (см. audio.NoiseSuppressionLevel):
//   0=off, 1=light (default), 2=rnn (требует сборки с tag nx_rnnoise), 3=deep (резерв).
// Хранится в settings('noise_suppression'). По умолчанию — light.

func (s *AudioService) SetNoiseSuppression(level int) error {
	lvl := audio.NoiseSuppressionLevel(level)
	if level < 0 || level > int(audio.NSDeep) {
		lvl = audio.NSLight
		level = int(lvl)
	}
	s.mu.Lock()
	if s.capture != nil {
		s.capture.SetNoiseSuppression(lvl)
	}
	s.mu.Unlock()
	// Проксируем на live voice capture (если юзер сидит в канале).
	// Делаем ВНЕ s.mu, чтобы не словить дедлок на VoiceService.s.mu.
	if s.voice != nil {
		s.voice.ApplyNoiseSuppression(lvl)
	}
	_, err := s.store.DB().Exec(
		"INSERT OR REPLACE INTO settings (key, value) VALUES ('noise_suppression', ?)",
		strconv.Itoa(level))
	s.log.Info("noise suppression level set",
		slog.Int("level", level),
		slog.Bool("rnnoise_available", audio.RNNoiseAvailable()))
	return err
}

// GetNoiseSuppression возвращает текущий уровень + силу DFN + доступность моделей.
type NoiseSuppressionInfo struct {
	Level            int  `json:"level"`
	RNNoiseAvailable bool `json:"rnnoiseAvailable"`
	DFNAvailable     bool `json:"dfnAvailable"`
	DFNStrength      int  `json:"dfnStrength"` // max attenuation в dB, 6..60
}

// DFN strength хранится как проценты (10..100%), маппится 1:1 в atten_lim dB
// для libDF: percent → db. 100% = 100 dB = unlimited (native libDF поведение),
// 10% = 10 dB мягкий NS. Значения < 10% бесполезны на практике.
//
// Дефолт 100% — DFN3 полноценно использует свои возможности; речь при этом
// не повреждается (подтверждено тестами). Проблема заикания, из-за которой
// мы изначально зажимали atten_lim, на самом деле была в ducking echo gate,
// который сейчас полностью удалён.
const (
	dfnStrengthDefault = 100 // %
	dfnStrengthMin     = 10
	dfnStrengthMax     = 100
)

func (s *AudioService) GetNoiseSuppression() (*NoiseSuppressionInfo, error) {
	info := &NoiseSuppressionInfo{
		Level:            defaultNSLevel(),
		RNNoiseAvailable: audio.RNNoiseAvailable(),
		DFNAvailable:     audio.DFNAvailable(),
		DFNStrength:      dfnStrengthDefault,
	}
	var v string
	s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'noise_suppression'").Scan(&v)
	if v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= int(audio.NSDeep) {
			info.Level = n
		}
	}
	var d string
	s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'dfn_strength'").Scan(&d)
	if d != "" {
		if n, err := strconv.Atoi(d); err == nil && n >= dfnStrengthMin && n <= dfnStrengthMax {
			info.DFNStrength = n
		}
	}
	return info, nil
}

// SetDFNStrength меняет силу DFN3 (максимальное attenuation в dB, 6..60).
// Сохраняется в settings('dfn_strength'), применяется к live capture если
// активен и DFN3 модель загружена — иначе применится при следующем
// JoinGroupVoice.
func (s *AudioService) SetDFNStrength(db int) error {
	if db < dfnStrengthMin {
		db = dfnStrengthMin
	}
	if db > dfnStrengthMax {
		db = dfnStrengthMax
	}
	s.mu.Lock()
	capRef := s.capture
	s.mu.Unlock()
	if capRef != nil {
		capRef.SetDFAttenLim(float32(db))
	}
	// Проксируем на live voice capture (если юзер сидит в канале).
	if s.voice != nil {
		s.voice.ApplyDFNStrength(float32(db))
	}
	_, err := s.store.DB().Exec(
		"INSERT OR REPLACE INTO settings (key, value) VALUES ('dfn_strength', ?)",
		strconv.Itoa(db))
	s.log.Info("DFN strength set", slog.Int("db", db))
	return err
}

// defaultNSLevel — дефолтный уровень при первом запуске (нет записи в settings).
// Приоритет: DFN3 > RNNoise > Light. DFN3 (NSDeep) по умолчанию с 2026-04-18 —
// он шире по диапазону и НЕ адаптируется к голосу как noise со временем, тогда
// как RNNoise может со временем начать глушить речь и молча ломать не-voice
// источники (видео/подкасты через speaker→mic). См. feedback_rnnoise_vs_dfn_nonvoice.
// Если DFN3-модель отсутствует на диске, C-код фоллбэчит на RNNoise (см. warning
// "DFN3 model not found, NSDeep will fall back to RNNoise" в audio.go:640).
func defaultNSLevel() int {
	if audio.DFNAvailable() {
		return int(audio.NSDeep)
	}
	if audio.RNNoiseAvailable() {
		return int(audio.NSRNN)
	}
	return int(audio.NSLight)
}

// --- Gain ---

func (s *AudioService) SetMicGain(gain float64) error {
	s.mu.Lock()
	if s.capture != nil {
		s.capture.SetGain(float32(gain))
	}
	s.mu.Unlock()
	// Live-apply на voice capture если юзер в звонке — иначе изменение
	// применилось бы только при следующем JoinGroupVoice из settings.
	if s.voice != nil {
		s.voice.ApplyMicGain(float32(gain))
	}
	s.store.DB().Exec("INSERT OR REPLACE INTO settings (key, value) VALUES ('mic_gain', ?)",
		strconv.FormatFloat(gain, 'f', 2, 64))
	return nil
}

// GetMicGain возвращает сохранённый mic gain multiplier из settings.
// 1.0 = нейтрально (default если юзер никогда не двигал слайдер).
func (s *AudioService) GetMicGain() (float64, error) {
	var raw string
	s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'mic_gain'").Scan(&raw)
	if raw == "" {
		return 1.0, nil
	}
	g, err := strconv.ParseFloat(raw, 64)
	if err != nil || g <= 0 {
		return 1.0, nil
	}
	return g, nil
}

func (s *AudioService) GetMicLevel() (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.capture != nil && s.capture.IsActive() {
		return float64(s.capture.PeakLevel()), nil
	}
	return 0, nil
}

// --- Mic Test ---

func (s *AudioService) StartMicTest(micIdx int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.stopMonitorLocked()
	s.micTesting = false
	if s.capture != nil && s.capture.IsActive() {
		s.capture.Stop()
	}
	s.capture = audio.NewCapture()
	s.applyGain()
	s.applyNoiseSuppression()

	if micIdx < 0 {
		micIdx = s.getMicIndex()
	}
	if err := s.capture.Start(micIdx); err != nil {
		return fmt.Errorf("AudioService.StartMicTest: %w", err)
	}
	s.micTesting = true
	if s.monitorOn {
		s.startMonitorLocked()
	}
	return nil
}

func (s *AudioService) StopMicTest() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopMonitorLocked()
	s.micTesting = false
	if s.capture != nil {
		s.capture.Stop()
	}
	return nil
}

// --- Recording ---

type StopRecordingResult struct {
	Audio    string `json:"audio"`    // base64 WAV
	Duration int    `json:"duration"` // мс
}

func (s *AudioService) StartRecording(micIdx int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Если capture уже запущен (mic test) — останавливаем. «Слушать себя»
	// читает тот же потоковый буфер, что и запись, — гасим первым.
	s.stopMonitorLocked()
	s.micTesting = false
	if s.capture != nil && s.capture.IsActive() {
		s.capture.Stop()
	}

	s.capture = audio.NewCapture()
	s.applyGain()
	s.applyNoiseSuppression()

	if micIdx < 0 {
		micIdx = s.getMicIndex()
	}
	if err := s.capture.Start(micIdx); err != nil {
		return fmt.Errorf("AudioService.StartRecording: %w", err)
	}
	s.capture.StartRecording()
	s.log.Info("🎙️ запись начата", slog.Int("mic", micIdx))
	return nil
}

func (s *AudioService) StopRecording() (*StopRecordingResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.capture == nil || !s.capture.IsActive() {
		return &StopRecordingResult{}, nil
	}

	duration := s.capture.RecordedDuration()
	wavData := s.capture.StopRecording()
	s.capture.Stop()

	if len(wavData) == 0 || duration < 500 {
		return &StopRecordingResult{}, nil
	}

	b64 := base64.StdEncoding.EncodeToString(wavData)
	s.log.Info("🎙️ запись остановлена",
		slog.Int("duration_ms", duration),
		slog.Int("wav_size", len(wavData)))

	return &StopRecordingResult{
		Audio:    b64,
		Duration: duration,
	}, nil
}

func (s *AudioService) GetRecordingLevel() (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.capture != nil && s.capture.IsActive() {
		return float64(s.capture.PeakLevel()), nil
	}
	return 0, nil
}

type RecordingStatus struct {
	Level    float64 `json:"level"`    // 0.0-1.0 peak
	Duration int     `json:"duration"` // мс
	Full     bool    `json:"full"`     // true = лимит достигнут
	MaxSec   int     `json:"maxSec"`   // максимум секунд
}

func (s *AudioService) GetRecordingStatus() (*RecordingStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := &RecordingStatus{MaxSec: audio.MaxRecordingSeconds()}
	if s.capture != nil && s.capture.IsActive() {
		status.Level = float64(s.capture.PeakLevel())
		status.Duration = s.capture.RecordedDuration()
		status.Full = s.capture.IsRecordingFull()
	}
	return status, nil
}

func (s *AudioService) GetRecordingDuration() (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.capture != nil {
		return float64(s.capture.RecordedDuration()), nil
	}
	return 0, nil
}

// --- Send Voice Message (DM) ---

func (s *AudioService) SendVoiceMessage(peerID, audioB64 string, duration int) (*SendResult, error) {
	const op = "AudioService.SendVoiceMessage"

	myID, err := s.sess.PeerID()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	// Decode audio
	wavData, err := base64.StdEncoding.DecodeString(audioB64)
	if err != nil {
		return nil, fmt.Errorf("%s: invalid audio: %w", op, err)
	}

	// Generate waveform
	waveform := audio.GenerateWaveform(wavData, 32)

	msgID := uuid.New().String()
	now := time.Now().Unix()

	// Save to DB
	_, err = s.store.DB().Exec(
		`INSERT INTO messages (id, peer_id, sender_id, content, type, audio_data, duration, waveform, delivered, created_at)
		 VALUES (?, ?, ?, ?, 'voice', ?, ?, ?, 0, ?)`,
		msgID, peerID, myID, "🎤 Голосовое сообщение", wavData, duration, waveform, now)
	if err != nil {
		return nil, fmt.Errorf("%s: db: %w", op, err)
	}

	// Send via P2P (chunked)
	delivered := 0
	if s.network.IsP2PConnected(peerID) {
		if s.sendVoiceChunked(peerID, "", msgID, audioB64, duration, waveform, now) {
			delivered = 1
			if _, err := s.store.DB().Exec("UPDATE messages SET delivered = 1 WHERE id = ?", msgID); err != nil {
				s.log.Warn("не удалось пометить voice message как доставленное (P2P отправка прошла, но БД UPDATE упал)",
					slog.String("msg_id", msgID),
					slog.String("error", err.Error()))
			}
		}
	}

	s.log.Info("🎤 VOICE DM ОТПРАВЛЕНО",
		slog.String("to", safeShort(peerID)),
		slog.String("msg_id", msgID),
		slog.Int("duration", duration),
		slog.Int("wav_size", len(wavData)),
		slog.Bool("delivered", delivered == 1))

	return &SendResult{ID: msgID, Delivered: delivered == 1, Waveform: waveform}, nil
}

// --- Send Group Voice Message ---

func (s *AudioService) SendGroupVoiceMessage(groupID, audioB64 string, duration int) (*SendResult, error) {
	const op = "AudioService.SendGroupVoiceMessage"

	myID, err := s.sess.PeerID()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	// Check membership
	var memberStatus string
	err = s.store.DB().QueryRow(
		"SELECT status FROM group_members WHERE group_id = ? AND peer_id = ?",
		groupID, myID).Scan(&memberStatus)
	if err != nil || memberStatus != "active" {
		return nil, fmt.Errorf("%s: не участник группы", op)
	}

	wavData, err := base64.StdEncoding.DecodeString(audioB64)
	if err != nil {
		return nil, fmt.Errorf("%s: invalid audio: %w", op, err)
	}

	waveform := audio.GenerateWaveform(wavData, 32)

	msgID := uuid.New().String()
	now := time.Now().Unix()

	// Save to DB
	_, err = s.store.DB().Exec(
		`INSERT INTO group_messages (id, group_id, sender_id, content, type, audio_data, duration, waveform, created_at)
		 VALUES (?, ?, ?, ?, 'voice', ?, ?, ?, ?)`,
		msgID, groupID, myID, "🎤 Голосовое сообщение", wavData, duration, waveform, now)
	if err != nil {
		return nil, fmt.Errorf("%s: db: %w", op, err)
	}

	// Fan-out to group members via P2P (chunked) + delivery tracking
	var sent int
	if s.groups != nil {
		members := s.groups.GetGroupMemberIDs(groupID)
		for _, pid := range members {
			if pid == myID {
				continue
			}
			// Создаём запись доставки как недоставленное (save-first)
			s.store.DB().Exec(
				`INSERT OR IGNORE INTO group_message_delivery (msg_id, peer_id, delivered) VALUES (?, ?, 0)`,
				msgID, pid)

			if s.network.IsP2PConnected(pid) {
				s.sendVoiceChunked(pid, groupID, msgID, audioB64, duration, waveform, now)
				sent++
				s.store.DB().Exec(
					"UPDATE group_message_delivery SET delivered = 1 WHERE msg_id = ? AND peer_id = ?",
					msgID, pid)
			}
		}
	}

	s.log.Info("🎤 VOICE GROUP ОТПРАВЛЕНО",
		slog.String("group", safeShort8(groupID)),
		slog.String("msg_id", msgID),
		slog.Int("duration", duration),
		slog.Int("sent", sent))

	return &SendResult{ID: msgID, Delivered: sent > 0, Waveform: waveform}, nil
}

// --- chunked voice P2P ---

// sendVoiceChunked отправляет голосовое сообщение через P2P по частям.
// groupID = "" для DM, иначе для групповых.
func (s *AudioService) sendVoiceChunked(peerID, groupID, msgID, audioB64 string, duration int, waveform string, now int64) bool {
	msgType := "voice_message"
	if groupID != "" {
		msgType = "group_voice_message"
	}

	// 1. Отправляем metadata (без audio)
	meta := P2PMessage{
		Type:     msgType,
		ID:       msgID,
		GroupID:  groupID,
		Duration: duration,
		Waveform: waveform,
		Time:     now,
	}
	data := marshalJSON(meta)
	if data == nil {
		return false
	}
	if err := s.network.SendP2PData(peerID, data); err != nil {
		return false
	}

	// 2. Отправляем chunks
	// Chunk base64 string directly (не decode → re-encode)
	chunkB64Size := 64 * 1024 // 64 KB base64 per chunk
	totalChunks := (len(audioB64) + chunkB64Size - 1) / chunkB64Size

	for i := 0; i < totalChunks; i++ {
		start := i * chunkB64Size
		end := start + chunkB64Size
		if end > len(audioB64) {
			end = len(audioB64)
		}

		chunk := P2PMessage{
			Type:    "voice_chunk",
			ID:      msgID,
			GroupID: groupID,
			Content: audioB64[start:end],
			Audio:   fmt.Sprintf("%d/%d", i, totalChunks),
		}
		chunkData := marshalJSON(chunk)
		if chunkData == nil {
			return false
		}
		if err := s.network.SendP2PDataThrottled(peerID, chunkData); err != nil {
			s.log.Warn("voice chunk send error",
				slog.String("peer", safeShort(peerID)),
				slog.Int("chunk", i),
				slog.String("error", err.Error()))
			return false
		}
	}

	// 3. Отправляем complete
	complete := P2PMessage{
		Type:    "voice_complete",
		ID:      msgID,
		GroupID: groupID,
	}
	completeData := marshalJSON(complete)
	if completeData != nil {
		s.network.SendP2PData(peerID, completeData)
	}

	s.log.Debug("voice chunked send done",
		slog.String("peer", safeShort(peerID)),
		slog.String("msg_id", msgID),
		slog.Int("chunks", totalChunks),
		slog.Int("total_b64", len(audioB64)))

	return true
}

// --- helpers ---

func (s *AudioService) getMicIndex() int {
	var idx string
	s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'microphone'").Scan(&idx)
	n, err := strconv.Atoi(idx)
	if err != nil {
		return -1
	}
	return n
}

func (s *AudioService) applyGain() {
	if s.capture == nil {
		return
	}
	var gainStr string
	s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'mic_gain'").Scan(&gainStr)
	if g, err := strconv.ParseFloat(gainStr, 32); err == nil && g > 0 {
		s.capture.SetGain(float32(g))
	}
}

// applyNoiseSuppression читает сохранённый уровень и применяет к текущему capture.
// Должен вызываться сразу после NewCapture(), до Start.
func (s *AudioService) applyNoiseSuppression() {
	if s.capture == nil {
		return
	}
	level := audio.NoiseSuppressionLevel(defaultNSLevel())
	var v string
	s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'noise_suppression'").Scan(&v)
	if v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= int(audio.NSDeep) {
			level = audio.NoiseSuppressionLevel(n)
		}
	}
	if level == audio.NSDeep {
		loadDFModelIfPossible(s.log, s.capture)
	}
	s.capture.SetNoiseSuppression(level)
	s.log.Debug("noise suppression applied to capture", slog.Int("level", int(level)))
}

// loadDFModelIfPossible ищет DeepFilterNet3.tar.gz рядом с исполняемым файлом
// и загружает его в capture. Вызывать только когда бинарь собран с тегом nx_dfn
// (audio.DFNAvailable()). Тихо пропускает если модель не найдена — тогда NSDeep
// фоллбекнется на RNNoise.
// dfnLoadMu сериализует загрузку DFN3-модели. JoinGroupVoice и
// ApplyNoiseSuppression запускают загрузку из независимых горутин; без этого
// mutex+recheck обе видели DFModelLoaded()==false и обе звали nx_capture_df_load,
// который destroy'ит уже опубликованный DFState → UAF в RT audio-callback'е.
var dfnLoadMu sync.Mutex

func loadDFModelIfPossible(log *slog.Logger, cap *audio.Capture) {
	if cap == nil || !audio.DFNAvailable() {
		return
	}
	dfnLoadMu.Lock()
	defer dfnLoadMu.Unlock()
	// Re-check под локом: другая горутина могла загрузить, пока мы ждали mutex.
	if cap.DFModelLoaded() {
		return
	}
	path := findDFModelPath()
	if path == "" {
		log.Warn("DFN3 model not found, NSDeep will fall back to RNNoise")
		return
	}
	if err := cap.LoadDFModel(path); err != nil {
		log.Warn("failed to load DFN3 model", slog.String("path", path), slog.String("err", err.Error()))
		return
	}
	log.Info("DFN3 model loaded", slog.String("path", path))
}

// findDFModelPath ищет DeepFilterNet3.tar.gz в стандартных местах:
//  1. env NEXUS_DFN_MODEL (полный путь)
//  2. <exeDir>/models/DeepFilterNet3.tar.gz
//  3. <cwd>/models/DeepFilterNet3.tar.gz
//  4. <cwd>/../.local/models/DeepFilterNet3.tar.gz  (для запуска из app/)
func findDFModelPath() string {
	if p := os.Getenv("NEXUS_DFN_MODEL"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	const name = "DeepFilterNet3.tar.gz"
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "models", name))
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates,
			filepath.Join(cwd, "models", name),
			filepath.Join(cwd, "..", ".local", "models", name))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

func marshalJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return data
}
