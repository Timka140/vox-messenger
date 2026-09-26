//go:build nx_onnx

// FastEnhancer — streaming neural NS через ONNX runtime.
//
// Работает на Go-стороне ПОСЛЕ capture.StreamRead в audioLoop, в отличие от
// DFN3/RNNoise которые встроены в C-callback. Это упрощает интеграцию (не
// тащим onnxruntime в C++) ценой одного дополнительного memcpy + лёгкой
// латентности ring buffer'а (≤ 20 ms, см. ниже).
//
// Модели качаются один раз скриптом app/internal/audio/fastenhancer/download_models.sh
// и коммитятся в репо. Их 5 (Tiny/Base/Small/Medium/Large), отличаются
// hop_size (256/256/256/160/100) и количеством cache буферов (4..7).
// При смене модели — Close + New (старая ONNX session уничтожается).

package audio

import (
	_ "embed"
	"fmt"
	"log/slog"
	"time"
	"vox_app/internal/ml/onnx"
)

// Модели embedded в бинарь (nx_onnx tag). Суммарно ~7.9 MB:
//   t 125KB + b 413KB + s 833KB + m 2.0MB + l 4.5MB ≈ 7.9MB
// Это proще чем возиться с путями к файлам на диске в production
// и даёт atomic update через замену .exe. Плата — небольшой рост
// размера бинаря (плюс ~8MB на 60MB exe), доступно только в nx_onnx сборке.

//go:embed fastenhancer/fastenhancer_t.onnx
var feModelTiny []byte

//go:embed fastenhancer/fastenhancer_b.onnx
var feModelBase []byte

//go:embed fastenhancer/fastenhancer_s.onnx
var feModelSmall []byte

//go:embed fastenhancer/fastenhancer_m.onnx
var feModelMedium []byte

//go:embed fastenhancer/fastenhancer_l.onnx
var feModelLarge []byte

func feModelBytes(size FastEnhancerSize) ([]byte, error) {
	switch size {
	case FETiny:
		return feModelTiny, nil
	case FEBase:
		return feModelBase, nil
	case FESmall:
		return feModelSmall, nil
	case FEMedium:
		return feModelMedium, nil
	case FELarge:
		return feModelLarge, nil
	default:
		return nil, fmt.Errorf("fastenhancer: неизвестный размер %q", size)
	}
}

// FastEnhancerAvailable — true если бинарь собран с -tags nx_onnx И нативный
// onnxruntime подгрузился. Вызывать из voice/audio service чтобы показывать
// или скрывать опцию в UI.
func FastEnhancerAvailable() bool { return onnx.Available() }

// FastEnhancerSize — однобуквенный код размера модели как в имени файла.
type FastEnhancerSize string

const (
	FETiny   FastEnhancerSize = "t"
	FEBase   FastEnhancerSize = "b"
	FESmall  FastEnhancerSize = "s"
	FEMedium FastEnhancerSize = "m"
	FELarge  FastEnhancerSize = "l"
)

// fastEnhancerSpec — статическая таблица I/O для каждого размера, узнана
// заранее через cmd/vad-inspect (см. fastenhancer/README.md). Если репо
// FastEnhancer выпустит модель с другим shape, тут потребуется обновить запись.
type fastEnhancerSpec struct {
	hopSize  int
	cacheIn  []onnx.IOSpec
	cacheOut []onnx.IOSpec
}

