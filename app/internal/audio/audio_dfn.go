//go:build nx_dfn

// Build-tag для включения DeepFilterNet3 в audio pipeline.
//
// Сборка:
//
//	CGO_ENABLED=1 go build -tags nx_dfn ./...
//	CGO_ENABLED=1 go build -tags "nx_dfn nx_rnnoise" ./...
//
// Что делает:
//  1. Определяет NX_USE_DFN → активирует #ifdef в audio.go (см. блок DFN3 hook).
//  2. Линкует staticlib third_party/libdf/target/release/libnx_df.a (см. third_party/libdf/README.md).
//  3. На Windows использует кросс-таргет x86_64-pc-windows-gnu.
//
// libnx_df.a предварительно собирается из third_party/libdf/ через
//   cargo build --release --manifest-path third_party/libdf/Cargo.toml
// (для Linux) или --target x86_64-pc-windows-gnu (для Windows). См. release.sh.
//
// RNNoise и DFN3 могут сосуществовать: при NX_NS_DEEP и загруженной DFN-модели
// используется DFN3, иначе fallback на RNNoise (если собран с nx_rnnoise).

package audio

// #cgo CFLAGS: -DNX_USE_DFN -I${SRCDIR}/../../../third_party/libdf/include
// #cgo linux   LDFLAGS: -L${SRCDIR}/../../../third_party/libdf/target/release -Wl,-Bstatic -lnx_df -Wl,-Bdynamic -lm -ldl -lpthread
// #cgo windows LDFLAGS: -L${SRCDIR}/../../../third_party/libdf/target/x86_64-pc-windows-gnu/release -Wl,-Bstatic -lnx_df -Wl,-Bdynamic -lws2_32 -luserenv -lntdll -lbcrypt
// #cgo darwin  LDFLAGS: -L${SRCDIR}/../../../third_party/libdf/target/release -lnx_df -framework Security -framework CoreFoundation
import "C"
