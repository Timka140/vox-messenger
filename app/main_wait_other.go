//go:build !windows

package main

import (
	"log/slog"
	"os"
	"syscall"
	"time"
)

// waitForParentExit — POSIX реализация (Linux/macOS).
//
// На unix Signal(0) — канонический способ проверки существования процесса:
// он не отправляет сигнал, но проходит через kill(2) permission check и
// возвращает ESRCH если процесс не существует.
func waitForParentExit(log *slog.Logger, pid int, timeout time.Duration) {
	start := time.Now()
	deadline := start.Add(timeout)
	checked := 0
	for time.Now().Before(deadline) {
		proc, err := os.FindProcess(pid)
		if err != nil {
			log.Info("update: parent process gone (FindProcess error)",
				slog.Int("pid", pid),
				slog.Duration("waited", time.Since(start)))
			return
		}
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			log.Info("update: parent process exited",
				slog.Int("pid", pid),
				slog.Duration("waited", time.Since(start)),
				slog.Int("checks", checked))
			return
		}
		checked++
		time.Sleep(200 * time.Millisecond)
	}
	log.Warn("update: parent process still alive after timeout, proceeding anyway",
		slog.Int("pid", pid),
		slog.Duration("timeout", timeout))
}
