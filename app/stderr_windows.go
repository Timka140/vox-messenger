package main

import (
	"os"
	"syscall"
)

// redirectStderr перенаправляет stderr (Go runtime panic output) в указанный файл.
func redirectStderr(f *os.File) {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	setStdHandle := kernel32.NewProc("SetStdHandle")
	// STD_ERROR_HANDLE = -12
	setStdHandle.Call(^uintptr(12-1), uintptr(f.Fd()))
	os.Stderr = f
}
