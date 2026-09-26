package files

import (
	"log/slog"
	"sync/atomic"
	"time"
)

// transferStats — счётчики передачи файлов (обе стороны). Инкременты только
// atomic из onChunkData/seeder; statsLoop раз в statsInterval печатает
// дельту. По образцу ScreenShareService.statsLoop: без этой строки скорость
// файлов мерялась только внешними счётчиками сетевого адаптера.
type transferStats struct {
	bytesIn    atomic.Int64 // принято payload-байт (после SHA-верификации)
	bytesOut   atomic.Int64 // отправлено байт chunk_data (frame целиком)
	chunksIn   atomic.Int64
	chunksOut  atomic.Int64
	naks       atomic.Int64 // получено CHUNK_NAK
	mismatches atomic.Int64 // SHA mismatch на приёме
	stalls     atomic.Int64 // stall-reset'ы pullLoop (pullStallTimeout)
}

const (
	statsInterval = 2 * time.Second
	// statsIdleTicks — сколько тиков подряд без активных downloads/uploads
	// живёт statsLoop, прежде чем выйти. Перезапускается ensureStatsLoop'ом
	// при следующей передаче.
	statsIdleTicks = 2
)

// ensureStatsLoop запускает statsLoop, если он ещё не крутится. Вызывать в
// начале любой передачи (startDownload, onChunkRequest).
func (s *FilesService) ensureStatsLoop() {
	if s.statsRunning.CompareAndSwap(false, true) {
		go s.statsLoop()
	}
}

// activeTransferPeers — пиры активных downloads/uploads и суммарный in-flight
// по downloads (под s.mu и st.mu).
func (s *FilesService) activeTransferPeers() (peers []string, inflight int, downloads, uploads int) {
	seen := map[string]bool{}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range s.downloads {
		st.mu.Lock()
		inflight += len(st.inFlight)
		st.mu.Unlock()
		if !seen[st.sourcePeer] {
			seen[st.sourcePeer] = true
			peers = append(peers, st.sourcePeer)
		}
	}
	for _, up := range s.uploads {
		up.mu.Lock()
		for pid := range up.peers {
			if !seen[pid] {
				seen[pid] = true
				peers = append(peers, pid)
			}
		}
		up.mu.Unlock()
	}
	return peers, inflight, len(s.downloads), len(s.uploads)
}

func (s *FilesService) statsLoop() {
	defer s.statsRunning.Store(false)
	ticker := time.NewTicker(statsInterval)
	defer ticker.Stop()

	prevIn := s.stats.bytesIn.Load()
	prevOut := s.stats.bytesOut.Load()
	prevTs := time.Now()
	idle := 0
	for range ticker.C {
		peers, inflight, nDown, nUp := s.activeTransferPeers()
		now := time.Now()
		dt := now.Sub(prevTs).Seconds()
		curIn := s.stats.bytesIn.Load()
		curOut := s.stats.bytesOut.Load()
		inKbps := float64(curIn-prevIn) * 8 / 1000 / dt
		outKbps := float64(curOut-prevOut) * 8 / 1000 / dt
		prevIn, prevOut, prevTs = curIn, curOut, now

		if nDown == 0 && nUp == 0 && curIn == prevIn && curOut == prevOut {
			idle++
			if idle >= statsIdleTicks {
				return
			}
			continue
		}
		idle = 0

		var maxBuffered uint64
		if s.network != nil {
			for _, pid := range peers {
				if b := s.network.AttachBufferedAmount(pid); b > maxBuffered {
					maxBuffered = b
				}
			}
		}
		s.log.Info("file transfer stats",
			slog.Float64("in_kbps", inKbps),
			slog.Float64("out_kbps", outKbps),
			slog.Int("inflight", inflight),
			slog.Int("downloads", nDown),
			slog.Int("uploads", nUp),
			slog.Uint64("attach_buffered", maxBuffered),
			slog.Int64("chunks_in", s.stats.chunksIn.Load()),
			slog.Int64("chunks_out", s.stats.chunksOut.Load()),
			slog.Int64("naks", s.stats.naks.Load()),
			slog.Int64("mismatches", s.stats.mismatches.Load()),
			slog.Int64("stalls", s.stats.stalls.Load()))
	}
}
