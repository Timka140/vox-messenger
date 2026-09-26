//go:build windows

package main

import (
	"log/slog"
	"time"

	"golang.org/x/sys/windows"
)

// waitForParentExit — Windows реализация.
//
// Используется в autoupdate flow: новый процесс не должен открывать БД пока
// старый держит SQLite WAL lock (иначе виснет в C-коде sqlite на PRAGMA
// journal_mode=WAL — busy_timeout не покрывает file-level locks).
//
// ВАЖНО: на Windows Go os.Process.Signal(syscall.Signal(0)) ВСЕГДА возвращает
// ошибку (реализация поддерживает только Kill, остальные сигналы → EWINDOWS).
// Поэтому проверка "процесс жив" через Signal(0) на Windows бесполезна — она
// сразу говорит "exited", что и произошло в логах до этого фикса
// (waited:0, checks:0, а старый процесс на самом деле был жив).
//
// Правильный способ — OpenProcess(SYNCHRONIZE) + WaitForSingleObject. Хэндл
// сразу сигналится когда процесс реально завершится; если процесс уже мёртв,
// OpenProcess вернёт ERROR_INVALID_PARAMETER и мы немедленно возвращаемся.
func waitForParentExit(log *slog.Logger, pid int, timeout time.Duration) {
	start := time.Now()

	// SYNCHRONIZE — минимальное право, нужное только для WaitForSingleObject.
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		// Процесс уже мёртв или недоступен — не на что ждать.
		log.Info("update: parent process gone (OpenProcess failed)",
			slog.Int("pid", pid),
			slog.String("error", err.Error()),
			slog.Duration("waited", time.Since(start)))
		return
	}
	defer windows.CloseHandle(handle)

	ms := uint32(timeout / time.Millisecond)
	event, err := windows.WaitForSingleObject(handle, ms)
	switch {
	case err != nil:
		log.Warn("update: WaitForSingleObject failed, proceeding anyway",
			slog.Int("pid", pid),
			slog.String("error", err.Error()),
			slog.Duration("waited", time.Since(start)))
	case event == uint32(windows.WAIT_OBJECT_0):
		log.Info("update: parent process exited",
			slog.Int("pid", pid),
			slog.Duration("waited", time.Since(start)))
	case event == uint32(windows.WAIT_TIMEOUT):
		log.Warn("update: parent process still alive after timeout, proceeding anyway",
			slog.Int("pid", pid),
			slog.Duration("timeout", timeout))
	default:
		log.Warn("update: WaitForSingleObject unexpected event",
			slog.Int("pid", pid),
			slog.Uint64("event", uint64(event)),
			slog.Duration("waited", time.Since(start)))
	}
}
