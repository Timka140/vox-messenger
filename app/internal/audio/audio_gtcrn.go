//go:build nx_onnx

// GTCRN — streaming neural NS via ONNX, работает в spectrogram-domain.
//
// В отличие от FastEnhancer (wav→wav) GTCRN требует STFT/iSTFT вокруг inference:
//
//   pcm 320s → append_ring → chunk_512 → FFT(hann^0.5) →
//   [mix, caches] → Run() → [enh, new_caches] →
//   iFFT(hann^0.5) → overlap_add → pcm 320s
//
// n_fft=512, hop=256, sample_rate=16k. Окно `hann(512)^0.5` даёт точную
// реконструкцию при 50% overlap через overlap-add: сумма (hann^0.5)^2 на
// двух соседних окнах равна hann, окно-нормализация не нужна (sum of Hann
// ровно 1.0 при hop=N/2).
//
// Модель GTCRN ~48K параметров = ~500× легче FE Large, CPU < 1%.

package audio

import (
	_ "embed"
	"fmt"
	"log/slog"
	"math"
	"math/cmplx"
	"time"
	"vox_app/internal/ml/onnx"

	"github.com/madelynnblue/go-dsp/fft"
)

// GTCRNAvailable — true если бинарь собран с nx_onnx и ORT runtime загрузился.
// UI скрывает опцию GTCRN если false.
func GTCRNAvailable() bool { return onnx.Available() }

//go:embed gtcrn/gtcrn.onnx
var gtcrnModelDNS3 []byte

//go:embed gtcrn/gtcrn_simple.onnx
var gtcrnModelSimple []byte

// GTCRNVariant — какую из двух обученных моделей использовать.
type GTCRNVariant string

const (
	GTCRNDNS3   GTCRNVariant = "dns3"   // обучена на Microsoft DNS Challenge (default)
	GTCRNSimple GTCRNVariant = "simple" // упрощённая архитектура, обучена на DNS3 тоже
)

const (
	gtcrnNFFT   = 512
	gtcrnHop    = 256
	gtcrnBins   = gtcrnNFFT/2 + 1 // 257
	gtcrnFrame  = 320              // Vox frame = 20 ms @ 16 kHz (не равен hop!)
	gtcrnCacheA = 2 * 1 * 16 * 16 * 33 // conv_cache
	gtcrnCacheB = 2 * 3 * 1 * 1 * 16   // tra_cache
	gtcrnCacheC = 2 * 1 * 33 * 16      // inter_cache

	// Overload protection — такие же пороги что у FE.
	gtcrnProcWindow      = 50
	gtcrnOverloadEnterNs = 17 * 1_000_000
	gtcrnOverloadExitNs  = 12 * 1_000_000
	// Hard-budget: single frame spike выше 30ms → немедленный bypass, не
	// ждём rolling avg. Rolling avg (1 sec) слишком инерционен.
	gtcrnSpikeBypassNs = 30 * 1_000_000
	// Sticky bypass: после входа в bypass держим минимум gtcrnBypassStickFrames
	// tick'ов (≈4 сек) прежде чем позволить probe inference. Без этого rolling
	// avg (который видит 0µs bypass-кадры) быстро падает ниже exit threshold,
	// overloaded=false, next frame опять spike 50-400ms, опять bypass — флап
	// на 20-50 Hz (каждый второй frame). В логах 0067 было отчётливо видно.
	gtcrnBypassStickFrames = 200
)

// sqrtHann — precomputed hann(N)^0.5. Для STFT (analysis) и iSTFT (synthesis)
// используется одно и то же окно: W·W на двух перекрывающихся окнах (hop=N/2)
// суммируется в Hann, сумма Hann на 50% overlap = 1.0 → perfect reconstruction.
var sqrtHann [gtcrnNFFT]float64

func init() {
	for i := 0; i < gtcrnNFFT; i++ {
		// hann(N)[i] = 0.5 * (1 - cos(2π i / (N-1)))
		h := 0.5 * (1.0 - math.Cos(2.0*math.Pi*float64(i)/float64(gtcrnNFFT-1)))
		sqrtHann[i] = math.Sqrt(h)
	}
}

