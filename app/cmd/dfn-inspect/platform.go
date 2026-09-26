//go:build nx_onnx

package main

import "runtime"

// defaultLibName возвращает имя shared library onnxruntime для текущей ОС.
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

// runtimeSubdir — имя подпапки `lib/<os>-<arch>/`, как раскладывает download_runtime.sh.
func runtimeSubdir() string {
	return runtime.GOOS + "-" + runtime.GOARCH
}
