package services

import (
	"encoding/binary"
	"log/slog"
	"strconv"
	"time"

	"vox_app/internal/audio"
	"vox_app/internal/codec"
)

// Звук системы при демонстрации экрана (фаза 5e).
//
// Отправитель: WASAPI loopback без звука самого Vox (Windows ≥ 20348) →
// Opus 48 кГц стерео, APPLICATION_AUDIO, 96 кбит/с, DTX → пакет
// [SAU1][seq u16 LE][opus] по DataChannel "screen-audio" каждому зрителю,
// приславшему caps screenAudio. Голосовой тракт (16 кГц моно, шумодав, VAD)
// для музыки и видео не годится, поэтому поток отдельный.
//
// Зритель: на каждого показывающего свой Opus-декодер с PLC и отдельное
// устройство вывода 48 кГц стерео на выбранный динамик. Громкость = мастер-
// громкость голосов × ползунок демонстрации; в режиме «наушники выключены»
// звук не пишется.
const (
	screenAudioMagic = "SAU1"
	screenAudioHdr   = 6
	screenAudioTick  = 10 * time.Millisecond
	// screenAudioMaxPLC — сколько потерянных кадров подряд заполнять PLC;
	// дальше (например, после паузы DTX) просто продолжаем с нового пакета.
	screenAudioMaxPLC = 5
)

type screenAudioRx struct {
	dec     *codec.Decoder
	pb      *audio.StereoPlayback
	lastSeq uint16
	started bool
	packets int64 // для лога при закрытии
	plc     int64
}

// ScreenAudioSupported — можно ли захватить звук системы без звука Vox.
// Проверяется пробным открытием, один раз за запуск.
func (s *ScreenShareService) ScreenAudioSupported() bool {
	s.audioSupportOnce.Do(func() {
		s.audioSupported = audio.LoopbackSupported()
		s.log.Info("screen audio: loopback", slog.Bool("supported", s.audioSupported))
	})
	return s.audioSupported
}

// SetScreenShareVolume — громкость демонстрации пира для меня (0..2).
func (s *ScreenShareService) SetScreenShareVolume(peerID string, volume float64) error {
	if volume < 0 {
		volume = 0
	}
	if volume > 2 {
		volume = 2
	}
	s.audioRxMu.Lock()
	s.shareVolumes[peerID] = float32(volume)
	rx := s.audioRx[peerID]
	s.audioRxMu.Unlock()
	if rx != nil && rx.pb != nil {
		rx.pb.SetVolume(s.shareVolume(peerID))
	}
	return nil
}

func (s *ScreenShareService) shareVolume(peerID string) float32 {
	master := float32(1)
	if s.voice != nil {
		if v, err := s.voice.GetMasterVoiceVolume(); err == nil {
			master = float32(v)
		}
	}
	s.audioRxMu.Lock()
	v, ok := s.shareVolumes[peerID]
	s.audioRxMu.Unlock()
	if !ok {
		v = 1
	}
	return master * v
}

// audioLoop — отправитель, живёт вместе с captureLoop (captureWG, stopCh).
func (s *ScreenShareService) audioLoop() {
	defer s.captureWG.Done()
	s.mu.Lock()
	stop := s.stopCh
	groupID := s.activeGroupID
	s.mu.Unlock()

	lb := audio.NewLoopback()
	defer lb.Close()
	if err := lb.Start(); err != nil {
		s.log.Warn("screen audio: loopback не открылся", slog.String("error", err.Error()))
		return
	}
	enc, err := codec.NewAudioEncoder()
	if err != nil {
		s.log.Warn("screen audio: кодек", slog.String("error", err.Error()))
		return
	}
	defer enc.Close()
	s.log.Info("screen audio start", slog.String("group", safeShort8(groupID)))

	var seq uint16
	var sent int64
	tick := time.NewTicker(screenAudioTick)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			s.log.Info("screen audio stop", slog.Int64("packets", sent))
			return
		case <-tick.C:
		}
		for {
			frame := lb.ReadFrame()
			if frame == nil {
				break
			}
			seq++
			pkt, err := enc.Encode(frame)
			if err != nil || len(pkt) <= 2 {
				continue // DTX: тишина — пакет не шлём
			}
			buf := make([]byte, screenAudioHdr+len(pkt))
			copy(buf, screenAudioMagic)
			binary.LittleEndian.PutUint16(buf[4:], seq)
			copy(buf[screenAudioHdr:], pkt)
			for _, pid := range s.viewerIDs(groupID) {
				if !s.network.peerHasCap(pid, capScreenAudio) || !s.network.IsScreenDCOpen(pid) {
					continue
				}
				if err := s.network.EnsureScreenAudioDC(pid); err != nil {
					continue
				}
				_ = s.network.SendScreenAudio(pid, buf)
			}
			sent++
		}
	}
}

