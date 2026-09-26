// Package onnx — generic обёртка над onnxruntime-go для ML-инференса.
//
// Предназначен как единый слой для всех ML-моделей в Nexus (аудио шумодав,
// в перспективе VAD, semantic search, typo-fix, STT). Build-tag nx_onnx включает
// реальную реализацию через yalue/onnxruntime_go + нативную onnxruntime DLL/so.
// Без тега пакет компилируется как stub — Available() == false, NewSession возвращает ошибку.
//
// Архитектурное разделение:
//
//   - onnx.go           — публичное API (этот файл, без тега)
//   - session.go        — реальная реализация (//go:build nx_onnx)
//   - session_stub.go   — no-op реализация (//go:build !nx_onnx)
//   - runtime_loader.go — поиск нативной onnxruntime библиотеки (//go:build nx_onnx)
//
// Не должен импортироваться из основного кода — только из build-tagged файлов
// потребителей (например, audio_dtln.go с //go:build nx_onnx). Для проверки
// наличия на стороне нетэгированного кода используется onnx.Available().
package onnx

import "errors"

// ErrNotBuilt возвращается когда бинарь собран без -tags nx_onnx.
var ErrNotBuilt = errors.New("onnx: built without nx_onnx tag")

// ErrRuntimeNotFound возвращается когда бинарь собран с тегом, но нативная
// onnxruntime библиотека (onnxruntime.dll / libonnxruntime.so / libonnxruntime.dylib)
// не найдена рядом с exe или по путям из NEXUS_ONNXRUNTIME_LIB.
var ErrRuntimeNotFound = errors.New("onnx: native runtime library not found")

// Available — читается как package-level var чтобы работать с build-тегами.
// В stub-реализации остаётся false, в реальной — выставляется init() после
// успешной инициализации runtime.
//
// Пример использования из нетэгированного кода:
//
//	if onnx.Available() {
//	    svc.DoThing()
//	}
func Available() bool {
	return runtimeAvailable
}

// runtimeAvailable — internal флаг, выставляется runtime_loader.go в onnx_enabled,
// либо остаётся false в stub.
var runtimeAvailable = false

// InitError возвращает последнюю ошибку инициализации runtime (если была).
// Полезно для логирования почему Available() == false.
func InitError() error {
	return initErr
}

var initErr error = ErrNotBuilt
