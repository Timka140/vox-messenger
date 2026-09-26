//go:build nx_onnx

package vad

import (
	"fmt"
	"log/slog"
	"vox_app/internal/ml/onnx"

	ort "github.com/yalue/onnxruntime_go"
)

// sileroDetector — обёртка ort.DynamicAdvancedSession для Silero v5 VAD.
//
// ВАЖНО: тензоры ВХОДА создаются/уничтожаются на каждый Infer. pre-allocated
// tInput/tState один раз при Init с записью в `GetData()` backing slice НЕ
// работает — ORT не перечитывает данные между Run'ами (видит только
// первоначальные значения). См. тесты yalue/onnxruntime_go — там новые
// тензоры каждый Run.
//
// Смешанные типы (input/state float32 + sr int64) не поддерживаются нашим
// internal/ml/onnx.Session (он float32-only), поэтому работаем с ort напрямую.
//
// Silero v5 требует ContextSize-префикса (64 sample) перед current chunk —
// см. feedback_silero_v5_context в памяти.
//
// Не потокобезопасен: Infer/Reset/Close вызываются только из одного owner
// (VoiceService.audioLoop).

type sileroDetector struct {
	sess *ort.DynamicAdvancedSession

	// Persistent LSTM state — копируем в новый state-tensor каждый Infer,
	// ORT возвращает новое состояние → копируем сюда для следующего Run.
	stateBuf []float32 // 256 floats = [2, 1, 128]

	// context — последние ContextSize (64) семплов предыдущего chunk'а.
	// Silero v5 конкатенирует [context, current_chunk] → вход модели 576 sample.
	// При первом вызове (и после Reset) инициализируется нулями.
	context []float32 // 64

	// sr — scalar не меняется, создаётся один раз (Silero жёстко требует
	// rank-0 shape []).
	tSr *ort.Scalar[int64]

	// inputData — переиспользуемый буфер float32 (576 samples = context+chunk)
	// куда собираем концат перед созданием tensor.
	inputData []float32

	log *slog.Logger
}

func newSilero(modelPath string, log *slog.Logger) (Detector, error) {
	if !onnx.Available() {
		if err := onnx.InitError(); err != nil {
			return nil, fmt.Errorf("vad: onnx runtime not available: %w", err)
		}
		return nil, onnx.ErrRuntimeNotFound
	}

	d := &sileroDetector{
		log:       log,
		stateBuf:  make([]float32, 2*1*128),
		context:   make([]float32, ContextSize),
		inputData: make([]float32, InputSize),
	}

	var err error
	if d.tSr, err = ort.NewScalar[int64](SampleRate); err != nil {
		return nil, fmt.Errorf("vad: alloc sr: %w", err)
	}

	inNames := []string{"input", "state", "sr"}
	outNames := []string{"output", "stateN"}

	d.sess, err = ort.NewDynamicAdvancedSession(modelPath, inNames, outNames, nil)
	if err != nil {
		_ = d.tSr.Destroy()
		return nil, fmt.Errorf("vad: load %s: %w", modelPath, err)
	}

	// Warm-up: 2 инференса с нулями чтобы libort/JIT прогрелись.
	zero := make([]int16, WindowSize)
	for i := 0; i < 2; i++ {
		if _, werr := d.Infer(zero); werr != nil {
			log.Warn("vad warm-up inference failed", slog.Int("iter", i), slog.String("err", werr.Error()))
		}
	}
	d.Reset()

	log.Info("silero VAD loaded", slog.String("model", modelPath))
	return d, nil
}

// Infer: каждый вызов создаёт свежие tInput + tState tensors, гоняет Run,
// читает outputs, Destroy'ит все временные тензоры. Overhead аллокаций
// ~10-20 µs на пару тензоров — незначительно на 30 fps (VAD частота).
func (d *sileroDetector) Infer(samples512 []int16) (float32, error) {
	if len(samples512) != WindowSize {
		return 0, fmt.Errorf("vad: expected %d samples, got %d", WindowSize, len(samples512))
	}

	// Собираем вход модели: [context_64 | chunk_512] = 576 float32.
	// Без context-префикса Silero v5 не работает (выдаёт constant prob ~0.0005).
	copy(d.inputData[:ContextSize], d.context)

	for i := 0; i < WindowSize; i++ {
		d.inputData[ContextSize+i] = float32(samples512[i]) / 32768.0
	}

	// Сохраняем последние ContextSize семплов нового chunk'а как context
	// для следующего вызова.
	copy(d.context, d.inputData[InputSize-ContextSize:])

	// Свежий input tensor с актуальными данными (shape [1, 576]).
	tInput, err := ort.NewTensor[float32](ort.NewShape(1, InputSize), d.inputData)
	if err != nil {
		return 0, fmt.Errorf("vad: new input tensor: %w", err)
	}
	defer tInput.Destroy()

	// Свежий state tensor с persistent LSTM-состоянием.
	stateCopy := make([]float32, len(d.stateBuf))
	copy(stateCopy, d.stateBuf)
	tState, err := ort.NewTensor[float32](ort.NewShape(2, 1, 128), stateCopy)
	if err != nil {
		return 0, fmt.Errorf("vad: new state tensor: %w", err)
	}
	defer tState.Destroy()

	inputs := []ort.Value{tInput, tState, d.tSr}
	outputs := []ort.Value{nil, nil}

	if err := d.sess.Run(inputs, outputs); err != nil {
		return 0, fmt.Errorf("vad: run: %w", err)
	}

	probTensor, ok := outputs[0].(*ort.Tensor[float32])
	if !ok {
		if outputs[0] != nil {
			_ = outputs[0].Destroy()
		}
		if outputs[1] != nil {
			_ = outputs[1].Destroy()
		}
		return 0, fmt.Errorf("vad: output[0] not *Tensor[float32] (got %T)", outputs[0])
	}
	stateNTensor, ok := outputs[1].(*ort.Tensor[float32])
	if !ok {
		_ = outputs[0].Destroy()
		if outputs[1] != nil {
			_ = outputs[1].Destroy()
		}
		return 0, fmt.Errorf("vad: output[1] not *Tensor[float32] (got %T)", outputs[1])
	}

	probData := probTensor.GetData()
	prob := float32(0)
	if len(probData) > 0 {
		prob = probData[0]
	}

	stateNData := stateNTensor.GetData()
	if len(stateNData) == len(d.stateBuf) {
		copy(d.stateBuf, stateNData)
	}

	_ = probTensor.Destroy()
	_ = stateNTensor.Destroy()

	return prob, nil
}

// Reset обнуляет LSTM state и context (для Silero важно и то и другое).
func (d *sileroDetector) Reset() {
	for i := range d.stateBuf {
		d.stateBuf[i] = 0
	}
	for i := range d.context {
		d.context[i] = 0
	}
}

// Close освобождает ONNX-сессию и persistent тензоры. Idempotent.
func (d *sileroDetector) Close() {
	if d.sess != nil {
		_ = d.sess.Destroy()
		d.sess = nil
	}
	if d.tSr != nil {
		_ = d.tSr.Destroy()
		d.tSr = nil
	}
}
