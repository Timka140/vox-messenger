// Package codec provides Opus audio encoding/decoding for voice transport.
// Uses vendored libopus static library (third_party/opus/).
package codec

/*
#cgo CFLAGS: -I${SRCDIR}/../../../third_party/opus
#cgo !linux LDFLAGS: -L${SRCDIR}/../../../third_party/opus -lopus -lm
#cgo linux LDFLAGS: -L${SRCDIR}/../../../third_party/opus/linux-amd64 -lopus -lm
#include <opus.h>
#include <stdlib.h>

// Wrappers for variadic opus_encoder_ctl (CGO cannot call variadic C functions).
extern int opus_encoder_ctl_set_bitrate(OpusEncoder *enc, opus_int32 bitrate);

// Inline wrappers для дополнительных int-setters. Объявляем прямо в preamble
// чтобы не плодить отдельный C-файл.
static inline int nx_opus_set_inband_fec(OpusEncoder *enc, int on) {
    return opus_encoder_ctl(enc, OPUS_SET_INBAND_FEC(on));
}
static inline int nx_opus_set_packet_loss_perc(OpusEncoder *enc, int perc) {
    return opus_encoder_ctl(enc, OPUS_SET_PACKET_LOSS_PERC(perc));
}
static inline int nx_opus_set_dtx(OpusEncoder *enc, int on) {
    return opus_encoder_ctl(enc, OPUS_SET_DTX(on));
}
*/
import "C"

import (
	"fmt"
	"sync"
	"unsafe"
)

const (
	SampleRate = 16000 // 16 kHz
	Channels   = 1     // mono
	FrameSize  = 320   // 20ms @ 16kHz
	MaxPacket  = 512   // max opus packet size for voice
)

// Звук демонстрации экрана (фаза 5e): музыка и видео, не речь.
const (
	AudioSampleRate = 48000
	AudioChannels   = 2
	AudioFrameSize  = 960 // 20 мс на канал
	AudioMaxPacket  = 1500
	AudioBitrate    = 96000
)

// Encoder wraps OpusEncoder for voice encoding.
//
// mu синхронизирует Encode (hot path из audioLoop) с SetDTX (редкие вызовы
// из UI-треда). opus_encoder_ctl setters атомарны, но читаются внутри
// opus_encode — без локи гонка видна в логах как редкие decode errors.
type Encoder struct {
	mu     sync.Mutex
	enc    *C.OpusEncoder
	frame  int // сэмплов на канал в кадре
	chans  int
	maxPkt int
}

// NewEncoder creates an Opus encoder optimized for VoIP (16kHz mono).
func NewEncoder() (*Encoder, error) {
	var errCode C.int
	enc := C.opus_encoder_create(C.opus_int32(SampleRate), C.int(Channels), C.OPUS_APPLICATION_VOIP, &errCode)
	if errCode != C.OPUS_OK {
		return nil, fmt.Errorf("opus_encoder_create: %s", C.GoString(C.opus_strerror(errCode)))
	}

	// Set bitrate to 24kbps — good quality for voice, low bandwidth
	C.opus_encoder_ctl_set_bitrate(enc, C.opus_int32(24000))

	// Inband FEC: encoder кладёт низкобитрейтную копию предыдущего фрейма в
	// текущий пакет. Даёт шанс декодеру восстановить потерянный фрейм, если
	// получен следующий (требует OPUS_SET_PACKET_LOSS_PERC чтобы FEC
	// действительно писался в bitstream).
	C.nx_opus_set_inband_fec(enc, C.int(1))
	C.nx_opus_set_packet_loss_perc(enc, C.int(10))

	return &Encoder{enc: enc, frame: FrameSize, chans: Channels, maxPkt: MaxPacket}, nil
}

// NewAudioEncoder — кодек звука демонстрации экрана (фаза 5e): 48 кГц стерео,
// OPUS_APPLICATION_AUDIO, 96 кбит/с, DTX — тишина не тратит трафик.
func NewAudioEncoder() (*Encoder, error) {
	var errCode C.int
	enc := C.opus_encoder_create(C.opus_int32(AudioSampleRate), C.int(AudioChannels), C.OPUS_APPLICATION_AUDIO, &errCode)
	if errCode != C.OPUS_OK {
		return nil, fmt.Errorf("opus_encoder_create: %s", C.GoString(C.opus_strerror(errCode)))
	}
	C.opus_encoder_ctl_set_bitrate(enc, C.opus_int32(AudioBitrate))
	C.nx_opus_set_dtx(enc, C.int(1))
	return &Encoder{enc: enc, frame: AudioFrameSize, chans: AudioChannels, maxPkt: AudioMaxPacket}, nil
}

