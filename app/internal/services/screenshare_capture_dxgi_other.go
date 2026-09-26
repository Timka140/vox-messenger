//go:build !windows

package services

import "errors"

// errDXGIUnavailable — DXGI есть только на Windows; openBestScreenCapture
// сразу уходит в BitBlt/X11 backend kbinani.
var errDXGIUnavailable = errors.New("dxgi: unavailable on this platform")

func openDXGICapture(int, int) (screenCapture, error) { return nil, errDXGIUnavailable }