// GTCRN — обёртка над загруженной GTCRN моделью + STFT-pipeline.
// Не потокобезопасна; Process/Close из одного владельца (audioLoop).
type GTCRN struct {
	sess    *onnx.Session
	variant GTCRNVariant
	log     *slog.Logger

	// --- STFT/iSTFT state ---
	// inBuf — кольцо PCM-сэмплов float32 [-1..1]. Размер feFrame + nfft чтобы
	// поместить накопление до 512 с резервом на ещё один 320-входа.
	inBuf     []float32
	inLen     int
	outBuf    []float32 // PCM после overlap-add, отдаём 320 samples в Process.
	outLen    int
	overlap   [gtcrnNFFT]float64 // tail от предыдущего iSTFT для overlap-add
	overlapOk bool                // после первого iSTFT становится true

	// Рабочие буферы FFT переиспользуются чтобы не аллоцировать per frame.
	fftIn  [gtcrnNFFT]complex128
	fftOut []complex128

	// dryBuf — последний отданный наружу dry-сигнал, для wet/dry mix.
	dryBuf [gtcrnFrame]float32
	mix    float32

	// --- Overload protection (аналогично FastEnhancer) ---
	procTimings  [gtcrnProcWindow]int64
	procIdx      int
	procFilled   bool
	overloaded   bool
	bypassStreak int // сколько tick'ов подряд в bypass (probe раз в gtcrnBypassStickFrames)
	OverloadHits int64
}

// NewGTCRN грузит embedded модель указанного варианта. Выполняет warm-up до
// возврата чтобы первый inference в audioLoop не дал max_loop скачка.
func NewGTCRN(variant GTCRNVariant, log *slog.Logger) (*GTCRN, error) {
	var data []byte
	switch variant {
	case GTCRNDNS3:
		data = gtcrnModelDNS3
	case GTCRNSimple:
		data = gtcrnModelSimple
	default:
		return nil, fmt.Errorf("gtcrn: unknown variant %q", variant)
	}

	inputs := []onnx.IOSpec{
		{Name: "mix", Shape: []int64{1, int64(gtcrnBins), 1, 2}},
		{Name: "conv_cache", Shape: []int64{2, 1, 16, 16, 33}},
		{Name: "tra_cache", Shape: []int64{2, 3, 1, 1, 16}},
		{Name: "inter_cache", Shape: []int64{2, 1, 33, 16}},
	}
	outputs := []onnx.IOSpec{
		{Name: "enh", Shape: []int64{1, int64(gtcrnBins), 1, 2}},
		{Name: "conv_cache_out", Shape: []int64{2, 1, 16, 16, 33}},
		{Name: "tra_cache_out", Shape: []int64{2, 3, 1, 1, 16}},
		{Name: "inter_cache_out", Shape: []int64{2, 1, 33, 16}},
	}

	sess, err := onnx.NewSessionFromBytesWithThreads(data, inputs, outputs, 1)
	if err != nil {
		return nil, fmt.Errorf("gtcrn: load %s: %w", variant, err)
	}

	g := &GTCRN{
		sess:    sess,
		variant: variant,
		log:     log,
		inBuf:   make([]float32, gtcrnFrame+gtcrnNFFT),
		outBuf:  make([]float32, gtcrnFrame+gtcrnNFFT),
		mix:     1.0,
	}

	// Warm-up: 4 inference на silence. Раньше было 2 на silence (OK, один 187ms
	// spike на первом real-speech). В 0067 попробовали 8 на shaped noise — стало
	// хуже (50-400ms spike'и каждый frame) — ORT видимо закешировал computation
	// path под распределение noise, которое потом не переиспользуется на real
	// speech. Вернулись на silence + 4 итерации.
	warmStart := time.Now()
	mixIn := sess.Input(0)
	for i := range mixIn {
		mixIn[i] = 0
	}
	for w := 0; w < 4; w++ {
		if err := sess.Run(); err != nil {
			log.Warn("gtcrn warmup failed", slog.String("error", err.Error()))
			break
		}
		// Cache rotation: output_i → input_i для следующего warmup-шага.
		for i := 1; i <= 3; i++ {
			copy(sess.Input(i), sess.Output(i))
		}
	}
	// Сброс cache в нули — реальный inference должен стартовать с чистого состояния.
	for i := 1; i <= 3; i++ {
		buf := sess.Input(i)
		for j := range buf {
			buf[j] = 0
		}
	}

	log.Info("gtcrn loaded",
		slog.String("variant", string(variant)),
		slog.Duration("warmup", time.Since(warmStart)))
	return g, nil
}

// SetMix wet/dry ratio для UI slider (0..1). 1 = full denoise, 0 = bypass (dry).
func (g *GTCRN) SetMix(mix float32) {
	switch {
	case mix < 0:
		mix = 0
	case mix > 1:
		mix = 1
	}
	g.mix = mix
}

// Variant возвращает текущую загруженную модель.
func (g *GTCRN) Variant() GTCRNVariant { return g.variant }

// Overloaded сигнализирует что Process сейчас в bypass из-за CPU нагрузки.
func (g *GTCRN) Overloaded() bool { return g.overloaded }

