package services

import (
	"log/slog"
	"math"
	"vox_app/internal/codec"
	"time"
)

// Incoming voice frame handler.
//
// handleVoiceFrame вызывается из onVoiceFrame callback PeerManager'а на
// каждом DC-сообщении от пира. Цепочка обработки:
//
//   1. Распаковка DFNP wire-latency header (debug only)
//   2. Захват s.mu (defer Unlock на любой path выхода — критично, см. ниже)
//   3. Profiling counters: recvFrames, jitter, latency probe
//   4. Auto-promote пира из channelPeers в participants (gossip recovery)
//   5. Drop если deafened
//   6. Opus decode + FEC/PLC recovery если есть VOX1 sequence header
//   7. Применение per-peer + master volume
//   8. Speaking detection (RMS) для UI индикатора
//   9. Append в mixBuf для audioLoop
//
// ВАЖНО: defer s.mu.Unlock() в начале — гарантирует освобождение мьютекса
// при любом пути выхода (включая panic в opus decoder). Раньше было ручное
// Unlock+return на каждой ветке, что не выдерживало panic'и.
//
// emitEvent вызовы в конце выполняются под удерживаемым lock'ом — это
// безопасно, т.к. emit использует NetworkService.mu, не VoiceService.mu.

var voiceFramesReceived int

