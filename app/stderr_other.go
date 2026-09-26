//go:build !windows

package main

import (
	"os"
	"syscall"
)

// redirectStderr перенаправляет stderr (Go runtime panic output) в указанный файл.
func redirectStderr(f *os.File) {
	syscall.Dup2(int(f.Fd()), 2)
	os.Stderr = f
}
