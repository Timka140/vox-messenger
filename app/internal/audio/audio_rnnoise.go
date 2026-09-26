//go:build nx_rnnoise

// Build-tag для включения RNNoise в audio pipeline.
//
// Сборка:
//   CGO_ENABLED=1 go build -tags nx_rnnoise ./...
//
// Что делает:
//   1. Определяет NX_USE_RNNOISE → активирует #ifdef в audio.go и rnnoise_unity.c.
//   2. Добавляет include paths для vendored xiph/rnnoise (subdir rnnoise/).
//   3. Линкует libm на Linux (Windows MinGW и macOS — libm подмешана автоматически).
//
// rnnoise_unity.c в это время компилирует все нужные .c одним TU. Веса модели
// (rnnoise_data.c, ~50 МБ tarball распакован) скачиваются один раз через
// rnnoise/download_model.sh — делать заново не нужно, файлы под git-ignore стоит
// добавить отдельно если pre-checkin размер критичен (см. README пакета).

package audio

// #cgo CFLAGS: -DNX_USE_RNNOISE -O3 -I${SRCDIR}/rnnoise/include -I${SRCDIR}/rnnoise/src
// #cgo amd64 CFLAGS: -msse4.1 -mssse3
// #cgo linux LDFLAGS: -lm
import "C"
