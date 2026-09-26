//go:build (windows && amd64) || (linux && amd64)

// Package h264enc — software H.264-энкодер (Cisco openh264) для демонстрации
// экрана. cgo-биндинг и статическая библиотека взяты из
// github.com/pion/mediadevices/pkg/codec/openh264 (BSD, см.
// LICENSE.openh264-bindings) и урезаны до одного синхронного API без
// зависимости на mediadevices: тот линкует `-lssp`, которой нет в mingw-w64 ≥ 13
// (локальная сборка падала), и тянет за собой весь фреймворк.
//
// Вход — I420 (image.YCbCr 4:2:0), выход — Annex-B NAL units одного кадра.
package h264enc

/*
#cgo CFLAGS: -I${SRCDIR}/include
#cgo CXXFLAGS: -I${SRCDIR}/include
#cgo windows,amd64 LDFLAGS: ${SRCDIR}/lib/libopenh264-windows-x64.a -static-libgcc -static-libstdc++ -Wl,-Bstatic -lstdc++ -lpthread -Wl,-Bdynamic
#cgo linux,amd64 LDFLAGS: ${SRCDIR}/lib/libopenh264-linux-x64.a -lstdc++ -lm
#include <openh264/codec_api.h>
#include "bridge.hpp"
*/
import "C"

import (
	"errors"
	"fmt"
	"image"
	"runtime"
	"sync"
	"unsafe"
)

// Config — параметры энкодера. Bitrate в бит/с; IntraPeriod — период
// keyframe в кадрах (0 → 2×FPS); Threads — 0 = авто (min(4, NumCPU)).
type Config struct {
	Width, Height int
	FPS           int
	Bitrate       int
	IntraPeriod   int
	Threads       int
}

// Encoder — обёртка над ISVCEncoder. Не потокобезопасен: Encode/SetBitrate/
// Close сериализуются внутренним mutex'ом, но вызывать из одной горутины
// (captureLoop) всё равно правильнее.
type Encoder struct {
	mu     sync.Mutex
	engine *C.Encoder
	cfg    Config
	closed bool
}

// ErrUnsupported — платформа без статической libopenh264 (см. build tags).
var ErrUnsupported = errors.New("h264enc: unsupported platform")

// New создаёт энкодер. UsageType = SCREEN_CONTENT_REAL_TIME, RC = bitrate,
// frame skip выключен (темп держит вызывающий).
func New(cfg Config) (*Encoder, error) {
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.FPS <= 0 || cfg.Bitrate <= 0 {
		return nil, fmt.Errorf("h264enc: bad config %+v", cfg)
	}
	if cfg.Width%2 != 0 || cfg.Height%2 != 0 {
		return nil, fmt.Errorf("h264enc: размеры должны быть чётными (%dx%d)", cfg.Width, cfg.Height)
	}
	if cfg.IntraPeriod <= 0 {
		cfg.IntraPeriod = cfg.FPS * 2
	}
	if cfg.Threads <= 0 {
		cfg.Threads = runtime.NumCPU()
		if cfg.Threads > 4 {
			cfg.Threads = 4
		}
	}
	var rv C.int
	eng := C.enc_new(C.EncoderOptions{
		width:                 C.int(cfg.Width),
		height:                C.int(cfg.Height),
		target_bitrate:        C.int(cfg.Bitrate),
		max_fps:               C.float(cfg.FPS),
		usage_type:            C.SCREEN_CONTENT_REAL_TIME,
		rc_mode:               C.RC_BITRATE_MODE,
		enable_frame_skip:     C.bool(false),
		max_nal_size:          0,
		intra_period:          C.uint(cfg.IntraPeriod),
		multiple_thread_idc:   C.int(cfg.Threads),
		slice_num:             1,
		slice_mode:            C.SM_SINGLE_SLICE,
		slice_size_constraint: 0,
	}, &rv)
	if err := errResult(rv); err != nil {
		return nil, fmt.Errorf("h264enc: create encoder: %w", err)
	}
	if eng == nil {
		return nil, errors.New("h264enc: create encoder: nil engine")
	}
	return &Encoder{engine: eng, cfg: cfg}, nil
}

// Config возвращает параметры, с которыми создан энкодер.
func (e *Encoder) Config() Config { return e.cfg }

// Encode кодирует один I420-кадр. Возвращает копию Annex-B потока (SPS/PPS/
// IDR для keyframe, иначе P-slice) и признак keyframe. Пустой результат
// (len 0) означает, что кодек пропустил кадр.
func (e *Encoder) Encode(img *image.YCbCr, forceKey bool) ([]byte, bool, error) {
	if img == nil || img.SubsampleRatio != image.YCbCrSubsampleRatio420 {
		return nil, false, errors.New("h264enc: нужен image.YCbCr 4:2:0")
	}
	b := img.Bounds()
	if b.Dx() != e.cfg.Width || b.Dy() != e.cfg.Height {
		return nil, false, fmt.Errorf("h264enc: кадр %dx%d, энкодер %dx%d", b.Dx(), b.Dy(), e.cfg.Width, e.cfg.Height)
	}
	if len(img.Y) == 0 || len(img.Cb) == 0 || len(img.Cr) == 0 {
		return nil, false, errors.New("h264enc: пустые плоскости")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, false, errors.New("h264enc: encoder closed")
	}
	if forceKey {
		e.engine.force_key_frame = C.int(1)
	}
	var rv C.int
	s := C.enc_encode(e.engine, C.Frame{
		y:       unsafe.Pointer(&img.Y[0]),
		u:       unsafe.Pointer(&img.Cb[0]),
		v:       unsafe.Pointer(&img.Cr[0]),
		ystride: C.int(img.YStride),
		cstride: C.int(img.CStride),
		height:  C.int(b.Dy()),
		width:   C.int(b.Dx()),
	}, &rv)
	if err := errResult(rv); err != nil {
		return nil, false, fmt.Errorf("h264enc: encode: %w", err)
	}
	if s.data_len == 0 {
		return nil, false, nil
	}
	out := C.GoBytes(unsafe.Pointer(s.data), s.data_len)
	return out, IsKeyframe(out), nil
}

// SetBitrate меняет целевой битрейт (бит/с) на лету.
func (e *Encoder) SetBitrate(bps int) {
	if bps <= 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	C.enc_set_bitrate(e.engine, C.int(bps))
}

// Close освобождает энкодер. Повторный вызов — no-op.
func (e *Encoder) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	e.closed = true
	var rv C.int
	C.enc_free(e.engine, &rv)
	e.engine = nil
}

type eResult int

func (r eResult) Error() string {
	switch r {
	case -1:
		return "failed"
	case -2:
		return "invalid param"
	case -3:
		return "out of memory"
	case -4:
		return "not supported"
	case -5:
		return "unexpected"
	case -6:
		return "need reinit"
	default:
		return fmt.Sprintf("openh264 error %d", int(r))
	}
}

func errResult(rv C.int) error {
	if rv == 0 {
		return nil
	}
	return eResult(rv)
}
