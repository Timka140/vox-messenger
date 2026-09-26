//go:build !windows

package services

import (
	"os/exec"
	"path/filepath"
	"runtime"
)

// makeHiddenCmd — на не-Windows просто exec.Command; скрывать нечего.
func makeHiddenCmd(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...)
}

// revealInFolder показывает файл в файловом менеджере: Finder умеет выделить
// файл (open -R), на Linux открываем папку — единого способа выделить нет.
func revealInFolder(path string) error {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", "-R", path).Start()
	}
	return exec.Command("xdg-open", filepath.Dir(path)).Start()
}
