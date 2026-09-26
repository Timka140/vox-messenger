//go:build nx_onnx

// dfn-inspect — standalone утилита для печати I/O спецификаций ONNX-моделей
// DeepFilterNet3 (enc.onnx / erb_dec.onnx / df_dec.onnx).
//
// Назначение: узнать точные имена и размерности тензоров перед написанием
// inference-пайплайна, чтобы не писать ERB/STFT порт вслепую.
//
// Сборка:
//
//	cd app
//	go build -tags nx_onnx -o bin/dfn-inspect.exe ./cmd/dfn-inspect
//
// Запуск:
//
//	./bin/dfn-inspect.exe ../.local/models/DeepFilterNet3
//
// Если onnxruntime.dll не лежит рядом с exe — укажи путь явно:
//
//	./bin/dfn-inspect.exe -runtime ../app/internal/ml/onnx/lib/windows-amd64/onnxruntime.dll ../.local/models/DeepFilterNet3
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	ort "github.com/yalue/onnxruntime_go"
)

func main() {
	runtime := flag.String("runtime", "", "путь к onnxruntime.dll / libonnxruntime.so / libonnxruntime.dylib (опционально)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [-runtime path] <models-dir>\n", os.Args[0])
		fmt.Fprintln(os.Stderr, "  <models-dir> — папка с enc.onnx, erb_dec.onnx, df_dec.onnx")
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(2)
	}
	modelsDir := flag.Arg(0)

	// Пытаемся найти onnxruntime.dll: из флага → NEXUS_ONNXRUNTIME_LIB → рядом с exe
	// → стандартный путь проекта относительно cwd.
	libPath := *runtime
	if libPath == "" {
		libPath = os.Getenv("NEXUS_ONNXRUNTIME_LIB")
	}
	if libPath == "" {
		if exe, err := os.Executable(); err == nil {
			candidate := filepath.Join(filepath.Dir(exe), defaultLibName())
			if _, err := os.Stat(candidate); err == nil {
				libPath = candidate
			}
		}
	}
	if libPath == "" {
		// Последний шанс — путь из download_runtime.sh относительно cwd.
		candidate := filepath.Join("internal", "ml", "onnx", "lib", runtimeSubdir(), defaultLibName())
		if _, err := os.Stat(candidate); err == nil {
			libPath = candidate
		}
	}

	if libPath != "" {
		fmt.Fprintf(os.Stderr, "[info] onnxruntime lib: %s\n", libPath)
		ort.SetSharedLibraryPath(libPath)
	} else {
		fmt.Fprintln(os.Stderr, "[warn] onnxruntime lib path не задан — полагаемся на системный loader")
	}

	if err := ort.InitializeEnvironment(); err != nil {
		fmt.Fprintf(os.Stderr, "init env failed: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = ort.DestroyEnvironment() }()

	models := []string{"enc.onnx", "erb_dec.onnx", "df_dec.onnx"}
	anyErr := false
	for _, m := range models {
		path := filepath.Join(modelsDir, m)
		fmt.Printf("=== %s ===\n", m)
		if _, err := os.Stat(path); err != nil {
			fmt.Printf("  SKIP: %v\n\n", err)
			anyErr = true
			continue
		}
		ins, outs, err := ort.GetInputOutputInfo(path)
		if err != nil {
			fmt.Printf("  ERROR: %v\n\n", err)
			anyErr = true
			continue
		}
		fmt.Printf("  Inputs (%d):\n", len(ins))
		for i := range ins {
			fmt.Printf("    [%d] %s\n", i, ins[i].String())
		}
		fmt.Printf("  Outputs (%d):\n", len(outs))
		for i := range outs {
			fmt.Printf("    [%d] %s\n", i, outs[i].String())
		}
		fmt.Println()
	}

	if anyErr {
		os.Exit(1)
	}
}
