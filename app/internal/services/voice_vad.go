package services

import (
	"log/slog"
	"vox_app/internal/audio/vad"
	"os"
	"path/filepath"
	"strconv"
)

// Voice Activity Detection (VAD) management.
//
// VAD — Этап 3 трёхфазного rollout'а экономии голосового трафика (см.
// memory/project_voice_bandwidth_rollout.md). Гейтит исходящие opus-фреймы
// до отправки в сеть: если Silero v5 сказал "не речь" (speech_prob < threshold),
// фрейм не отправляется, но seq продолжает двигаться (на resume приёмник
// заполнит PLC). Safety nets: pre-roll 100 ms + hangover 400 ms, чтобы не
// рубить начала/концы слов (см. memory/feedback_vad_no_gate.md).
//
// Модель грузится из models/VAD/silero_vad.onnx (runtime-load, как DFN3).
// Создание детектора — асинхронно, до завершения audioLoop использует
// pass-through режим (vadDetector == nil → любой фрейм считается речью).

// SetVADEnabled переключает VAD. Сохраняет в БД, обновляет s.vadEnabled
// и запускает асинхронную загрузку модели если мы в голосовой сессии
// и детектор ещё не создан. Hot-swap во время звонка безопасен.
func (s *VoiceService) SetVADEnabled(enabled bool) error {
	val := "0"
	if enabled {
		val = "1"
	}
	if _, err := s.store.DB().Exec(
		"INSERT OR REPLACE INTO settings (key, value) VALUES ('vad_enabled', ?)", val); err != nil {
		return err
	}

	s.mu.Lock()
	s.vadEnabled = enabled
	activeSession := s.activeGroupID != ""
	needLoad := enabled && s.vadDetector == nil && activeSession
	// При выключении — чистим pre-roll/ring/hangover, детектор оставляем живым
	// для быстрого re-enable. При включении — ресет state если детектор уже есть.
	if !enabled {
		s.vadRingLen = 0
		s.vadPreRollHead = 0
		s.vadPreRollCount = 0
		s.vadHangover = 0
		for i := range s.vadPreRoll {
			s.vadPreRoll[i] = nil
		}
	} else if s.vadDetector != nil {
		s.vadDetector.Reset()
	}
	s.mu.Unlock()

	if needLoad {
		go s.loadVADDetectorAsync()
	}
	s.log.Info("VAD setting changed", slog.Bool("enabled", enabled))
	return nil
}

// GetVADEnabled возвращает текущее значение.
func (s *VoiceService) GetVADEnabled() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.vadEnabled, nil
}

// SetVADThreshold обновляет порог в [0.1, 0.9].
func (s *VoiceService) SetVADThreshold(threshold float64) error {
	if threshold < 0.1 {
		threshold = 0.1
	}
	if threshold > 0.9 {
		threshold = 0.9
	}
	if _, err := s.store.DB().Exec(
		"INSERT OR REPLACE INTO settings (key, value) VALUES ('vad_threshold', ?)",
		strconv.FormatFloat(threshold, 'f', 3, 64)); err != nil {
		return err
	}
	s.mu.Lock()
	s.vadThreshold = float32(threshold)
	s.mu.Unlock()
	return nil
}

// GetVADThreshold возвращает текущий порог как float64 (для JS).
func (s *VoiceService) GetVADThreshold() (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return float64(s.vadThreshold), nil
}

// LoadVADSettings подгружает настройки из БД. Вызывается из main.go после
// LoadOpusSettings, до первого JoinGroupVoice.
func (s *VoiceService) LoadVADSettings() {
	var val string
	if s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'vad_enabled'").Scan(&val) == nil {
		s.vadEnabled = val == "1"
	}
	if s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'vad_threshold'").Scan(&val) == nil {
		if v, err := strconv.ParseFloat(val, 32); err == nil && v >= 0.1 && v <= 0.9 {
			s.vadThreshold = float32(v)
		}
	}
	s.log.Info("VAD settings loaded",
		slog.Bool("enabled", s.vadEnabled),
		slog.Float64("threshold", float64(s.vadThreshold)))
}

// loadVADDetectorAsync грузит Silero модель в фоне и кладёт в s.vadDetector.
// Безопасно к вызову несколько раз: под s.mu проверяется что detector всё
// ещё nil и сессия всё ещё та же. Ошибки — в лог.
func (s *VoiceService) loadVADDetectorAsync() {
	exe, err := os.Executable()
	if err != nil {
		s.log.Warn("VAD load: cannot locate executable", slog.String("err", err.Error()))
		return
	}
	modelPath := filepath.Join(filepath.Dir(exe), "models", "VAD", "silero_vad.onnx")
	if _, err := os.Stat(modelPath); err != nil {
		s.log.Warn("VAD model not found",
			slog.String("path", modelPath),
			slog.String("err", err.Error()))
		return
	}

	det, err := vad.NewSilero(modelPath, s.log)
	if err != nil {
		s.log.Warn("VAD load failed", slog.String("err", err.Error()))
		return
	}

	s.mu.Lock()
	// Гонка: пока грузили, пользователь мог выключить VAD или выйти из канала.
	// Если detector в это время был создан конкурентно — закрываем наш.
	if s.vadDetector != nil || !s.vadEnabled || s.activeGroupID == "" {
		s.mu.Unlock()
		det.Close()
		s.log.Info("VAD detector discarded (session/toggle changed during load)")
		return
	}
	s.vadDetector = det
	s.mu.Unlock()
	s.log.Info("VAD detector ready")
}
