package services

import (
	"log/slog"
	"time"
)

// Opus DTX (Discontinuous Transmission) management.
//
// DTX — встроенный режим Opus, при котором энкодер не emit'ит пакеты для
// полной тишины (или эмитит 1-байтный TOC). Экономия трафика ~70% на паузах,
// без лишних зависимостей и моделей. Поверх DTX планируется Silero VAD как
// отдельный этап (Этап 3 в плане трёхфазного эксперимента по трафику).
//
// Настройка сохраняется в settings (ключ opus_dtx), применяется в
// JoinGroupVoice к новому энкодеру, и hot-swap через SetOpusDTX во время
// активной сессии (Encoder.SetDTX сериализуется с Encode через внутренний
// sync.Mutex в codec.Encoder).

// SetOpusDTX переключает DTX. Сохраняет в БД, обновляет s.opusDTX и, если
// кодек уже создан (активная голосовая сессия), применяет сразу —
// перевхода в канал не требуется.
func (s *VoiceService) SetOpusDTX(enabled bool) error {
	val := "0"
	if enabled {
		val = "1"
	}
	if _, err := s.store.DB().Exec(
		"INSERT OR REPLACE INTO settings (key, value) VALUES ('opus_dtx', ?)", val); err != nil {
		return err
	}
	s.mu.Lock()
	s.opusDTX = enabled
	enc := s.opusEncoder
	s.mu.Unlock()

	if enc != nil {
		if err := enc.SetDTX(enabled); err != nil {
			s.log.Warn("opus DTX hot-swap failed",
				slog.Bool("enabled", enabled),
				slog.String("error", err.Error()))
			return err
		}
		s.log.Info("opus DTX hot-swap", slog.Bool("enabled", enabled))
	}
	return nil
}

// GetOpusDTX возвращает текущее значение настройки.
func (s *VoiceService) GetOpusDTX() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opusDTX, nil
}

// LoadOpusSettings подгружает DTX-флаг из БД. Вызывается при старте из main.go
// после LoadSFUSettings, до первого JoinGroupVoice.
func (s *VoiceService) LoadOpusSettings() {
	var val string
	if s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'opus_dtx'").Scan(&val) == nil {
		s.opusDTX = val == "1"
		s.log.Info("opus settings loaded", slog.Bool("dtx", s.opusDTX))
	}
}

// VoiceBitrate — снапшот трафика голосовой сессии для UI-readout'а.
// Kbps считается по дельте с предыдущего вызова (окно = поллинг интервал
// UI, обычно 2с). AvgKbps — среднее за всю сессию для baseline сравнения.
type VoiceBitrate struct {
	Active       bool    `json:"active"`       // false если не в канале
	TotalBytes   int64   `json:"totalBytes"`   // суммарно за сессию
	InstantKbps  float64 `json:"instantKbps"`  // за последнее окно (между двумя GetVoiceBitrate)
	AvgKbps      float64 `json:"avgKbps"`      // за всю сессию
	SessionSec   float64 `json:"sessionSec"`   // длительность сессии
}

// GetVoiceBitrate возвращает актуальный снапшот трафика.
// Во время первого вызова InstantKbps == AvgKbps (нет предыдущей точки).
// После — InstantKbps по дельте. Побочный эффект: обновляет prev-snapshot.
func (s *VoiceService) GetVoiceBitrate() (VoiceBitrate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var br VoiceBitrate
	if s.activeGroupID == "" {
		return br, nil
	}
	br.Active = true

	now := time.Now()
	total := s.prof.sentBytes
	sessionElapsed := now.Sub(s.prof.sessionStart).Seconds()
	if sessionElapsed > 0 {
		br.AvgKbps = float64(total) * 8.0 / 1000.0 / sessionElapsed
	}
	br.TotalBytes = total
	br.SessionSec = sessionElapsed

	// Instant kbps = delta с предыдущего вызова. Первый вызов =
	// prevSentTs == sessionStart, что даёт тот же avg.
	windowSec := now.Sub(s.prevSentTs).Seconds()
	if windowSec > 0.1 { // нижняя граница, избегаем деления на ~0
		delta := total - s.prevSentBytes
		br.InstantKbps = float64(delta) * 8.0 / 1000.0 / windowSec
	} else {
		br.InstantKbps = br.AvgKbps
	}
	s.prevSentBytes = total
	s.prevSentTs = now

	return br, nil
}
