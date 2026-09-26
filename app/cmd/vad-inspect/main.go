//go:build nx_onnx

// vad-inspect — standalone утилита для печати I/O спецификаций одной ONNX-модели.
// Нужна чтобы узнать точные имена и размерности тензоров Silero VAD перед
// написанием inference-кода. По образцу cmd/dfn-inspect, но для одного файла.
//
// Сборка:
//
//	cd app
//	go build -tags nx_onnx -o bin/vad-inspect.exe ./cmd/vad-inspect
//
// Запуск:
//
//	./bin/vad-inspect.exe ../.local/models/VAD/silero_vad.onnx
//
// Если onnxruntime.dll не лежит рядом с exe — указать путь через -runtime или
// переменную NEXUS_ONNXRUNTIME_LIB.
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
		fmt.Fprintf(os.Stderr, "Usage: %s [-runtime path] <model.onnx>\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(2)
	}
	modelPath := flag.Arg(0)

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

	if _, err := os.Stat(modelPath); err != nil {
		fmt.Fprintf(os.Stderr, "model not found: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("=== %s ===\n", filepath.Base(modelPath))
	ins, outs, err := ort.GetInputOutputInfo(modelPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  ERROR: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  Inputs (%d):\n", len(ins))
	for i := range ins {
		fmt.Printf("    [%d] %s\n", i, ins[i].String())
	}
	fmt.Printf("  Outputs (%d):\n", len(outs))
	for i := range outs {
		fmt.Printf("    [%d] %s\n", i, outs[i].String())
	}
}
