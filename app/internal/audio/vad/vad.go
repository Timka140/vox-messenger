// Package vad — Voice Activity Detection для голосового pipeline.
//
// Используется в voice_loop.go как gate ДО отправки opus-фреймов в сеть:
// если VAD говорит "не речь" — фрейм откладывается в pre-roll и не летит
// в SFU/mesh. Экономия трафика ~80% на паузах без ухудшения качества
// (в отличие от Opus DTX, который портит аудио на приёме).
//
// Архитектурное разделение:
//
//   - vad.go         — публичное API (этот файл, без build-тега)
//   - silero.go      — реальная реализация Silero v5 (//go:build nx_onnx)
//   - silero_stub.go — no-op реализация (//go:build !nx_onnx)
//
// Build-tag nx_onnx включается если бинарь собирается с ONNX-рантаймом
// (тот же тег, что у DFN3). Без тега NewSilero возвращает (nil, ErrNotBuilt).
package vad

import (
	"errors"
	"log/slog"
)

// ErrNotBuilt — build без тега nx_onnx, VAD недоступен.
var ErrNotBuilt = errors.New("vad: built without nx_onnx tag")

// Detector — интерфейс VAD-детектора. Вся реализация stateful (Silero v5
// содержит LSTM), поэтому потокобезопасность гарантируется вызывающей
// стороной: Infer() вызывается только из одного goroutine (audioLoop),
// Reset/Close — только при удержании внешнего mutex.
type Detector interface {
	// Infer вычисляет speech probability для окна ровно 512 sample @ 16 kHz
	// (требование Silero v5). Возвращает значение в [0, 1]. Обновляет
	// внутреннее LSTM-состояние.
	Infer(samples512 []int16) (speechProb float32, err error)

	// Reset сбрасывает LSTM-состояние (нужен при mute/unmute, join/leave).
	Reset()

	// Close освобождает ONNX-сессию. После этого Detector использовать нельзя.
	Close()
}

// WindowSize — размер "свежего" чанка аудио per-call (Silero v5 @ 16 kHz).
// На этом размере VoiceService буферизует семплы перед вызовом Infer.
const WindowSize = 512

// ContextSize — дополнительный префикс из последних семплов предыдущего
// chunk'а. Silero v5 требует, чтобы фактический input был concat(context, chunk).
// Без этого префикса модель получает "неправильное" окно и выдаёт мусор
// (state-counter pattern, prob ~ 0.0005 независимо от содержимого).
// Для 16 kHz context_size = 64 (Python: context_size = 64 if sr == 16000).
const ContextSize = 64

// InputSize — фактический размер тензора input модели = ContextSize + WindowSize.
const InputSize = ContextSize + WindowSize // 576

// SampleRate — фиксированная частота Silero v5.
const SampleRate = 16000

// NewSilero загружает Silero VAD модель из файла (runtime-load, не embed).
// Путь обычно bin/models/VAD/silero_vad.onnx. Под build-тегом nx_onnx
// возвращает работающий Detector; без тега — stub ошибку ErrNotBuilt.
//
// Тяжёлая операция (~100-300 мс на загрузку + warm-up), вызывать при
// JoinGroupVoice под горутину если нужно.
func NewSilero(modelPath string, log *slog.Logger) (Detector, error) {
	return newSilero(modelPath, log)
}
