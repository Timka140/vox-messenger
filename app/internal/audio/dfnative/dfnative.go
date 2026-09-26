// Package dfnative — DeepFilterNet3 через cgo-обёртку вокруг upstream crate
// `deep_filter` (см. third_party/libdf/). Использует встроенный C API (feature "capi")
// upstream-репа, тот же код что и deep-filter.exe из tools/ns-reference/pyref/.
//
// Заменяет старый Go-порт из internal/audio/dfn, который давал некорректный
// результат из-за расхождений в erb_fb / Vorbis window.
//
// Build tag `nx_dfn` включает реальную реализацию. Без тега — stub.
//
// Контракт:
//   proc, err := dfnative.New(dfnative.Config{ModelPath: "models/DeepFilterNet3.tar.gz"})
//   defer proc.Close()
//   proc.ProcessFrame(in480, out480) // НЕ in-place, in и out — разные буферы
//
// ВАЖНО: ModelPath — это путь к .tar.gz архиву с моделью (config.ini + *.onnx
// запакованные в tar+gzip), НЕ путь к распакованной папке. Upstream df_create
// внутри открывает файл как GzDecoder+Archive. Помощник для упаковки —
// third_party/libdf/pack_model.sh.
//
// ProcessFrame НЕ потокобезопасен и НЕ in-place safe — upstream пишет в output
// через ArrayViewMut2 пока читает из input через ArrayView2, один буфер для
// обоих даст UB aliasing.
package dfnative

import "errors"

var ErrNotBuilt = errors.New("dfnative: built without nx_dfn tag")

// FrameSize — 10 ms при 48 kHz, hop_size DFN3.
const FrameSize = 480

// SampleRate — рабочая частота DFN3.
const SampleRate = 48000

// Config — параметры создания процессора.
type Config struct {
	// ModelPath — путь к .tar.gz с моделью DeepFilterNet3.
	ModelPath string

	// AttenLimDb — ограничение подавления в дБ.
	// ВНИМАНИЕ upstream логика: >=100 → без ограничения (полное DFN-подавление),
	// <0.01 → passthrough (шумодав отключён!). Значение 20-30 → мягче,
	// оставляет часть шума. По умолчанию использовать 100.
	AttenLimDb float32

	// PostFilterBeta — бета post-filter (0 = выключен).
	PostFilterBeta float32
}
