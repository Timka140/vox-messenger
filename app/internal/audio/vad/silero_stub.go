//go:build !nx_onnx

package vad

import "log/slog"

func newSilero(_ string, _ *slog.Logger) (Detector, error) {
	return nil, ErrNotBuilt
}
