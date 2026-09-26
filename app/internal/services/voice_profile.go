package services

import (
	"log/slog"
	"math"
	"time"
)

// dumpVoiceProfileLocked пишет снапшот профайлера голосового канала в лог
// и сбрасывает debug-счётчики wire-latency / mix-dwell / mix-peak.
//
// ВАЖНО: вызывается ТОЛЬКО из audioLoop под удерживаемым s.mu. Метод читает
// и мутирует s.prof (счётчики дебага сбрасываются), а также читает
// s.participants — всё должно быть защищено.
//
// Ранее был инлайн-блоком ~60 строк внутри audioLoop; выделен чтобы тот
// держался ближе к своему основному потоку (capture → gate → send → mix).
// Поведение байт-в-байт совпадает с инлайн-версией.
func (s *VoiceService) dumpVoiceProfileLocked() {
	attrs := []any{
		slog.Int64("capture", s.prof.captureFrames),
		slog.Int64("sent", s.prof.sentFrames),
		slog.Int64("send_err", s.prof.sendErrors),
		slog.Int64("recv", s.prof.recvFrames),
		slog.Int64("pb_writes", s.prof.playbackWrites),
		slog.Int64("pb_silence", s.prof.playbackSilence),
		slog.Int64("drop_no_peer", s.prof.dropNoPeer),
		slog.Int64("drop_muted", s.prof.dropMuted),
		slog.Int64("decode_err", s.prof.decodeErrors),
		slog.Int64("seq_skip", s.prof.seqSkips),
		slog.String("max_loop", s.prof.maxLoopTime.String()),
		slog.String("max_recv", s.prof.maxRecvTime.String()),
		slog.Float64("recv_jitter_ms", math.Round(s.prof.lastRecvJitter*10)/10),
		slog.Int("participants", len(s.participants)),
		slog.String("session_dur", time.Since(s.prof.sessionStart).Truncate(time.Second).String()),
		slog.Int64("sfu_sent", s.prof.sfuSentFrames),
		slog.Int64("sfu_recv", s.prof.sfuRecvFrames),
		slog.Int64("vad_skipped", s.prof.vadSkipped),
		slog.Int64("vad_err", s.prof.vadInferenceErr),
		slog.Float64("vad_last_prob", math.Round(float64(s.vadLastProb)*1000)/1000),
		slog.Float64("vad_max_infer_ms", float64(s.prof.vadMaxInferNs)/1e6),
	}
	if voiceDebug {
		// Wire-latency stats (DFNP probe) — считаем и сбрасываем каждый цикл
		var wireAvgMs, wireMinMs, wireMaxMs float64
		var wireCount int64
		if s.prof.latProbeCount > 0 {
			wireCount = s.prof.latProbeCount
			wireAvgMs = float64(s.prof.latProbeSumNs/s.prof.latProbeCount) / 1e6
			wireMaxMs = float64(s.prof.latProbeMaxNs) / 1e6
			if s.prof.latProbeMinNs > 0 {
				wireMinMs = float64(s.prof.latProbeMinNs) / 1e6
			}
			s.prof.latProbeCount = 0
			s.prof.latProbeSumNs = 0
			s.prof.latProbeMaxNs = 0
			s.prof.latProbeMinNs = -1
		}
		// Mix dwell time stats
		var mixAvgMs, mixMaxMs float64
		if s.prof.mixDwellCount > 0 {
			mixAvgMs = float64(s.prof.mixDwellSumNs/s.prof.mixDwellCount) / 1e6
			mixMaxMs = float64(s.prof.mixDwellMaxNs) / 1e6
			s.prof.mixDwellCount = 0
			s.prof.mixDwellSumNs = 0
			s.prof.mixDwellMaxNs = 0
		}
		mixPeak := s.prof.mixQueuePeak
		s.prof.mixQueuePeak = 0
		attrs = append(attrs,
			slog.Int64("wire_n", wireCount),
			slog.Float64("wire_avg_ms", math.Round(wireAvgMs*10)/10),
			slog.Float64("wire_min_ms", math.Round(wireMinMs*10)/10),
			slog.Float64("wire_max_ms", math.Round(wireMaxMs*10)/10),
			slog.Float64("mix_dwell_avg_ms", math.Round(mixAvgMs*10)/10),
			slog.Float64("mix_dwell_max_ms", math.Round(mixMaxMs*10)/10),
			slog.Int("mix_peak", mixPeak),
		)
	}
	s.log.Info("🎙️ VOICE PROFILE", attrs...)
}
