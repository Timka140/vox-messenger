//go:build !nx_onnx

package audio

import (
	"errors"
	"log/slog"
)

type GTCRNVariant string

const (
	GTCRNDNS3   GTCRNVariant = "dns3"
	GTCRNSimple GTCRNVariant = "simple"
)

var errGTCRNNotBuilt = errors.New("gtcrn: built without nx_onnx tag")

type GTCRN struct{}

func NewGTCRN(variant GTCRNVariant, log *slog.Logger) (*GTCRN, error) {
	return nil, errGTCRNNotBuilt
}

func (g *GTCRN) SetMix(mix float32)         {}
func (g *GTCRN) Variant() GTCRNVariant      { return "" }
func (g *GTCRN) Process(pcm []byte) []byte  { return pcm }
func (g *GTCRN) Close()                     {}
func (g *GTCRN) Overloaded() bool           { return false }

// GTCRNAvailable — доступен только в nx_onnx сборке (как FastEnhancer).
func GTCRNAvailable() bool { return false }
