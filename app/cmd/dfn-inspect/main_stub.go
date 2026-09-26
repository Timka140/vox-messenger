//go:build !nx_onnx

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "dfn-inspect: бинарь собран без nx_onnx тега — ONNX недоступен.")
	fmt.Fprintln(os.Stderr, "Пересобери: go build -tags nx_onnx -o bin/dfn-inspect.exe ./cmd/dfn-inspect")
	os.Exit(1)
}