// handleScreenAudio — зритель: пакет звука от показывающего.
func (s *ScreenShareService) handleScreenAudio(from string, data []byte) {
	if s.network.IsBlocked(from) {
		return // заблокированный (blocklist.go)
	}
	if len(data) <= screenAudioHdr || string(data[:4]) != screenAudioMagic {
		return
	}
	if s.voice != nil && s.voice.IsDeafened() {
		return
	}
	seq := binary.LittleEndian.Uint16(data[4:6])
	rx := s.ensureAudioRx(from)
	if rx == nil {
		return
	}
	if rx.started {
		gap := seq - rx.lastSeq
		if gap == 0 || gap > 0x8000 {
			return // дубль или опоздавший
		}
		if gap > 1 && int(gap) <= screenAudioMaxPLC+1 {
			for i := 1; i < int(gap); i++ {
				if pcm, err := rx.dec.DecodePLC(); err == nil {
					rx.pb.Write(pcm)
					rx.plc++
				}
			}
		}
	}
	rx.started = true
	rx.lastSeq = seq
	pcm, err := rx.dec.Decode(data[screenAudioHdr:], false)
	if err != nil {
		return
	}
	rx.pb.Write(pcm)
	rx.packets++
}

func (s *ScreenShareService) ensureAudioRx(peerID string) *screenAudioRx {
	s.audioRxMu.Lock()
	rx := s.audioRx[peerID]
	s.audioRxMu.Unlock()
	if rx != nil {
		return rx
	}
	dec, err := codec.NewAudioDecoder()
	if err != nil {
		s.log.Warn("screen audio: декодер", slog.String("error", err.Error()))
		return nil
	}
	pb := audio.NewStereoPlayback()
	if err := pb.Start(s.speakerIndex()); err != nil {
		s.log.Warn("screen audio: динамик", slog.String("error", err.Error()))
		dec.Close()
		pb.Close()
		return nil
	}
	rx = &screenAudioRx{dec: dec, pb: pb}
	s.audioRxMu.Lock()
	if existing := s.audioRx[peerID]; existing != nil {
		s.audioRxMu.Unlock()
		dec.Close()
		pb.Close()
		return existing
	}
	s.audioRx[peerID] = rx
	s.audioRxMu.Unlock()
	pb.SetVolume(s.shareVolume(peerID))
	s.log.Info("screen audio: приём начат", slog.String("peer", safeShort(peerID)))
	s.network.emitEvent("vox:screen-audio", map[string]any{"peerId": peerID, "active": true})
	return rx
}

// closeAudioRx — показывающий остановился или его screen PC закрылся.
func (s *ScreenShareService) closeAudioRx(peerID string) {
	s.audioRxMu.Lock()
	rx := s.audioRx[peerID]
	delete(s.audioRx, peerID)
	s.audioRxMu.Unlock()
	if rx == nil {
		return
	}
	rx.pb.Close()
	rx.dec.Close()
	s.log.Info("screen audio: приём завершён", slog.String("peer", safeShort(peerID)),
		slog.Int64("packets", rx.packets), slog.Int64("plc", rx.plc))
	s.network.emitEvent("vox:screen-audio", map[string]any{"peerId": peerID, "active": false})
}

// speakerIndex — выбранный динамик (как у голосового playback), -1 — по умолчанию.
func (s *ScreenShareService) speakerIndex() int {
	var v string
	s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'speaker'").Scan(&v)
	if n, err := strconv.Atoi(v); err == nil {
		return n
	}
	return -1
}