func (s *VoiceService) handleVoiceFrame(fromPubKey string, pcmData []byte) {
	if s.network != nil && s.network.IsBlocked(fromPubKey) {
		return // заблокированный — голос заглушён (blocklist.go)
	}
	recvStart := time.Now()
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("handleVoiceFrame panic", slog.Any("recover", r))
		}
	}()

	// DFNP wire-latency header: ВСЕГДА распаковываем если магик есть,
	// независимо от voiceDebug на приёмнике. Иначе debug-отправитель
	// (nx_voicedebug) добавляет 12-байтный префикс, а release-приёмник
	// его не снимает → unwrapVOX1 не находит VOX1 → Opus декодирует
	// garbage с "валидным" TOC (0x44='D' → SILK WB 10ms), возвращая
	// 160 сэмплов (320 байт) вместо 640 → mix-loop дропает фрейм по
	// условию `len(frame) >= FrameSize*2` → pb_writes=0 → тишина.
	// Статистику probe учитываем только в debug-сборке.
	var hasProbe bool
	var probeNs int64
	if p, sendNs, ok := unwrapDFNP(pcmData); ok {
		pcmData = p
		if voiceDebug {
			probeNs = recvStart.UnixNano() - sendNs
			hasProbe = true
		}
	}

	// Snapshot для event за пределами мьютекса
	var deferredEvent map[string]any
	var deferredJoinEvent map[string]any
	var playbackData []byte

	s.mu.Lock()
	defer s.mu.Unlock()

	// Echo prevention: не воспроизводить собственные фреймы
	myID, _ := s.sess.PeerID()
	if fromPubKey == myID {
		return
	}

	// --- Profiling: recv counter + jitter + DFNP latency (debug-only) ---
	if voiceDebug && hasProbe && probeNs > 0 {
		s.prof.latProbeCount++
		s.prof.latProbeSumNs += probeNs
		if probeNs > s.prof.latProbeMaxNs {
			s.prof.latProbeMaxNs = probeNs
		}
		if s.prof.latProbeMinNs < 0 || probeNs < s.prof.latProbeMinNs {
			s.prof.latProbeMinNs = probeNs
		}
	}
	s.prof.recvFrames++
	if !s.prof.lastRecvTime.IsZero() {
		delta := recvStart.Sub(s.prof.lastRecvTime).Seconds() * 1000 // ms
		s.prof.recvIntervals = append(s.prof.recvIntervals, delta)
		if len(s.prof.recvIntervals) > 50 {
			s.prof.recvIntervals = s.prof.recvIntervals[1:]
		}
		// Jitter = stddev of intervals
		if len(s.prof.recvIntervals) >= 10 {
			var sum float64
			for _, d := range s.prof.recvIntervals {
				sum += d
			}
			mean := sum / float64(len(s.prof.recvIntervals))
			var sqSum float64
			for _, d := range s.prof.recvIntervals {
				diff := d - mean
				sqSum += diff * diff
			}
			s.prof.lastRecvJitter = math.Sqrt(sqSum / float64(len(s.prof.recvIntervals)))
		}
	}
	s.prof.lastRecvTime = recvStart

	voiceFramesReceived++
	if voiceFramesReceived%250 == 1 { // log every ~5 seconds
		s.log.Debug("🔊 voice frame received",
			slog.String("from", safeShort(fromPubKey)),
			slog.Int("size", len(pcmData)),
			slog.Int("total", voiceFramesReceived),
			slog.Bool("playback_active", s.playback != nil && s.playback.IsActive()))
	}

	if s.activeGroupID == "" || s.playback == nil {
		return
	}

	// Check if this peer is a participant.
	// Safety net: если пир известен в канале через gossip (channelPeers),
	// но voice_join ещё не обработан — авто-промоутим на первом фрейме.
	if !s.participants[fromPubKey] {
		if peers, ok := s.channelPeers[s.activeGroupID]; ok && peers[fromPubKey] {
			s.participants[fromPubKey] = true
			s.log.Info("🎧 auto-promote peer via voice_frame",
				slog.String("peer", safeShort(fromPubKey)),
				slog.String("group", safeShort8(s.activeGroupID)))
			deferredJoinEvent = map[string]any{
				"groupId":  s.activeGroupID,
				"peerId":   fromPubKey,
				"wasKnown": false,
			}
		} else {
			s.prof.dropNoPeer++
			return
		}
	}

	// Deafened — не воспроизводить никого
	if s.deafened {
		s.prof.dropMuted++
		return
	}

	// Opus decode: если данные < 640 bytes — это Opus, иначе raw PCM (backward compat).
	// recoveredFrames — PCM фреймы, восстановленные через FEC (для одиночного
	// пропуска) или PLC (для множественного), которые должны быть сыграны ПЕРЕД
	// текущим фреймом чтобы сохранить хронологию.
	var recoveredFrames [][]byte
	if len(pcmData) < codec.FrameSize*2 {
		dec, hasDec := s.opusDecoders[fromPubKey]
		if !hasDec {
			var decErr error
			dec, decErr = codec.NewDecoder()
			if decErr != nil {
				s.log.Warn("opus decoder failed", slog.String("peer", safeShort(fromPubKey)), slog.String("error", decErr.Error()))
				return
			}
			s.opusDecoders[fromPubKey] = dec
		}

		// Парсим VOX1-обёртку если она есть. Legacy-отправитель (v0.00.000.0020
		// и ниже) шлёт raw opus — тогда просто декодируем без FEC recovery.
		opusBytes, seq, hasSeq := unwrapVOX1(pcmData)

		if hasSeq {
			st := s.peerRxSeq[fromPubKey]
			if st.hasLast {
				// gap в uint16 пространстве с учётом wraparound
				gap := int32(seq) - int32(st.lastSeq)
				if gap < -32000 {
					gap += 65536
				} else if gap > 32000 {
					gap -= 65536
				}
				switch {
				case gap <= 0:
					// дубликат или out-of-order — пропускаем
					s.prof.seqSkips++
					if s.prof.seqSkips%100 == 1 {
						s.log.Warn("VOX1 seq skip",
							slog.String("peer", safeShort(fromPubKey)),
							slog.Int("gap", int(gap)),
							slog.Int("recv_seq", int(seq)),
							slog.Int("last_seq", int(st.lastSeq)),
							slog.Int64("total_skips", s.prof.seqSkips))
					}
					return
				case gap == 1:
					// в порядке — нормальный декод ниже
				case gap == 2:
					// один фрейм потерян — восстанавливаем через LBRR/FEC
					// копию из текущего пакета. Opus требует fec=1 при
					// декоде именно того пакета, который ПОСЛЕДОВАЛ за
					// пропущенным.
					if recovered, err := dec.Decode(opusBytes, true); err == nil {
						recoveredFrames = append(recoveredFrames, recovered)
					} else if plc, plcErr := dec.DecodePLC(); plcErr == nil {
						recoveredFrames = append(recoveredFrames, plc)
					}
				default:
					// gap > 2: FEC покрывает только 1 кадр назад, остальное
					// заполняем PLC. Ограничиваем 10 фреймами (200ms),
					// иначе при длительном отключении забьём буфер.
					missing := int(gap) - 1
					if missing > 10 {
						missing = 10
					}
					for i := 0; i < missing; i++ {
						if plc, err := dec.DecodePLC(); err == nil {
							recoveredFrames = append(recoveredFrames, plc)
						}
					}
				}
			}
			st.lastSeq = seq
			st.hasLast = true
			s.peerRxSeq[fromPubKey] = st
		}

		// Пустой payload после NXV — отправитель использует Opus DTX и не emit'ил
		// кадр для тишины (или легаси-отправитель с багом). Декодим через PLC,
		// иначе opus_decode вернёт ошибку и фрейм просто пропадёт.
		var decoded []byte
		var decErr error
		if len(opusBytes) == 0 {
			decoded, decErr = dec.DecodePLC()
		} else {
			decoded, decErr = dec.Decode(opusBytes, false)
		}
		if decErr != nil {
			s.prof.decodeErrors++
			// Reset decoder — может быть в bad state (corrupt internal), новый
			// создастся на следующем фрейме. Диагностический лог раз в 100
			// ошибок чтобы не спамить.
			dec.Close()
			delete(s.opusDecoders, fromPubKey)
			if s.prof.decodeErrors%100 == 1 {
				s.log.Warn("opus decode error (decoder reset)",
					slog.String("peer", safeShort(fromPubKey)),
					slog.Int("opus_size", len(opusBytes)),
					slog.String("err", decErr.Error()),
					slog.Int64("total_errors", s.prof.decodeErrors))
			}
			return
		}
		pcmData = decoded
	}

	// Apply per-peer volume × master voice volume
	vol, ok := s.peerVolumes[fromPubKey]
	if !ok {
		vol = 1.0
	}
	vol *= s.masterVoiceVolume
	if vol == 0 {
		s.prof.dropMuted++
		return // muted
	}

	s.peerLastFrame[fromPubKey] = time.Now()

	// Detect speaking — RMS + hysteresis + decay
	wasSpeaking := s.peerSpeaking[fromPubKey]
	var sumSq int64
	sampleCount := len(pcmData) / 2
	for i := 0; i < len(pcmData)-1; i += 2 {
		sample := int16(pcmData[i]) | int16(pcmData[i+1])<<8
		sumSq += int64(sample) * int64(sample)
	}
	rms := math.Sqrt(float64(sumSq) / float64(sampleCount))

	if rms > speakingRMSThreshold {
		s.speakDecay[fromPubKey] = 15 // 15 frames × 20ms = 300ms decay
		s.peerSpeaking[fromPubKey] = true
	} else if s.speakDecay[fromPubKey] > 0 {
		s.speakDecay[fromPubKey]--
	} else {
		s.peerSpeaking[fromPubKey] = false
	}
	speaking := s.peerSpeaking[fromPubKey]

	if speaking != wasSpeaking {
		deferredEvent = map[string]any{
			"peerId":   fromPubKey,
			"speaking": speaking,
		}
	}

	// Apply volume scaling and store in mix buffer (mixing happens in audioLoop)
	if vol != 1.0 {
		adjusted := make([]byte, len(pcmData))
		copy(adjusted, pcmData)
		for i := 0; i < len(adjusted)-1; i += 2 {
			sample := int16(adjusted[i]) | int16(adjusted[i+1])<<8
			v := float32(sample) * vol
			if v > 32767 {
				v = 32767
			}
			if v < -32768 {
				v = -32768
			}
			s16 := int16(v)
			adjusted[i] = byte(s16)
			adjusted[i+1] = byte(s16 >> 8)
		}
		playbackData = adjusted
	} else {
		// Copy to avoid referencing the DataChannel buffer
		playbackData = make([]byte, len(pcmData))
		copy(playbackData, pcmData)
	}

	// FEC/PLC recovered frames идут в очередь ПЕРЕД текущим фреймом, чтобы
	// сохранить хронологию. Применяем ту же громкость что и к текущему.
	for _, rec := range recoveredFrames {
		recData := rec
		if vol != 1.0 {
			recData = make([]byte, len(rec))
			copy(recData, rec)
			for i := 0; i < len(recData)-1; i += 2 {
				sample := int16(recData[i]) | int16(recData[i+1])<<8
				v := float32(sample) * vol
				if v > 32767 {
					v = 32767
				}
				if v < -32768 {
					v = -32768
				}
				s16 := int16(v)
				recData[i] = byte(s16)
				recData[i+1] = byte(s16 >> 8)
			}
		} else {
			recData = make([]byte, len(rec))
			copy(recData, rec)
		}
		s.mixBuf[fromPubKey] = append(s.mixBuf[fromPubKey], recData)
		if voiceDebug {
			s.mixBufTs[fromPubKey] = append(s.mixBufTs[fromPubKey], time.Now().UnixNano())
		}
	}

	// Append to per-peer mix queue — audioLoop will mix all peers and write to playback
	s.mixBuf[fromPubKey] = append(s.mixBuf[fromPubKey], playbackData)
	if voiceDebug {
		s.mixBufTs[fromPubKey] = append(s.mixBufTs[fromPubKey], time.Now().UnixNano())
		if qLen := len(s.mixBuf[fromPubKey]); qLen > s.prof.mixQueuePeak {
			s.prof.mixQueuePeak = qLen
		}
	}

	// Bound jitter buffer: при burst'e через TURN relay очередь может распухнуть
	// до 16+ фреймов (320+ ms added latency). Ограничиваем сверху — дропаем старые,
	// сохраняем последние maxJitterFrames-1. Opus PLC восстановит пропуски.
	if q := s.mixBuf[fromPubKey]; len(q) > maxJitterFrames {
		keep := maxJitterFrames
		drop := len(q) - keep
		s.mixBuf[fromPubKey] = q[drop:]
		if voiceDebug {
			if tsq, ok := s.mixBufTs[fromPubKey]; ok && len(tsq) >= drop {
				s.mixBufTs[fromPubKey] = tsq[drop:]
			}
			s.prof.mixOverflowDrops += drop
		}
	}
	// NB: recvFrames уже инкрементирован в начале handleVoiceFrame, второй раз не считаем.

	// Profiling: замер полного времени handleVoiceFrame под удерживаемым мьютексом.
	// defer s.mu.Unlock() в начале функции гарантирует освобождение при любом пути
	// выхода (включая панику opus-декодера), чего не делал ручной Unlock-паттерн.
	if recvElapsed := time.Since(recvStart); recvElapsed > s.prof.maxRecvTime {
		s.prof.maxRecvTime = recvElapsed
	}

	if deferredEvent != nil {
		s.network.emitEvent("vox:voice-activity", deferredEvent)
	}
	if deferredJoinEvent != nil {
		s.network.emitEvent("vox:voice-join", deferredJoinEvent)
	}
}
