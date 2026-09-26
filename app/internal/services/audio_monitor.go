package services

import (
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"vox_app/internal/audio"
	"vox_app/internal/codec"
)

// «Слушать себя» и «Сбросить настройки звука» (фаза 7c, макет SettingsDevices).
//
// Слушать себя: во время проверки микрофона обработанный голос (усиление +
// шумодав уровня C: Light / RNNoise / DFN3) возвращается в динамик. Флаг
// живёт только в памяти — без наушников будет свист, поэтому после
// перезапуска он выключен.

// SetMicMonitor включает/выключает возврат звука. Если проверка идёт —
// сразу, иначе при следующем StartMicTest.
func (s *AudioService) SetMicMonitor(on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.monitorOn = on
	if !on {
		s.stopMonitorLocked()
		return nil
	}
	if s.micTesting && s.capture != nil && s.capture.IsActive() && s.monPB == nil {
		s.startMonitorLocked()
	}
	return nil
}

// startMonitorLocked — под s.mu. Горутина s.mu не берёт: capture и playback
// получает замыканием, останавливается через monStop.
func (s *AudioService) startMonitorLocked() {
	capRef := s.capture
	spk := s.getSpeakerIndex()
	pb := audio.NewPlayback()
	if err := pb.Start(spk); err != nil {
		pb.Close()
		s.log.Warn("mic monitor: playback start failed",
			slog.Int("speaker_idx", spk),
			slog.String("error", err.Error()))
		return
	}
	// 40 мс тишины — запас между тикером и callback'ом устройства (как voice.go).
	silence := make([]byte, codec.FrameSize*2)
	for range 2 {
		pb.Write(silence)
	}
	capRef.StreamStart()

	stop := make(chan struct{})
	done := make(chan struct{})
	s.monCap, s.monPB, s.monStop, s.monDone = capRef, pb, stop, done
	go func() {
		defer close(done)
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				// Сливаем всё накопленное: StreamRead отдаёт сколько есть.
				for {
					chunk := capRef.StreamRead(codec.FrameSize * 2)
					if len(chunk) == 0 {
						break
					}
					pb.Write(chunk)
				}
			}
		}
	}()
	s.log.Info("mic monitor started", slog.Int("speaker_idx", spk))
}

// stopMonitorLocked — под s.mu; без монитора ничего не делает.
func (s *AudioService) stopMonitorLocked() {
	if s.monPB == nil {
		return
	}
	close(s.monStop)
	<-s.monDone
	if s.monCap != nil {
		s.monCap.StreamStop()
	}
	s.monPB.Stop()
	s.monPB.Close()
	s.monCap, s.monPB, s.monStop, s.monDone = nil, nil, nil, nil
	s.log.Info("mic monitor stopped")
}

// getSpeakerIndex — сохранённый динамик, -1 — системный.
func (s *AudioService) getSpeakerIndex() int {
	var v string
	s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'speaker'").Scan(&v)
	if n, err := strconv.Atoi(v); err == nil {
		return n
	}
	return -1
}

// ResetAudioSettings — «Сбросить настройки звука»: всё из вкладки «Звук» —
// устройства на системные, усиление и громкость голоса 100%, шумодав,
// сила DFN и нейрофильтр по умолчанию. Через сеттеры, чтобы идущий звонок и
// проверка микрофона применили сразу. VAD/DTX/SFU («Дополнительно») и
// громкости участников не трогает.
func (s *AudioService) ResetAudioSettings() error {
	if _, err := s.store.DB().Exec("DELETE FROM settings WHERE key IN ('microphone', 'speaker')"); err != nil {
		return fmt.Errorf("AudioService.ResetAudioSettings: %w", err)
	}
	if err := s.SetMicGain(1.0); err != nil {
		return fmt.Errorf("AudioService.ResetAudioSettings: %w", err)
	}
	ns := defaultNSLevel()
	if err := s.SetNoiseSuppression(ns); err != nil {
		return fmt.Errorf("AudioService.ResetAudioSettings: %w", err)
	}
	if err := s.SetDFNStrength(dfnStrengthDefault); err != nil {
		return fmt.Errorf("AudioService.ResetAudioSettings: %w", err)
	}
	if s.voice != nil {
		if err := s.voice.SetMasterVoiceVolume(1.0); err != nil {
			return fmt.Errorf("AudioService.ResetAudioSettings: %w", err)
		}
		s.voice.SetNeuralMode("gtcrn")
		s.voice.SetFastEnhancerSize(string(audio.FEBase))
		s.voice.SetFastEnhancerMix(1)
		s.voice.SetGTCRNVariant(string(audio.GTCRNDNS3))
		s.voice.SetGTCRNMix(1)
	}
	s.log.Info("audio settings reset",
		slog.Int("noise_suppression", ns),
		slog.Int("dfn_strength", dfnStrengthDefault))
	return nil
}