func feSpec(size FastEnhancerSize) (fastEnhancerSpec, error) {
	switch size {
	case FETiny:
		return fastEnhancerSpec{
			hopSize: 256,
			cacheIn: []onnx.IOSpec{
				{Name: "cache_in_0", Shape: []int64{1, 256}},
				{Name: "cache_in_1", Shape: []int64{1, 256}},
				{Name: "cache_in_2", Shape: []int64{1, 16, 20}},
				{Name: "cache_in_3", Shape: []int64{1, 16, 20}},
			},
			cacheOut: []onnx.IOSpec{
				{Name: "cache_out_0", Shape: []int64{1, 256}},
				{Name: "cache_out_1", Shape: []int64{1, 256}},
				{Name: "cache_out_2", Shape: []int64{1, 16, 20}},
				{Name: "cache_out_3", Shape: []int64{1, 16, 20}},
			},
		}, nil
	case FEBase:
		return fastEnhancerSpec{
			hopSize: 256,
			cacheIn: []onnx.IOSpec{
				{Name: "cache_in_0", Shape: []int64{1, 256}},
				{Name: "cache_in_1", Shape: []int64{1, 256}},
				{Name: "cache_in_2", Shape: []int64{1, 24, 36}},
				{Name: "cache_in_3", Shape: []int64{1, 24, 36}},
				{Name: "cache_in_4", Shape: []int64{1, 24, 36}},
			},
			cacheOut: []onnx.IOSpec{
				{Name: "cache_out_0", Shape: []int64{1, 256}},
				{Name: "cache_out_1", Shape: []int64{1, 256}},
				{Name: "cache_out_2", Shape: []int64{1, 24, 36}},
				{Name: "cache_out_3", Shape: []int64{1, 24, 36}},
				{Name: "cache_out_4", Shape: []int64{1, 24, 36}},
			},
		}, nil
	case FESmall:
		return fastEnhancerSpec{
			hopSize: 256,
			cacheIn: []onnx.IOSpec{
				{Name: "cache_in_0", Shape: []int64{1, 256}},
				{Name: "cache_in_1", Shape: []int64{1, 256}},
				{Name: "cache_in_2", Shape: []int64{1, 36, 48}},
				{Name: "cache_in_3", Shape: []int64{1, 36, 48}},
				{Name: "cache_in_4", Shape: []int64{1, 36, 48}},
			},
			cacheOut: []onnx.IOSpec{
				{Name: "cache_out_0", Shape: []int64{1, 256}},
				{Name: "cache_out_1", Shape: []int64{1, 256}},
				{Name: "cache_out_2", Shape: []int64{1, 36, 48}},
				{Name: "cache_out_3", Shape: []int64{1, 36, 48}},
				{Name: "cache_out_4", Shape: []int64{1, 36, 48}},
			},
		}, nil
	case FEMedium:
		return fastEnhancerSpec{
			hopSize: 160,
			cacheIn: []onnx.IOSpec{
				{Name: "cache_in_0", Shape: []int64{1, 352}},
				{Name: "cache_in_1", Shape: []int64{1, 352}},
				{Name: "cache_in_2", Shape: []int64{1, 48, 72}},
				{Name: "cache_in_3", Shape: []int64{1, 48, 72}},
				{Name: "cache_in_4", Shape: []int64{1, 48, 72}},
				{Name: "cache_in_5", Shape: []int64{1, 48, 72}},
			},
			cacheOut: []onnx.IOSpec{
				{Name: "cache_out_0", Shape: []int64{1, 352}},
				{Name: "cache_out_1", Shape: []int64{1, 352}},
				{Name: "cache_out_2", Shape: []int64{1, 48, 72}},
				{Name: "cache_out_3", Shape: []int64{1, 48, 72}},
				{Name: "cache_out_4", Shape: []int64{1, 48, 72}},
				{Name: "cache_out_5", Shape: []int64{1, 48, 72}},
			},
		}, nil
	case FELarge:
		return fastEnhancerSpec{
			hopSize: 100,
			cacheIn: []onnx.IOSpec{
				{Name: "cache_in_0", Shape: []int64{1, 412}},
				{Name: "cache_in_1", Shape: []int64{1, 412}},
				{Name: "cache_in_2", Shape: []int64{1, 64, 96}},
				{Name: "cache_in_3", Shape: []int64{1, 64, 96}},
				{Name: "cache_in_4", Shape: []int64{1, 64, 96}},
				{Name: "cache_in_5", Shape: []int64{1, 64, 96}},
				{Name: "cache_in_6", Shape: []int64{1, 64, 96}},
			},
			cacheOut: []onnx.IOSpec{
				{Name: "cache_out_0", Shape: []int64{1, 412}},
				{Name: "cache_out_1", Shape: []int64{1, 412}},
				{Name: "cache_out_2", Shape: []int64{1, 64, 96}},
				{Name: "cache_out_3", Shape: []int64{1, 64, 96}},
				{Name: "cache_out_4", Shape: []int64{1, 64, 96}},
				{Name: "cache_out_5", Shape: []int64{1, 64, 96}},
				{Name: "cache_out_6", Shape: []int64{1, 64, 96}},
			},
		}, nil
	default:
		return fastEnhancerSpec{}, fmt.Errorf("fastenhancer: неизвестный размер %q (ожидается t/b/s/m/l)", size)
	}
}

