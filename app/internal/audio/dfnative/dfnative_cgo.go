//go:build nx_dfn

package dfnative

// #cgo CFLAGS: -I${SRCDIR}/../../../../third_party/libdf/include
// #cgo linux   LDFLAGS: -L${SRCDIR}/../../../../third_party/libdf/target/release -lnx_df -lm -ldl -lpthread
// #cgo windows LDFLAGS: -L${SRCDIR}/../../../../third_party/libdf/target/x86_64-pc-windows-gnu/release -lnx_df -lws2_32 -luserenv -lntdll -lbcrypt
// #cgo darwin  LDFLAGS: -L${SRCDIR}/../../../../third_party/libdf/target/release -lnx_df -framework Security -framework CoreFoundation
// #include <stdlib.h>
// #include "nx_df.h"
import "C"

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"
)

// Processor — DFN3-процессор. Держит указатель на Rust DFState.
type Processor struct {
	state    *C.DFState
	frameLen int
}

// New создаёт процессор. ModelPath — путь к .tar.gz архиву модели.
func New(cfg Config) (*Processor, error) {
	if cfg.ModelPath == "" {
		return nil, errors.New("dfnative: ModelPath is empty")
	}
	cpath := C.CString(cfg.ModelPath)
	defer C.free(unsafe.Pointer(cpath))

	st := C.df_create(cpath, C.float(cfg.AttenLimDb))
	if st == nil {
		return nil, fmt.Errorf("dfnative: df_create failed for %q", cfg.ModelPath)
	}
	flen := int(C.df_get_frame_length(st))
	if flen != FrameSize {
		C.df_free(st)
		return nil, fmt.Errorf("dfnative: unexpected frame_len %d (want %d)", flen, FrameSize)
	}
	if cfg.PostFilterBeta > 0 {
		C.df_set_post_filter_beta(st, C.float(cfg.PostFilterBeta))
	}
	p := &Processor{state: st, frameLen: flen}
	runtime.SetFinalizer(p, func(p *Processor) { _ = p.Close() })
	return p, nil
}

// ProcessFrame обрабатывает один 10 ms кадр. in и out ДОЛЖНЫ быть разными
// буферами длины FrameSize. Возвращает LSNR (local SNR в дБ).
func (p *Processor) ProcessFrame(in, out []float32) (float32, error) {
	if p.state == nil {
		return 0, errors.New("dfnative: processor closed")
	}
	if len(in) != p.frameLen || len(out) != p.frameLen {
		return 0, fmt.Errorf("dfnative: frame len mismatch in=%d out=%d want=%d",
			len(in), len(out), p.frameLen)
	}
	if &in[0] == &out[0] {
		return 0, errors.New("dfnative: in and out must be distinct buffers (aliasing UB)")
	}
	lsnr := C.df_process_frame(
		p.state,
		(*C.float)(unsafe.Pointer(&in[0])),
		(*C.float)(unsafe.Pointer(&out[0])),
	)
	return float32(lsnr), nil
}

// SetAttenLim меняет ограничение подавления (дБ).
func (p *Processor) SetAttenLim(limDb float32) {
	if p.state != nil {
		C.df_set_atten_lim(p.state, C.float(limDb))
	}
}

// Close освобождает Rust state. Идемпотентно.
func (p *Processor) Close() error {
	if p.state != nil {
		C.df_free(p.state)
		p.state = nil
	}
	runtime.SetFinalizer(p, nil)
	return nil
}
