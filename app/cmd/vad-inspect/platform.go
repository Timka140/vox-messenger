//go:build nx_onnx

package main

import "runtime"

func defaultLibName() string {
	switch runtime.GOOS {
	case "windows":
		return "onnxruntime.dll"
	case "darwin":
		return "libonnxruntime.dylib"
	default:
		return "libonnxruntime.so"
	}
}

func runtimeSubdir() string {
	return runtime.GOOS + "-" + runtime.GOARCH
}
