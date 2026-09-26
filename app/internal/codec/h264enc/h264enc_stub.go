//go:build !((windows && amd64) || (linux && amd64))

package h264enc

import (
	"errors"
	"image"
)

// Config — см. h264enc.go.
type Config struct {
	Width, Height int
	FPS           int
	Bitrate       int
	IntraPeriod   int
	Threads       int
}

// Encoder — заглушка для платформ без статической libopenh264.
type Encoder struct{ cfg Config }

// ErrUnsupported — платформа без статической libopenh264.
var ErrUnsupported = errors.New("h264enc: unsupported platform")

func New(Config) (*Encoder, error)                                 { return nil, ErrUnsupported }
func (e *Encoder) Config() Config                                  { return e.cfg }
func (e *Encoder) Encode(*image.YCbCr, bool) ([]byte, bool, error) { return nil, false, ErrUnsupported }
func (e *Encoder) SetBitrate(int)                                  {}
func (e *Encoder) Close()                                          {}
