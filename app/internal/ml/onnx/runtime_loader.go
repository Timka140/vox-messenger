//go:build nx_onnx

package onnx

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	ort "github.com/yalue/onnxruntime_go"
)

// Поиск нативной onnxruntime библиотеки производится в следующем порядке:
//   1. $NEXUS_ONNXRUNTIME_LIB — явный путь (абсолютный или относительный к cwd)
//   2. Рядом с exe: <exeDir>/<platformLibName>
//   3. <exeDir>/onnxruntime/<platformLibName>
//   4. Рядом с exe в подпапке lib/<os>-<arch>/<platformLibName>
//
// Если ни один путь не найден, runtimeAvailable остаётся false,
// initErr = ErrRuntimeNotFound. Вызывающий видит это через Available()/InitError().
//
// Имя библиотеки зависит от платформы:
//   windows: onnxruntime.dll
//   linux:   libonnxruntime.so
//   darwin:  libonnxruntime.dylib

func init() {
	path, err := findRuntimeLibrary()
	if err != nil {
		initErr = err
		return
	}
	ort.SetSharedLibraryPath(path)
	if err := ort.InitializeEnvironment(); err != nil {
		initErr = fmt.Errorf("onnx: init runtime %q: %w", path, err)
		return
	}
	runtimeAvailable = true
	initErr = nil
}

// TryInit форсирует повторную инициализацию runtime с явным путём к библиотеке.
// Нужно когда бинарь собран отдельно (cmd/dfn-test, cmd/dfn-inspect) и экзешник
// лежит не там, где ожидает findRuntimeLibrary.
//
// Если runtime уже инициализирован — no-op. Если первая попытка init() провалилась
// и TryInit вызван с валидным путём — перезапускает SetSharedLibraryPath +
// InitializeEnvironment и выставляет runtimeAvailable = true.
//
// Вызывать строго из main ДО любого onnx.NewSession.
func TryInit(libPath string) error {
	if runtimeAvailable {
		return nil
	}
	if libPath == "" {
		return ErrRuntimeNotFound
	}
	if !fileExists(libPath) {
		return fmt.Errorf("onnx: TryInit: %s: %w", libPath, ErrRuntimeNotFound)
	}
	ort.SetSharedLibraryPath(libPath)
	if err := ort.InitializeEnvironment(); err != nil {
		initErr = fmt.Errorf("onnx: TryInit %q: %w", libPath, err)
		return initErr
	}
	runtimeAvailable = true
	initErr = nil
	return nil
}

func platformLibName() string {
	switch runtime.GOOS {
	case "windows":
		return "onnxruntime.dll"
	case "darwin":
		return "libonnxruntime.dylib"
	default:
		return "libonnxruntime.so"
	}
}

func findRuntimeLibrary() (string, error) {
	name := platformLibName()

	if env := os.Getenv("NEXUS_ONNXRUNTIME_LIB"); env != "" {
		if fileExists(env) {
			return env, nil
		}
	}

	exe, err := os.Executable()
	if err == nil {
		exeDir := filepath.Dir(exe)
		platformDir := fmt.Sprintf("%s-%s", runtime.GOOS, runtime.GOARCH)
		candidates := []string{
			filepath.Join(exeDir, name),
			filepath.Join(exeDir, "onnxruntime", name),
			filepath.Join(exeDir, "lib", platformDir, name),
		}
		for _, c := range candidates {
			if fileExists(c) {
				return c, nil
			}
		}
	}

	return "", ErrRuntimeNotFound
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