// Encode converts PCM (16-bit LE, 320 samples) to Opus.
// Returns encoded Opus bytes. Пустой слайс (n==0) — валидный результат
// при включённом DTX: кодек не эмитит пакет на полной тишине.
func (e *Encoder) Encode(pcm []byte) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.enc == nil {
		return nil, fmt.Errorf("encoder closed")
	}
	if len(pcm) != e.frame*e.chans*2 {
		return nil, fmt.Errorf("expected %d bytes PCM, got %d", e.frame*e.chans*2, len(pcm))
	}

	output := make([]byte, e.maxPkt)
	n := C.opus_encode(
		e.enc,
		(*C.opus_int16)(unsafe.Pointer(&pcm[0])),
		C.int(e.frame),
		(*C.uchar)(unsafe.Pointer(&output[0])),
		C.opus_int32(e.maxPkt),
	)
	if n < 0 {
		return nil, fmt.Errorf("opus_encode: %s", C.GoString(C.opus_strerror(n)))
	}

	return output[:n], nil
}

// SetDTX включает/выключает Discontinuous Transmission. При DTX=1 энкодер
// не эмитит пакеты (или эмитит 1-байтный TOC) на полной тишине — даёт
// экономию трафика ~70% на паузах. Требует OPUS_APPLICATION_VOIP (уже стоит
// в NewEncoder) и моно (тоже ок). Hot-swap безопасен: mu сериализует
// с Encode в audioLoop.
func (e *Encoder) SetDTX(on bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.enc == nil {
		return fmt.Errorf("encoder closed")
	}
	var v C.int
	if on {
		v = 1
	}
	ret := C.nx_opus_set_dtx(e.enc, v)
	if ret != C.OPUS_OK {
		return fmt.Errorf("opus SET_DTX: %s", C.GoString(C.opus_strerror(ret)))
	}
	return nil
}

// Close destroys the encoder.
func (e *Encoder) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.enc != nil {
		C.opus_encoder_destroy(e.enc)
		e.enc = nil
	}
}

// Decoder wraps OpusDecoder for voice decoding.
type Decoder struct {
	dec   *C.OpusDecoder
	frame int // сэмплов на канал в кадре
	chans int
}

// NewDecoder creates an Opus decoder (16kHz mono).
func NewDecoder() (*Decoder, error) {
	var errCode C.int
	dec := C.opus_decoder_create(C.opus_int32(SampleRate), C.int(Channels), &errCode)
	if errCode != C.OPUS_OK {
		return nil, fmt.Errorf("opus_decoder_create: %s", C.GoString(C.opus_strerror(errCode)))
	}

	return &Decoder{dec: dec, frame: FrameSize, chans: Channels}, nil
}

// NewAudioDecoder — декодер звука демонстрации (48 кГц стерео).
func NewAudioDecoder() (*Decoder, error) {
	var errCode C.int
	dec := C.opus_decoder_create(C.opus_int32(AudioSampleRate), C.int(AudioChannels), &errCode)
	if errCode != C.OPUS_OK {
		return nil, fmt.Errorf("opus_decoder_create: %s", C.GoString(C.opus_strerror(errCode)))
	}
	return &Decoder{dec: dec, frame: AudioFrameSize, chans: AudioChannels}, nil
}

// Decode converts Opus frame to PCM (16-bit LE).
// Returns PCM bytes (640 bytes for 320 samples).
// Если wantFEC=true — Opus пытается вытащить LBRR-копию предыдущего фрейма
// из переданного пакета (используется для recovery потерянного фрейма,
// когда энкодер на той стороне включил inband FEC).
func (d *Decoder) Decode(opusData []byte, wantFEC bool) ([]byte, error) {
	if d.dec == nil {
		return nil, fmt.Errorf("decoder closed")
	}
	if len(opusData) == 0 {
		return nil, fmt.Errorf("empty opus data")
	}
	var fecFlag C.int
	if wantFEC {
		fecFlag = 1
	}
	pcm := make([]byte, d.frame*d.chans*2)
	n := C.opus_decode(
		d.dec,
		(*C.uchar)(unsafe.Pointer(&opusData[0])),
		C.opus_int32(len(opusData)),
		(*C.opus_int16)(unsafe.Pointer(&pcm[0])),
		C.int(d.frame),
		fecFlag,
	)
	if n < 0 {
		return nil, fmt.Errorf("opus_decode: %s", C.GoString(C.opus_strerror(n)))
	}

	return pcm[:int(n)*2*d.chans], nil // n samples на канал * 2 байта * каналы
}

// DecodePLC генерирует PCM для потерянного фрейма через встроенный
// packet-loss concealment Opus. Вызывается когда seq gap > 2 и FEC недоступен
// (LBRR копия есть только для одного предыдущего фрейма).
func (d *Decoder) DecodePLC() ([]byte, error) {
	if d.dec == nil {
		return nil, fmt.Errorf("decoder closed")
	}
	pcm := make([]byte, d.frame*d.chans*2)
	// opus_decode с data=NULL len=0 и decode_fec=0 — это PLC mode.
	n := C.opus_decode(
		d.dec,
		(*C.uchar)(nil),
		C.opus_int32(0),
		(*C.opus_int16)(unsafe.Pointer(&pcm[0])),
		C.int(d.frame),
		0,
	)
	if n < 0 {
		return nil, fmt.Errorf("opus_decode PLC: %s", C.GoString(C.opus_strerror(n)))
	}
	return pcm[:int(n)*2*d.chans], nil
}

// Close destroys the decoder.
func (d *Decoder) Close() {
	if d.dec != nil {
		C.opus_decoder_destroy(d.dec)
		d.dec = nil
	}
}