// FastEnhancer — обёртка над одной загруженной моделью FastEnhancer.
// Не потокобезопасна — Process/Close из одного владельца (audioLoop).
type FastEnhancer struct {
	sess    *onnx.Session
	spec    fastEnhancerSpec
	size    FastEnhancerSize
	log     *slog.Logger

	// Кольцевые буферы для адаптации к Vox-фрейму 320 samples.
	// inBuf копит float32 PCM из Process(); как только inLen >= hopSize,
	// извлекаем chunk → inference → результат в outBuf. Из outBuf отдаём
	// 320 floats наружу (после mix с dry).
	inBuf  []float32
	inLen  int
	outBuf []float32
	outLen int

	// dryBuf — последний отданный наружу dry-сигнал, для wet/dry mix.
	dryBuf [320]float32

	// mix — strength [0..1]. 1.0 = чистый wet (full denoise), 0.0 = dry (bypass).
	mix float32

	// --- Overload protection ---
	// Скользящее окно длительностей последних N Process'ов. Если средняя
	// > overloadEnterNs (85% бюджета) — переходим в bypass до тех пор пока
	// avg не упадёт < overloadExitNs. В bypass Process возвращает dry мгновенно
	// (без inference), что предотвращает накопление capture буфера и рост
	// задержки голоса под CPU-нагрузкой.
	procTimings  [feProcWindow]int64 // ns
	procIdx      int
	procFilled   bool
	overloaded   bool
	bypassStreak int   // сколько tick'ов подряд в bypass — см. Process/probe
	OverloadHits int64 // счётчик фреймов в bypass (для diagnostics/UI)
}

const (
	feFrameSize        = 320              // 20 ms @ 16 kHz, фиксировано в Vox
	feProcWindow       = 50               // ~1 сек скользящего окна
	feOverloadEnterNs  = 17 * 1_000_000   // 17 ms = 85% бюджета — войти в bypass
	feOverloadExitNs   = 12 * 1_000_000   // 12 ms = 60% — выйти (гистерезис чтобы не флапать)
	feSpikeBypassNs    = 30 * 1_000_000   // single-frame hard budget — immediate bypass
	feBypassStickFrames = 200             // sticky bypass ≈4 сек, probe в конце периода
)

// NewFastEnhancer загружает embedded модель указанного размера.
// Возвращает ошибку если ONNX runtime не доступен или модель не загружается.
// При смене модели вызывающий должен Close старую и сделать New().
func NewFastEnhancer(size FastEnhancerSize, log *slog.Logger) (*FastEnhancer, error) {
	spec, err := feSpec(size)
	if err != nil {
		return nil, err
	}

	data, err := feModelBytes(size)
	if err != nil {
		return nil, err
	}

	// Inputs = [wav_in, cache_in_0..N-1]. Outputs = [wav_out, cache_out_0..N-1].
	inputs := make([]onnx.IOSpec, 0, 1+len(spec.cacheIn))
	inputs = append(inputs, onnx.IOSpec{Name: "wav_in", Shape: []int64{1, int64(spec.hopSize)}})
	inputs = append(inputs, spec.cacheIn...)

	outputs := make([]onnx.IOSpec, 0, 1+len(spec.cacheOut))
	outputs = append(outputs, onnx.IOSpec{Name: "wav_out", Shape: []int64{1, int64(spec.hopSize)}})
	outputs = append(outputs, spec.cacheOut...)

	// 1 поток — ORT по умолчанию берёт N_CPU потоков, что для 12G MACs модели
	// на realtime-аудио даёт 3-5× overhead (синхронизация > счёт). Замер:
	// Large на 16-ядерном CPU — 50% загрузки при default vs 8-10% при 1 потоке.
	// Никакой потери скорости: inference сам по себе последовательный и
	// укладывается в бюджет с запасом.
	sess, err := onnx.NewSessionFromBytesWithThreads(data, inputs, outputs, 1)
	if err != nil {
		return nil, fmt.Errorf("fastenhancer: load size %s: %w", size, err)
	}

	// Cache buffers инициализированы нулями автоматически (NewEmptyTensor).
	// inBuf размером с 2 фрейма с запасом на hop=256 после 320: 320+256-160 max wait.
	fe := &FastEnhancer{
		sess:   sess,
		spec:   spec,
		size:   size,
		log:    log,
		inBuf:  make([]float32, feFrameSize+spec.hopSize),
		outBuf: make([]float32, feFrameSize+spec.hopSize),
		mix:    1.0,
	}

	// Warm-up: 4 итерации на silence. Симметрично GTCRN — см. комментарий там
	// про регресс 0067 с shaped noise.
	warmStart := time.Now()
	wavIn := sess.Input(0)
	for i := range wavIn {
		wavIn[i] = 0
	}
	for w := 0; w < 4; w++ {
		if err := sess.Run(); err != nil {
			log.Warn("fastenhancer warmup failed (продолжаем без него)",
				slog.String("size", string(size)),
				slog.String("error", err.Error()))
			break
		}
		// Cache rotation: эмулируем то что делает Process, чтобы вторая итерация
		// прошла через те же hot-paths.
		for i := 0; i < len(spec.cacheIn); i++ {
			copy(sess.Input(1+i), sess.Output(1+i))
		}
	}
	// Сбрасываем cache в нули — warm-up оставил мусор от silence inference,
	// а реальная сессия должна стартовать с чистого состояния.
	for i := 0; i < len(spec.cacheIn); i++ {
		buf := sess.Input(1 + i)
		for j := range buf {
			buf[j] = 0
		}
	}

	log.Info("fastenhancer loaded",
		slog.String("size", string(size)),
		slog.Int("hop", spec.hopSize),
		slog.Int("cache_buffers", len(spec.cacheIn)),
		slog.Duration("warmup", time.Since(warmStart)))
	return fe, nil
}

