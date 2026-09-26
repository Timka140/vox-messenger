package services

import (
	"encoding/json"
	"log/slog"
	"math"
	"vox_app/internal/audio/vad"
	"vox_app/internal/codec"
	"time"
)

// voiceJoinFastRetries — сколько повторов voice_join идут часто (3 с): этого
// хватает на потерянный первый join; дальше раз в 30 с — пока в канале никого,
// повтор нужен только чтобы найти вошедших позже, а не засыпать группу.
const voiceJoinFastRetries = 5

// voiceJoinRetryInterval — пауза перед повтором voice_join номер attempt (с 0).
func voiceJoinRetryInterval(attempt int) time.Duration {
	if attempt < voiceJoinFastRetries {
		return 3 * time.Second
	}
	return 30 * time.Second
}

// audioLoop — основной capture/send/mix цикл голосового канала.
//
// Запускается из JoinGroupVoice как goroutine, останавливается через
// close(s.stopCh) в leaveInternal. Тикает каждые 20ms (50 Hz = native
// для 16kHz @ 320 samples).
//
// Этапы каждой итерации:
//
//   1. Snapshot state ПОД s.mu:
//      · capture.StreamRead(320)
//      · RMS + speaking detection
//      · Сбор sendPeers из participants
//      · Профайлинг: maxLoopTime, retry voice_join planning
//      · Каждые 5s — dumpVoiceProfileLocked
//
//   2. Lock released → opus encode → network sends (вне лока, чтобы
//      не блокировать handleVoiceFrame)
//
//   3. Re-lock для аудио-микса:
//      · Берём по 1 фрейму из mixBuf каждого пира
//      · Суммируем int32, клампим в int16
//      · pb.Write(mixedPCM) (вне лока)
//
//   4. Lock released → emit speaking events
//
//   5. NS A/B stats CSV row каждые 200ms (только voiceDebug)
//
// На panic — recover, leaveInternal под локом. Без recover один баг
// в opus_decode/mix мог бы убить весь голосовой стек.
func (s *VoiceService) audioLoop() {
	// stopCh фиксируем один раз под локом. Раньше audioLoop читал s.stopCh
	// без лока каждую итерацию, гонясь с обнулением в leaveInternal.
	s.mu.Lock()
	stop := s.stopCh
	s.mu.Unlock()

	defer func() {
		r := recover()
		// В любом случае снимаем себя из audioWG ДО возможного teardown:
		// leaveInternal делает audioWG.Wait(), и если бы мы держали счётчик,
		// вызванный из паники teardown задедлочился бы сам на себе.
		s.audioWG.Done()
		if r == nil {
			return // нормальный выход по stopCh
		}
		s.log.Error("audioLoop panic, выход из голосового канала", slog.Any("recover", r))
		s.mu.Lock()
		s.leaveInternalNoWait()
		s.mu.Unlock()
	}()

	ticker := time.NewTicker(20 * time.Millisecond) // 20ms = 320 samples @ 16kHz
	defer ticker.Stop()

	myID, _ := s.sess.PeerID()
	frameCount := 0
	sentCount := 0
	var lastLog time.Time
	var lastSpeakCheck time.Time
	var lastVoiceJoinRetry time.Time
	var joinRetryAttempt int // повторов voice_join подряд, пока в канале никого (voiceJoinRetryInterval)
	// micCarry накапливает недобор сэмплов: StreamRead при джиттере Windows-
	// тикера отдаёт < 320 сэмплов. Раньше неполный кадр уходил в opus.Encode,
	// падал по размеру и слался как СЫРОЙ PCM → у получателя decode error и
	// потеря 10-20ms речи. Теперь копим до ровно 640 байт (320 сэмплов).
	var micCarry []byte
	const frameBytes = 640
	// txSeq — локальный счётчик кадров (был полем s.txSeq: инкремент вне лока
	// гонялся с обнулением в leaveInternal). Каждая сессия = свой audioLoop =
	// свой счётчик с нуля, поле в структуре не нужно.
	var txSeq uint16

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}

		// --- Snapshot state under lock, release for NS Process ---
		loopStart := time.Now()
		s.mu.Lock()
		if s.capture == nil || s.activeGroupID == "" {
			s.mu.Unlock()
			return
		}

		// Always read to keep stream_pos advancing (prevents buffer accumulation on mute)
		raw := s.capture.StreamRead(320) // 320 samples = 20ms @ 16kHz = 640 bytes

		// Carry-аккумуляция: собираем ровно один кадр 640B из потенциально
		// неполных чтений. Остаток переносится на следующую итерацию.
		var pcm []byte
		if s.muted {
			// На mute звук не шлём — сбрасываем накопленное, чтобы после
			// unmute не выстрелить устаревшими сэмплами.
			micCarry = micCarry[:0]
		} else if len(raw) > 0 {
			micCarry = append(micCarry, raw...)
			// Защита от неограниченного роста, если NS надолго затормозит.
			if len(micCarry) > frameBytes*8 {
				micCarry = micCarry[len(micCarry)-frameBytes*8:]
			}
			if len(micCarry) >= frameBytes {
				pcm = make([]byte, frameBytes) // свежий буфер: не алиасит carry
				copy(pcm, micCarry[:frameBytes])
				micCarry = append(micCarry[:0], micCarry[frameBytes:]...)
			}
		}

		// Snapshot NS pointers под lock и отпускаем lock на время Process.
		// NS.Process single-caller (только audioLoop goroutine), поэтому безопасно.
		// Без этого: GTCRN cold-start inference (187ms spike в логах 0066) держал
		// s.mu → handleVoiceFrame не мог добавить в mixBuf → mix+playback тоже
		// висел → собеседник слышал тишину и на своей стороне тоже underrun.
		fe := s.fastEnhancer
		g := s.gtcrn
		s.mu.Unlock()

		if pcm != nil && len(pcm) == 640 {
			if fe != nil {
				pcm = fe.Process(pcm)
			} else if g != nil {
				pcm = g.Process(pcm)
			}
		}

		s.mu.Lock()
		if s.capture == nil || s.activeGroupID == "" {
			s.mu.Unlock()
			return
		}

		var sendPeers []string // snapshot of participants to send to (outside lock)
		shouldSend := false
		opusEnc := s.opusEncoder   // snapshot encoder pointer (only used by audioLoop)
		sfuActive := s.sfuActive   // SFU mode snapshot
		sfuConn := s.sfuConn       // SFU connection snapshot

		// VAD snapshot: активен только если включён пользователем И детектор
		// загрузился. Async-loading может быть ещё в процессе при старте сессии.
		vadActive := s.vadEnabled && s.vadDetector != nil

		var localSpeakEvent map[string]any

		if !s.muted && pcm != nil && len(pcm) > 0 {
			frameCount++
			s.prof.captureFrames++

			// RMS фрейма — для UI speaking detection (speakDecay/peerSpeaking)
			var sumSq int64
			sampleCount := len(pcm) / 2
			for i := 0; i < len(pcm)-1; i += 2 {
				sample := int16(pcm[i]) | int16(pcm[i+1])<<8
				sumSq += int64(sample) * int64(sample)
			}
			rms := math.Sqrt(float64(sumSq) / float64(sampleCount))

			// Echo gate удалён: в double-talk (собеседник громко говорит пока
			// юзер тоже говорит) ducking gate давал false positive и дропал
			// речь юзера. Суммарно терялось ~3 сек речи за минуту разговора.
			// При появлении реальных проблем с эхом рассматривать WebRTC AEC.
			for peerID := range s.participants {
				if peerID != myID {
					sendPeers = append(sendPeers, peerID)
				}
			}
			shouldSend = true

			// Speaking detection для локального пользователя
			wasSpeaking := s.peerSpeaking[myID]
			if rms > speakingRMSThreshold {
				s.speakDecay[myID] = 15
				s.peerSpeaking[myID] = true
			} else if s.speakDecay[myID] > 0 {
				s.speakDecay[myID]--
			} else {
				s.peerSpeaking[myID] = false
			}
			if s.peerSpeaking[myID] != wasSpeaking {
				localSpeakEvent = map[string]any{
					"peerId":   myID,
					"speaking": s.peerSpeaking[myID],
				}
			}
		} else if s.muted && s.peerSpeaking[myID] {
			// Замьючен — сбрасываем speaking
			s.peerSpeaking[myID] = false
			s.speakDecay[myID] = 0
			localSpeakEvent = map[string]any{
				"peerId":   myID,
				"speaking": false,
			}
		}

		// Сбрасываем speaking для пиров, от которых нет фреймов >500ms
		var speakResetEvents []map[string]any
		if time.Since(lastSpeakCheck) > 500*time.Millisecond {
			lastSpeakCheck = time.Now()
			for pid, last := range s.peerLastFrame {
				if s.peerSpeaking[pid] && lastSpeakCheck.Sub(last) > 500*time.Millisecond {
					s.peerSpeaking[pid] = false
					s.speakDecay[pid] = 0
					speakResetEvents = append(speakResetEvents, map[string]any{
						"peerId":   pid,
						"speaking": false,
					})
				}
			}
		}

		// --- VAD: подготовка окон под локом, инференс — ВНЕ лока ---
		// Ring накапливает 320 sample per tick; при >=512 (Silero v5) готово окно.
		// Раньше vadDetector.Infer вызывался прямо здесь под s.mu — ONNX-спайк
		// (десятки мс) держал лок → handleVoiceFrame всех пиров стоял → mixBuf
		// пустел → треск/провалы у всех. Теперь под локом только копируем окна,
		// Infer гоняем после Unlock (детектор — single-caller из audioLoop;
		// leaveInternal ждёт audioWG перед Close, поэтому UAF нет).
		var vadWindows [][]int16
		vadDet := s.vadDetector
		if vadActive && pcm != nil && len(pcm) > 0 {
			nSamples := len(pcm) / 2
			room := len(s.vadRingBuf) - s.vadRingLen
			if nSamples > room {
				nSamples = room
			}
			for i := 0; i < nSamples; i++ {
				s.vadRingBuf[s.vadRingLen] = int16(pcm[i*2]) | int16(pcm[i*2+1])<<8
				s.vadRingLen++
			}
			for s.vadRingLen >= vad.WindowSize {
				w := make([]int16, vad.WindowSize)
				copy(w, s.vadRingBuf[:vad.WindowSize])
				vadWindows = append(vadWindows, w)
				if s.vadRingLen > vad.WindowSize {
					copy(s.vadRingBuf[:], s.vadRingBuf[vad.WindowSize:s.vadRingLen])
				}
				s.vadRingLen -= vad.WindowSize
			}
		}

		// Profiling: замер времени итерации (под мьютексом)
		loopElapsed := time.Since(loopStart)
		if loopElapsed > s.prof.maxLoopTime {
			s.prof.maxLoopTime = loopElapsed
		}

		// Retry voice_join: если participants пуст >2с после старта — повторяем
		// broadcast с нарастающим интервалом (voiceJoinRetryInterval). Раньше —
		// каждые 3 с бесконечно всем участникам группы, в т.ч. сидящим в голосе
		// другой группы.
		var retryPeers []string
		retryGroupID := s.activeGroupID
		if len(s.participants) > 0 {
			joinRetryAttempt = 0
		}
		if len(s.participants) == 0 && time.Since(s.prof.sessionStart) > 2*time.Second &&
			time.Since(lastVoiceJoinRetry) > voiceJoinRetryInterval(joinRetryAttempt) {
			lastVoiceJoinRetry = time.Now()
			joinRetryAttempt++
			if s.groups != nil {
				members := s.groups.GetGroupMemberIDs(retryGroupID)
				var connectedCount, offlineCount int
				for _, pid := range members {
					if pid == myID {
						continue
					}
					if s.network.IsP2PConnected(pid) {
						retryPeers = append(retryPeers, pid)
						connectedCount++
					} else {
						offlineCount++
					}
				}
				if connectedCount == 0 && offlineCount > 0 {
					s.log.Debug("🎧 voice_join retry: нет подключённых пиров",
						slog.String("group", safeShort8(retryGroupID)),
						slog.Int("members", len(members)),
						slog.Int("offline", offlineCount))
				}
			}
		}

		// Debug log + profile dump every 5 seconds
		if time.Since(lastLog) > 5*time.Second {
			s.dumpVoiceProfileLocked()
			lastLog = time.Now()
		}
		// VAD решение снепшотится до Unlock — использует vadLastProb,
		// обновлённый выше этой же итерацией. vadActive гарантирует что
		// детектор жив и включён.
		vadIsSpeech := !vadActive || (s.vadLastProb >= s.vadThreshold)
		s.mu.Unlock()
		// --- Lock released ---

		// VAD inference ВНЕ лока: ONNX-спайк больше не блокирует приём звука
		// других пиров. vadLastProb обновляется для СЛЕДУЮЩИХ кадров (VAD —
		// hint, одно-кадровая задержка допустима; фреймы никогда не дропаем).
		if vadDet != nil && len(vadWindows) > 0 {
			for _, w := range vadWindows {
				inferStart := time.Now()
				prob, err := vadDet.Infer(w)
				inferElapsed := time.Since(inferStart).Nanoseconds()
				s.mu.Lock()
				if inferElapsed > s.prof.vadMaxInferNs {
					s.prof.vadMaxInferNs = inferElapsed
				}
				if err != nil {
					s.prof.vadInferenceErr++
					prob = 1.0 // fail-safe: считаем речью
				}
				s.vadLastProb = prob
				s.mu.Unlock()
			}
		}

		// Opus encode OUTSIDE the lock (encoder is only used by audioLoop goroutine)
		var sendData []byte
		if shouldSend && pcm != nil {
			if opusEnc != nil {
				if encoded, encErr := opusEnc.Encode(pcm); encErr == nil {
					if len(encoded) == 0 {
						// Opus DTX: энкодер не emit'ил пакет (глубокая тишина).
						// Не шлём и не двигаем seq — получатель увидит seq как
						// будто кадра не было. На переходе silence→speech
						// возникнет gap>=2 и существующий PLC-путь заполнит
						// до 10 фреймов (200ms артефактов), что приемлемо.
						sendData = nil
					} else {
						// VOX1 обёртка с seq номером — обязательна для FEC/PLC
						// recovery на приёме. 6 байт overhead на фрейм.
						sendData = wrapVOX1(encoded, txSeq)
						txSeq++
					}
				} else {
					// pcm теперь всегда ровно 640B (carry-буфер), поэтому
					// Encode-ошибка — реальный сбой кодека. Сырой PCM без VOX1
					// приёмник принял бы за opus и словил decode error, поэтому
					// лучше дропнуть кадр (PLC заполнит).
					sendData = nil
				}
			} else {
				sendData = nil
			}
			// Debug-probe: wire-latency через DFNP header.
			// В release-сборке voiceDebug=false → ветка compile-time удаляется.
			if voiceDebug && sendData != nil {
				sendData = wrapDFNP(sendData, time.Now().UnixNano())
			}
		}

		// --- VAD gate: конвертируем sendData в список фреймов для отправки ---
		// Безусловные инварианты:
		//   * sendData != nil → это свеже-закодированный кадр с уже инкрементнутым seq.
		//   * vadActive=false → pass-through, framesToSend = [sendData] (как раньше).
		//   * vadActive=true && isSpeech → flush pre-roll (oldest first) + sendData.
		//   * vadActive=true && !isSpeech && hangover>0 → hangover-- → считается речью.
		//   * vadActive=true && !isSpeech && hangover=0 → stash в pre-roll ring, skip send.
		var framesToSend [][]byte
		if sendData != nil {
			if !vadActive {
				framesToSend = [][]byte{sendData}
			} else {
				s.mu.Lock()
				effectiveSpeech := vadIsSpeech
				if effectiveSpeech {
					s.vadHangover = vadHangoverFrames
				} else if s.vadHangover > 0 {
					s.vadHangover--
					effectiveSpeech = true
				}
				if effectiveSpeech {
					if s.vadPreRollCount > 0 {
						oldest := (s.vadPreRollHead - s.vadPreRollCount + vadPreRollSize) % vadPreRollSize
						framesToSend = make([][]byte, 0, s.vadPreRollCount+1)
						for i := 0; i < s.vadPreRollCount; i++ {
							idx := (oldest + i) % vadPreRollSize
							framesToSend = append(framesToSend, s.vadPreRoll[idx])
							s.vadPreRoll[idx] = nil
						}
						s.vadPreRollCount = 0
					}
					framesToSend = append(framesToSend, sendData)
				} else {
					// Stash в pre-roll ring (evict oldest при полном кольце).
					stash := make([]byte, len(sendData))
					copy(stash, sendData)
					s.vadPreRoll[s.vadPreRollHead] = stash
					s.vadPreRollHead = (s.vadPreRollHead + 1) % vadPreRollSize
					if s.vadPreRollCount < vadPreRollSize {
						s.vadPreRollCount++
					}
					s.prof.vadSkipped++
				}
				s.mu.Unlock()
			}
		}

		// Network sends OUTSIDE the lock — prevents blocking handleVoiceFrame.
		// Счётчики sent/sendErr накапливаются локально и применяются к s.prof
		// одним Lock'ом после цикла — убирает per-peer reentry lock/unlock.
		// batchSentBytes — реальные wire-байты (frameSize × число успешных отправок).
		// В mesh это N × frameSize, в SFU — 1 × frameSize на кадр.
		// При VAD framesToSend может содержать несколько кадров (flush pre-roll + current).
		var batchSent, batchSendErr, batchSfuSent, batchSentBytes int64
		for _, frame := range framesToSend {
			frameSize := int64(len(frame))
			if sfuActive && sfuConn != nil {
				sfuConn.send(frame)
				batchSent += int64(len(sendPeers))
				sentCount += len(sendPeers)
				batchSfuSent++
				batchSentBytes += frameSize
			} else {
				for _, peerID := range sendPeers {
					// Skip silently если Voice DC ещё не открыт — это ожидаемо
					// в первую секунду после P2P connected (handshake DTLS/SCTP)
					if !s.network.IsVoiceDCOpen(peerID) {
						continue
					}
					if err := s.network.SendVoiceFrame(peerID, frame); err == nil {
						sentCount++
						batchSent++
						batchSentBytes += frameSize
					} else {
						batchSendErr++
						if frameCount%250 == 1 {
							s.log.Debug("🔇 voice frame send error",
								slog.String("peer", safeShort(peerID)),
								slog.String("error", err.Error()))
						}
					}
				}
			}
		}
		if batchSent > 0 || batchSendErr > 0 || batchSfuSent > 0 {
			s.mu.Lock()
			s.prof.sentFrames += batchSent
			s.prof.sendErrors += batchSendErr
			s.prof.sfuSentFrames += batchSfuSent
			s.prof.sentBytes += batchSentBytes
			s.mu.Unlock()
		}

		// Retry voice_join outside lock
		if len(retryPeers) > 0 {
			retryMsg := P2PMessage{Type: "voice_join", GroupID: retryGroupID, Content: myID, Nickname: s.network.MyNickname()}
			retryData, _ := json.Marshal(retryMsg)
			for _, pid := range retryPeers {
				if err := s.network.SendP2PData(pid, retryData); err != nil {
					s.log.Debug("🎧 voice_join retry FAILED",
						slog.String("peer", safeShort(pid)),
						slog.String("error", err.Error()))
				} else {
					s.log.Info("🎧 voice_join RETRY sent",
						slog.String("peer", safeShort(pid)),
						slog.String("group", safeShort8(retryGroupID)))
				}
			}
		}

		// --- Audio mixing: берём по одному фрейму от каждого пира, суммируем, пишем в playback ---
		// ВАЖНО: audioLoop должен писать в ring buffer КАЖДЫЙ tick когда сессия
		// активна, даже если данных от пиров нет (silence-fill). Read и write
		// rate оба 16k/sec; gap в ring buffer размывается при любом пропущенном
		// тике (Windows ticker jitter до 15ms), что даёт слышимые silence-gap'ы
		// у собеседника. См. комментарий у pre-fill в voice.go JoinGroupVoice.
		s.mu.Lock()
		var nowNs int64
		if voiceDebug {
			nowNs = time.Now().UnixNano()
		}
		var writePCM []byte
		if s.playback != nil {
			var mixed [codec.FrameSize]int32 // 320 samples, int32 для суммирования без overflow
			peerCount := 0
			for pid, queue := range s.mixBuf {
				if len(queue) == 0 {
					delete(s.mixBuf, pid)
					if voiceDebug {
						delete(s.mixBufTs, pid)
					}
					continue
				}
				// Adaptive catch-up: если очередь распухла выше mixTargetFrames
				// (после блокирующего пика handleVoiceFrame, который вмиг
				// закидывает накопившиеся фреймы) — дропаем старейшие чтобы
				// вернуть latency. Без этого audioLoop тащит по 1 кадру/tick =
				// ровно столько сколько recv приносит, и распухшая очередь не
				// сокращается никогда → постоянный рассинхрон с другими peer'ами.
				if len(queue) > mixTargetFrames {
					drop := len(queue) - mixTargetFrames
					queue = queue[drop:]
					s.mixBuf[pid] = queue
					if voiceDebug {
						if tsQueue, ok := s.mixBufTs[pid]; ok && len(tsQueue) >= drop {
							s.mixBufTs[pid] = tsQueue[drop:]
						}
					}
				}
				// Берём первый фрейм из очереди
				frame := queue[0]
				// Debug-probe: параллельная очередь ts для dwell-time (recv→mix-out)
				if voiceDebug {
					if tsQueue, ok := s.mixBufTs[pid]; ok && len(tsQueue) > 0 {
						dwellNs := nowNs - tsQueue[0]
						s.prof.mixDwellCount++
						s.prof.mixDwellSumNs += dwellNs
						if dwellNs > s.prof.mixDwellMaxNs {
							s.prof.mixDwellMaxNs = dwellNs
						}
						if len(tsQueue) == 1 {
							delete(s.mixBufTs, pid)
						} else {
							s.mixBufTs[pid] = tsQueue[1:]
						}
					}
				}
				if len(queue) == 1 {
					delete(s.mixBuf, pid)
				} else {
					s.mixBuf[pid] = queue[1:]
				}
				if len(frame) >= codec.FrameSize*2 {
					for i := 0; i < codec.FrameSize; i++ {
						sample := int16(frame[i*2]) | int16(frame[i*2+1])<<8
						mixed[i] += int32(sample)
					}
					peerCount++
				}
			}
			if peerCount > 0 {
				writePCM = make([]byte, codec.FrameSize*2)
				for i := 0; i < codec.FrameSize; i++ {
					v := mixed[i]
					if v > 32767 {
						v = 32767
					}
					if v < -32768 {
						v = -32768
					}
					writePCM[i*2] = byte(v)
					writePCM[i*2+1] = byte(v >> 8)
				}
				s.prof.playbackWrites++
			} else if len(s.participants) > 0 {
				// Есть хотя бы один другой участник (в participants хранятся
				// только peer'ы, не self), но никто не прислал frame на этот
				// tick — типично при direct LAN, где mixBuf пустеет каждый
				// tick. Пишем silence, чтобы ring buffer gap не схлопнулся.
				writePCM = make([]byte, codec.FrameSize*2)
				s.prof.playbackSilence++
			}
		}
		pb := s.playback
		s.mu.Unlock()
		if writePCM != nil && pb != nil {
			pb.Write(writePCM)
		}

		// Emit speaking events outside lock
		for _, evt := range speakResetEvents {
			s.network.emitEvent("vox:voice-activity", evt)
		}
		if localSpeakEvent != nil {
			s.network.emitEvent("vox:voice-activity", localSpeakEvent)
		}

		// NS A/B stats CSV row каждые nsStatsLogPeriod тиков (200ms).
		// Полностью compile-time eliminated в release-сборке (voiceDebug=false).
		// Файл, capture и время старта снапшотим под локом — leaveInternal
		// может параллельно обнулить s.nsStatsLog и s.capture.
		if voiceDebug && frameCount%nsStatsLogPeriod == 0 {
			s.mu.Lock()
			nsLog := s.nsStatsLog
			nsStart := s.nsStatsLogStart
			capRef := s.capture
			s.mu.Unlock()
			if nsLog != nil && capRef != nil {
				stats := capRef.TakeNSStats()
				writeNSStatsRow(nsLog, nsStart, stats)
			}
		}
	}
}
