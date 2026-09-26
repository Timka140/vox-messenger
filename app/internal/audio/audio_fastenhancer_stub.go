//go:build !nx_onnx

// Stub для сборки без -tags nx_onnx — FastEnhancer недоступен, NewFastEnhancer
// возвращает ErrNotBuilt; Process bypass'ит звук без изменений. UI в этом
// случае должен скрывать опцию (FastEnhancerAvailable() == false).

package audio

import (
	"errors"
	"log/slog"
)

// FastEnhancerAvailable — false в stub-сборке.
func FastEnhancerAvailable() bool { return false }

type FastEnhancerSize string

const (
	FETiny   FastEnhancerSize = "t"
	FEBase   FastEnhancerSize = "b"
	FESmall  FastEnhancerSize = "s"
	FEMedium FastEnhancerSize = "m"
	FELarge  FastEnhancerSize = "l"
)

// errFastEnhancerNotBuilt — возвращается из NewFastEnhancer в stub-сборке.
var errFastEnhancerNotBuilt = errors.New("fastenhancer: built without nx_onnx tag")

type FastEnhancer struct{}

func NewFastEnhancer(size FastEnhancerSize, log *slog.Logger) (*FastEnhancer, error) {
	return nil, errFastEnhancerNotBuilt
}

func (fe *FastEnhancer) SetMix(mix float32)              {}
func (fe *FastEnhancer) Size() FastEnhancerSize          { return "" }
func (fe *FastEnhancer) Process(pcm []byte) []byte       { return pcm }
func (fe *FastEnhancer) Close()                          {}
func (fe *FastEnhancer) Overloaded() bool                { return false }