// SetMix настраивает wet/dry strength: 1.0 = full denoise, 0.0 = bypass (dry).
// Аналог DFN AttenLim, но в линейной шкале а не dB.
func (fe *FastEnhancer) SetMix(mix float32) {
	switch {
	case mix < 0:
		mix = 0
	case mix > 1:
		mix = 1
	}
	fe.mix = mix
}

// Size возвращает текущий загруженный размер модели.
func (fe *FastEnhancer) Size() FastEnhancerSize { return fe.size }

// Process принимает 640 bytes (320 samples int16 LE) и возвращает 640 bytes
// обработанных. На первых 1-2 фреймах outBuf ещё недостаточно заполнен — в
// этом случае возвращаем dry (latency ≤ 20-40 ms на старте сессии).
//
// Под CPU-нагрузкой: если среднее время Process в скользящем окне (~1 сек)
// превышает 17ms — входим в bypass-режим, возвращаем dry мгновенно без
// inference. Это не даёт накопиться capture буферу и предотвращает рост
// задержки голоса. Выход из bypass — когда avg упадёт ниже 12ms (гистерезис).
//
// Возвращает копию (новый slice) чтобы вызывающий мог переиспользовать вход.
func (fe *FastEnhancer) Process(pcmBytes []byte) []byte {
	if len(pcmBytes) != feFrameSize*2 {
		// Не наш формат — возвращаем как есть, не трогаем.
		out := make([]byte, len(pcmBytes))
		copy(out, pcmBytes)
		return out
	}

	// Bypass при CPU overload — sticky на ≈4 сек, потом probe inference.
	// recordProcTime вызываем ТОЛЬКО для real inference (не для bypass), чтобы
	// rolling avg не загрязнялся near-0µs замерами bypass-пути — иначе avg
	// быстро падает ниже exit threshold, выходим из bypass, spike снова, флап.
	start := time.Now()
	if fe.overloaded {
		fe.bypassStreak++
		if fe.bypassStreak < feBypassStickFrames {
			out := make([]byte, feFrameSize*2)
			copy(out, pcmBytes)
			fe.OverloadHits++
			return out
		}
		// Probe inference — fall through к real Run. Streak сбрасываем
		// независимо от результата: следующий probe через период.
		fe.bypassStreak = 0
	}
	defer func() {
		fe.recordProcTime(time.Since(start).Nanoseconds())
	}()

	// int16 LE → float32 [-1..1]
	for i := 0; i < feFrameSize; i++ {
		s := int16(pcmBytes[2*i]) | int16(pcmBytes[2*i+1])<<8
		f := float32(s) / 32768.0
		fe.dryBuf[i] = f
		fe.inBuf[fe.inLen+i] = f
	}
	fe.inLen += feFrameSize

	// Прокручиваем inference столько раз, сколько накопилось hop'ов.
	hop := fe.spec.hopSize
	for fe.inLen >= hop {
		wavIn := fe.sess.Input(0) // [1, hop]
		copy(wavIn, fe.inBuf[:hop])
		// Сдвигаем inBuf
		copy(fe.inBuf, fe.inBuf[hop:fe.inLen])
		fe.inLen -= hop

		if err := fe.sess.Run(); err != nil {
			// Inference failed — продолжаем с dry, чтобы не глушить голос.
			// Логируем редко (каждый 100-й фрейм был бы спамом — здесь рассчитываем
			// что Run падает редко, поэтому каждый раз).
			fe.log.Warn("fastenhancer inference failed", slog.String("error", err.Error()))
			// Кладём dry в outBuf вместо обработанного.
			if fe.outLen+hop <= len(fe.outBuf) {
				copy(fe.outBuf[fe.outLen:], wavIn)
				fe.outLen += hop
			}
			continue
		}

		wavOut := fe.sess.Output(0) // [1, hop]
		if fe.outLen+hop <= len(fe.outBuf) {
			copy(fe.outBuf[fe.outLen:], wavOut)
			fe.outLen += hop
		}

		// Cache rotation: cache_out_X → cache_in_X для следующего шага.
		// Index 0 — wav, поэтому cache начинается с 1.
		for i := 0; i < len(fe.spec.cacheIn); i++ {
			copy(fe.sess.Input(1+i), fe.sess.Output(1+i))
		}
	}

	// Если outBuf ещё не накопил полный фрейм — возвращаем dry (warm-up).
	out := make([]byte, feFrameSize*2)
	if fe.outLen < feFrameSize {
		copy(out, pcmBytes)
		return out
	}

	// wet/dry mix → int16 LE.
	mix := fe.mix
	for i := 0; i < feFrameSize; i++ {
		v := mix*fe.outBuf[i] + (1-mix)*fe.dryBuf[i]
		// клиппинг
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
	// Сдвигаем outBuf на feFrameSize
	copy(fe.outBuf, fe.outBuf[feFrameSize:fe.outLen])
	fe.outLen -= feFrameSize
	return out
}

// Close освобождает ONNX-сессию. После этого Process использовать нельзя.
func (fe *FastEnhancer) Close() {
	if fe.sess != nil {
		_ = fe.sess.Close()
		fe.sess = nil
	}
}

// recordProcTime записывает новое измерение и обновляет overload-состояние.
// Вызывается ТОЛЬКО для real inference (не для bypass-кадров) — иначе near-0µs
// bypass-замеры быстро топят avg ниже exit threshold и создают флап.
func (fe *FastEnhancer) recordProcTime(ns int64) {
	fe.procTimings[fe.procIdx] = ns
	fe.procIdx++
	if fe.procIdx >= len(fe.procTimings) {
		fe.procIdx = 0
		fe.procFilled = true
	}
	// Hard-budget spike: single frame > feSpikeBypassNs → немедленный bypass.
	// Probe inference тоже проходит сюда — если >30ms, остаёмся в bypass.
	if ns > feSpikeBypassNs {
		if !fe.overloaded {
			fe.overloaded = true
			fe.log.Warn("fastenhancer: spike detected, immediate bypass",
				slog.String("size", string(fe.size)),
				slog.Float64("spike_ms", float64(ns)/1_000_000))
		}
		// Сброс cache — stale после spike'а.
		for i := 0; i < len(fe.spec.cacheIn); i++ {
			buf := fe.sess.Input(1 + i)
			for j := range buf {
				buf[j] = 0
			}
		}
		return
	}
	// Пока окно не заполнено — не принимаем решения об overload (малая выборка).
	if !fe.procFilled {
		return
	}
	var sum int64
	for _, t := range fe.procTimings {
		sum += t
	}
	avg := sum / int64(len(fe.procTimings))
	if !fe.overloaded && avg > feOverloadEnterNs {
		fe.overloaded = true
		fe.log.Warn("fastenhancer: CPU overload, переход в bypass-режим",
			slog.String("size", string(fe.size)),
			slog.Float64("avg_ms", float64(avg)/1_000_000))
	} else if fe.overloaded && avg < feOverloadExitNs {
		fe.overloaded = false
		// При возврате сбрасываем cache чтобы не было артефактов от stale state
		// (часть spectrogram у модели завязана на историю которая прервалась).
		for i := 0; i < len(fe.spec.cacheIn); i++ {
			buf := fe.sess.Input(1 + i)
			for j := range buf {
				buf[j] = 0
			}
		}
		fe.log.Info("fastenhancer: CPU свободен, выход из bypass",
			slog.String("size", string(fe.size)),
			slog.Float64("avg_ms", float64(avg)/1_000_000))
	}
}

// Overloaded возвращает true если Process сейчас в bypass из-за CPU нагрузки.
// Для UI/diagnostics. Owner Process'а может увидеть отставание — но менять
// модель или отключать FE должен сам владелец (VoiceService), не FE.
func (fe *FastEnhancer) Overloaded() bool {
	return fe.overloaded
}