// Process принимает 640 bytes (320 samples int16 LE) и возвращает 640 bytes.
// На первых 1-2 фреймах возвращает dry — STFT pipeline ещё не заполнен.
func (g *GTCRN) Process(pcmBytes []byte) []byte {
	if len(pcmBytes) != gtcrnFrame*2 {
		out := make([]byte, len(pcmBytes))
		copy(out, pcmBytes)
		return out
	}

	start := time.Now()
	if g.overloaded {
		g.bypassStreak++
		// Probe: раз в gtcrnBypassStickFrames пробуем real inference,
		// чтобы проверить, восстановилась ли производительность. Если нет —
		// recordProcTime'll detect spike и reset bypassStreak → остаёмся
		// в bypass ещё период.
		if g.bypassStreak < gtcrnBypassStickFrames {
			out := make([]byte, gtcrnFrame*2)
			copy(out, pcmBytes)
			g.OverloadHits++
			return out
		}
		// Fall through для probe inference. Сбрасываем streak независимо
		// от результата — следующий probe через период.
		g.bypassStreak = 0
	}
	defer func() {
		g.recordProcTime(time.Since(start).Nanoseconds())
	}()

	// int16 LE → float32 [-1..1], складываем в inBuf и dryBuf.
	for i := 0; i < gtcrnFrame; i++ {
		s := int16(pcmBytes[2*i]) | int16(pcmBytes[2*i+1])<<8
		f := float32(s) / 32768.0
		g.dryBuf[i] = f
		g.inBuf[g.inLen+i] = f
	}
	g.inLen += gtcrnFrame

	// Пока есть >= nfft сэмплов — обработка STFT/iSTFT по hop=256.
	// overlap=256 на nfft=512: после iSTFT первые hop сэмплов = overlap-add с tail,
	// остальные hop сэмплов = новый tail для следующего шага.
	for g.inLen >= gtcrnNFFT {
		g.processOneFrame()
		// Сдвигаем inBuf на hop (не на nfft! 50% overlap).
		copy(g.inBuf, g.inBuf[gtcrnHop:g.inLen])
		g.inLen -= gtcrnHop
	}

	out := make([]byte, gtcrnFrame*2)
	if g.outLen < gtcrnFrame || !g.overlapOk {
		copy(out, pcmBytes)
		return out
	}

	// wet/dry mix → int16 LE
	mix := g.mix
	for i := 0; i < gtcrnFrame; i++ {
		v := mix*g.outBuf[i] + (1-mix)*g.dryBuf[i]
		switch {
		case v > 1.0:
			v = 1.0
		case v < -1.0:
			v = -1.0
		}
		s := int16(v * 32767.0)
		out[2*i] = byte(s)
		out[2*i+1] = byte(s >> 8)
	}
	copy(g.outBuf, g.outBuf[gtcrnFrame:g.outLen])
	g.outLen -= gtcrnFrame
	return out
}

