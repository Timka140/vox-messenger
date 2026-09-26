//go:build nx_onnx

package onnx

import (
	"fmt"

	ort "github.com/yalue/onnxruntime_go"
)

// IOSpec описывает вход/выход модели: имя тензора и его shape (int64, как в ONNX).
// Динамические оси не поддерживаются этой обёрткой — нужны конкретные размерности,
// чтобы можно было аллоцировать тензоры один раз и переиспользовать их каждый кадр
// (критично для realtime-инференса в аудио-пайплайне).
type IOSpec struct {
	Name  string
	Shape []int64
}

// Session — тонкая обёртка над ort.AdvancedSession с предаллокированными
// float32 тензорами. Все входные/выходные буферы живут ровно столько, сколько
// Session; вызывающий пишет в Input(i), вызывает Run(), читает Output(i).
//
// Потокобезопасность: Run() не потокобезопасен, предполагается один владелец
// (обычно audioLoop).
type Session struct {
	ortSession *ort.AdvancedSession
	inTensors  []*ort.Tensor[float32]
	outTensors []*ort.Tensor[float32]
	inputs     [][]float32
	outputs    [][]float32
}

// NewSession загружает модель с диска и создаёт сессию с заранее
// выделенными тензорами. Требует успешной инициализации runtime (Available() == true).
func NewSession(modelPath string, inputs, outputs []IOSpec) (*Session, error) {
	return newSession(modelPath, nil, inputs, outputs, 0)
}

// NewSessionFromBytes грузит модель напрямую из []byte (удобно с //go:embed).
// В остальном идентично NewSession.
func NewSessionFromBytes(onnxData []byte, inputs, outputs []IOSpec) (*Session, error) {
	return newSession("", onnxData, inputs, outputs, 0)
}

// NewSessionFromBytesWithThreads — как NewSessionFromBytes, но ограничивает
// количество intra-op/inter-op потоков ORT. По умолчанию ORT запускает пул
// равный числу ядер CPU, что для лёгких realtime-моделей (FastEnhancer, VAD)
// даёт 3-5× overhead без ускорения — нужен 1 поток. threads=0 использует
// дефолт ORT (не рекомендуется для наших моделей).
func NewSessionFromBytesWithThreads(onnxData []byte, inputs, outputs []IOSpec, threads int) (*Session, error) {
	return newSession("", onnxData, inputs, outputs, threads)
}

func newSession(modelPath string, onnxData []byte, inputs, outputs []IOSpec, threads int) (*Session, error) {
	if !runtimeAvailable {
		if initErr != nil {
			return nil, initErr
		}
		return nil, ErrRuntimeNotFound
	}

	s := &Session{
		inTensors:  make([]*ort.Tensor[float32], len(inputs)),
		outTensors: make([]*ort.Tensor[float32], len(outputs)),
		inputs:     make([][]float32, len(inputs)),
		outputs:    make([][]float32, len(outputs)),
	}

	// Cleanup в случае ошибки — частично созданные тензоры освобождаем.
	cleanup := func() {
		for _, t := range s.inTensors {
			if t != nil {
				t.Destroy()
			}
		}
		for _, t := range s.outTensors {
			if t != nil {
				t.Destroy()
			}
		}
	}

	inNames := make([]string, len(inputs))
	outNames := make([]string, len(outputs))
	inVals := make([]ort.Value, len(inputs))
	outVals := make([]ort.Value, len(outputs))

	for i, spec := range inputs {
		t, err := ort.NewEmptyTensor[float32](ort.NewShape(spec.Shape...))
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("onnx: alloc input %q: %w", spec.Name, err)
		}
		s.inTensors[i] = t
		s.inputs[i] = t.GetData()
		inNames[i] = spec.Name
		inVals[i] = t
	}
	for i, spec := range outputs {
		t, err := ort.NewEmptyTensor[float32](ort.NewShape(spec.Shape...))
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("onnx: alloc output %q: %w", spec.Name, err)
		}
		s.outTensors[i] = t
		s.outputs[i] = t.GetData()
		outNames[i] = spec.Name
		outVals[i] = t
	}

	// Опциональное ограничение threads. Уничтожаем SessionOptions сразу после
	// создания сессии — они копируются внутрь ORT и дальше не нужны.
	var opts *ort.SessionOptions
	if threads > 0 {
		o, err := ort.NewSessionOptions()
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("onnx: create session options: %w", err)
		}
		defer o.Destroy()
		if err := o.SetIntraOpNumThreads(threads); err != nil {
			cleanup()
			return nil, fmt.Errorf("onnx: set intra-op threads: %w", err)
		}
		if err := o.SetInterOpNumThreads(threads); err != nil {
			cleanup()
			return nil, fmt.Errorf("onnx: set inter-op threads: %w", err)
		}
		opts = o
	}

	var sess *ort.AdvancedSession
	var err error
	if onnxData != nil {
		sess, err = ort.NewAdvancedSessionWithONNXData(onnxData, inNames, outNames, inVals, outVals, opts)
	} else {
		sess, err = ort.NewAdvancedSession(modelPath, inNames, outNames, inVals, outVals, opts)
	}
	if err != nil {
		cleanup()
		src := modelPath
		if onnxData != nil {
			src = fmt.Sprintf("[embedded %d bytes]", len(onnxData))
		}
		return nil, fmt.Errorf("onnx: load %s: %w", src, err)
	}
	s.ortSession = sess
	return s, nil
}

// Input возвращает буфер i-го входа. Вызывающий пишет в него перед Run().
// Срез валиден до вызова Close().
func (s *Session) Input(i int) []float32 { return s.inputs[i] }

// Output возвращает буфер i-го выхода. После Run() в нём лежит результат.
// Срез валиден до вызова Close().
func (s *Session) Output(i int) []float32 { return s.outputs[i] }

// Run запускает инференс с текущим содержимым входных буферов.
func (s *Session) Run() error {
	return s.ortSession.Run()
}

// Close освобождает сессию и все тензоры.
func (s *Session) Close() error {
	if s.ortSession != nil {
		_ = s.ortSession.Destroy()
		s.ortSession = nil
	}
	for i, t := range s.inTensors {
		if t != nil {
			t.Destroy()
			s.inTensors[i] = nil
		}
	}
	for i, t := range s.outTensors {
		if t != nil {
			t.Destroy()
			s.outTensors[i] = nil
		}
	}
	return nil
}
