package services

import (
	"log/slog"

	"vox_app/internal/audio"
)

// handleDeviceChanges крутится во фоне пока активен звонок (stopCh не закрыт)
// и пересоздаёт capture/playback при смене default-устройства в системе.
//
// Триггер срабатывает только если юзер выбрал default (idx==-1) для
// соответствующего типа. Если он явно выбрал конкретное устройство
// (watchedMicIdx >= 0), смена системного default'а игнорируется — это
// его осознанный выбор, не трогаем.
//
// Restart выполняется под s.mu чтобы не гонять с leaveInternal/JoinGroupVoice.
// Между ma_device_stop и ma_device_start fragment ~50-200ms — пир услышит
// микропровал/тишину, jitter buffer переживёт.
func (s *VoiceService) handleDeviceChanges(w *audio.DefaultDeviceWatcher, stopCh <-chan struct{}) {
	if w == nil {
		return
	}
	for {
		select {
		case <-stopCh:
			return
		case ev, ok := <-w.Events():
			if !ok {
				return
			}
			s.applyDeviceChange(ev)
		}
	}
}

func (s *VoiceService) applyDeviceChange(ev audio.DeviceChange) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Сессия могла закончиться между recv event и acquired lock.
	if s.activeGroupID == "" {
		return
	}

	switch ev.Type {
	case audio.DeviceCapture:
		if s.watchedMicIdx != -1 || s.capture == nil {
			return
		}
		if err := s.capture.Restart(-1); err != nil {
			s.log.Warn("🔄 audio: capture restart failed",
				slog.String("new_device", ev.NewName),
				slog.String("error", err.Error()))
			return
		}
		s.log.Info("🔄 audio: capture restarted on new default mic",
			slog.String("device", ev.NewName))
	case audio.DevicePlayback:
		if s.watchedSpkIdx != -1 || s.playback == nil {
			return
		}
		if err := s.playback.Restart(-1); err != nil {
			s.log.Warn("🔄 audio: playback restart failed",
				slog.String("new_device", ev.NewName),
				slog.String("error", err.Error()))
			return
		}
		s.log.Info("🔄 audio: playback restarted on new default speaker",
			slog.String("device", ev.NewName))
	}
}