// processOneFrame: STFT(inBuf[:512]) → inference → iSTFT → overlap-add → outBuf.
func (g *GTCRN) processOneFrame() {
	// Analysis: окно * входной буфер → FFT.
	for i := 0; i < gtcrnNFFT; i++ {
		g.fftIn[i] = complex(float64(g.inBuf[i])*sqrtHann[i], 0)
	}
	spec := fft.FFT(g.fftIn[:])

	// Записываем complex spectrum в input tensor "mix" как 257 × (real, imag).
	// Shape [1, 257, 1, 2]: layout last-dim-first, индексы (0, bin, 0, 0|1).
	mixIn := g.sess.Input(0)
	for b := 0; b < gtcrnBins; b++ {
		mixIn[b*2+0] = float32(real(spec[b]))
		mixIn[b*2+1] = float32(imag(spec[b]))
	}

	if err := g.sess.Run(); err != nil {
		g.log.Warn("gtcrn inference failed, bypass фрейм",
			slog.String("error", err.Error()))
		// Fallback: сохраняем как overlap-add от dry чтобы continuity не рвать.
		g.appendHopFromDry()
		return
	}

	// Enh spectrum → iFFT.
	enh := g.sess.Output(0)
	specOut := make([]complex128, gtcrnNFFT)
	for b := 0; b < gtcrnBins; b++ {
		specOut[b] = complex(float64(enh[b*2+0]), float64(enh[b*2+1]))
	}
	// Negative frequencies — conjugate symmetry (real signal).
	for b := 1; b < gtcrnBins-1; b++ {
		specOut[gtcrnNFFT-b] = cmplx.Conj(specOut[b])
	}
	wavFrame := fft.IFFT(specOut)

	// Synthesis window (тот же sqrt-Hann) + overlap-add.
	// overlap — это tail предыдущего iSTFT: хвост последних hop сэмплов.
	// Новые (1-overlap_factor) части hop идут наружу, остальное — overlap add.
	// Конкретно: первые hop сэмплов нового фрейма = windowed[i] + overlap[i]
	// (сумма двух окон, i < hop); затем новый tail = windowed[hop:].
	var cur [gtcrnNFFT]float64
	for i := 0; i < gtcrnNFFT; i++ {
		cur[i] = real(wavFrame[i]) * sqrtHann[i]
	}

	if g.overlapOk {
		// Первые hop сэмплов = overlap-add предыдущего хвоста и начала нового фрейма.
		for i := 0; i < gtcrnHop; i++ {
			sum := g.overlap[i] + cur[i]
			if g.outLen < len(g.outBuf) {
				g.outBuf[g.outLen] = float32(sum)
				g.outLen++
			}
		}
	} else {
		// Первый iSTFT — нет накопленного overlap, отдаём первые hop как есть.
		for i := 0; i < gtcrnHop; i++ {
			if g.outLen < len(g.outBuf) {
				g.outBuf[g.outLen] = float32(cur[i])
				g.outLen++
			}
		}
		g.overlapOk = true
	}

	// Новый tail для следующего overlap-add — последние (nfft-hop) сэмплов.
	// Смещаем индексы: overlap[0..nfft-hop-1] = cur[hop..nfft-1].
	for i := 0; i < gtcrnNFFT-gtcrnHop; i++ {
		g.overlap[i] = cur[gtcrnHop+i]
	}

	// Cache rotation для следующего inference.
	for i := 1; i <= 3; i++ {
		copy(g.sess.Input(i), g.sess.Output(i))
	}
}

// appendHopFromDry — fallback при ошибке inference. Кладём в outBuf hop
// сэмплов dry из inBuf чтобы пользователь не слышал разрыв.
func (g *GTCRN) appendHopFromDry() {
	for i := 0; i < gtcrnHop; i++ {
		if g.outLen < len(g.outBuf) {
			g.outBuf[g.outLen] = g.inBuf[i]
			g.outLen++
		}
	}
}

// recordProcTime ведёт скользящее окно времени Process и управляет overload flag.
// Вызывается только для real inference (не для bypass-кадров), см. Process.
func (g *GTCRN) recordProcTime(ns int64) {
	g.procTimings[g.procIdx] = ns
	g.procIdx++
	if g.procIdx >= len(g.procTimings) {
		g.procIdx = 0
		g.procFilled = true
	}
	// Hard-budget: single spike > gtcrnSpikeBypassNs → немедленный bypass.
	// Probe inference тоже попадает сюда — если >30ms, мы остаёмся в bypass
	// на следующий период (bypassStreak=0 уже сброшен в Process).
	if ns > gtcrnSpikeBypassNs {
		if !g.overloaded {
			g.overloaded = true
			g.log.Warn("gtcrn: spike detected, immediate bypass",
				slog.String("variant", string(g.variant)),
				slog.Float64("spike_ms", float64(ns)/1_000_000))
		}
		// Сброс cache при переходе в bypass — иначе probe inference
		// работает с stale состоянием.
		for i := 1; i <= 3; i++ {
			buf := g.sess.Input(i)
			for j := range buf {
				buf[j] = 0
			}
		}
		g.overlapOk = false
		return
	}
	if !g.procFilled {
		return
	}
	var sum int64
	for _, t := range g.procTimings {
		sum += t
	}
	avg := sum / int64(len(g.procTimings))
	if !g.overloaded && avg > gtcrnOverloadEnterNs {
		g.overloaded = true
		g.log.Warn("gtcrn: CPU overload, переход в bypass",
			slog.String("variant", string(g.variant)),
			slog.Float64("avg_ms", float64(avg)/1_000_000))
	} else if g.overloaded && avg < gtcrnOverloadExitNs {
		g.overloaded = false
		// Сброс cache при выходе чтобы не было артефактов от stale state.
		for i := 1; i <= 3; i++ {
			buf := g.sess.Input(i)
			for j := range buf {
				buf[j] = 0
			}
		}
		g.overlapOk = false // пересоберём overlap state
		g.log.Info("gtcrn: CPU свободен, выход из bypass",
			slog.String("variant", string(g.variant)),
			slog.Float64("avg_ms", float64(avg)/1_000_000))
	}
}

// Close освобождает ONNX сессию.
func (g *GTCRN) Close() {
	if g.sess != nil {
		_ = g.sess.Close()
		g.sess = nil
	}
}
