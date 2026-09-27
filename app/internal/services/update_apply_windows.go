//go:build windows

package services

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const updateRelaunchFlag = "--from-update"
const updateWaitPidFlag = "--wait-pid"

// startDetached запускает новый exe полностью независимо от текущего процесса.
// Использует `cmd.exe /C start` — канонический Windows способ detach GUI приложения.
// Альтернатива (exec.Command + DETACHED_PROCESS + Release) на практике даёт
// child который тихо крашится при выходе parent — видимо из-за наследуемых handle.
//
// Передаёт PID текущего процесса как `--wait-pid <N>` — новый exe ждёт пока
// мы реально умрём, прежде чем открывать БД (иначе race на WAL lock,
// см. main.go waitForParentExit).
//
// Право на foreground выдаётся всем (ASFW_ANY) до запуска: PID здесь есть
// только у промежуточного cmd.exe, который завершается сразу, а без права
// окно нового процесса откроется под другими окнами.
//
// Командная строка собирается целиком: exec.Command берёт путь в кавычки
// только при пробеле, и cmd разбирал бы & ^ | в пути как свои операторы
// (D:\R&D\vox.exe → «D:\R» и команда «D\vox.exe»). /S снимает внешние
// кавычки, путь остаётся в своих; "" — заголовок окна для start.
func startDetached(log *slog.Logger, exe string) error {
	// Несуществующий exe start не отвергает, а показывает окно ошибки и
	// ждёт, пока его закроют, — проверяем заранее.
	if _, err := os.Stat(exe); err != nil {
		return err
	}
	if err := allowSetForegroundWindowAny(); err != nil {
		log.Debug("AllowSetForegroundWindow не удался, окно может открыться позади",
			slog.String("error", err.Error()))
	}
	cmd := exec.Command("cmd.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
		CmdLine: fmt.Sprintf(`cmd.exe /S /C "start "" /B "%s" %s %s %d"`,
			exe, updateRelaunchFlag, updateWaitPidFlag, os.Getpid()),
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// start возвращается сразу; ненулевой код — новый процесс не запустился.
	// Завис (окно ошибки ждёт пользователя) — считаем, что не запустился.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("cmd start: %w", err)
		}
		return nil
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		return fmt.Errorf("cmd start: нет ответа за 15 с")
	}
}

// ASFW_ANY: право на foreground получает любой процесс.
const asfwAny = ^uintptr(0)

var procAllowSetForegroundWindow = user32.NewProc("AllowSetForegroundWindow")

// allowSetForegroundWindowAny разрешает любому процессу забрать foreground
// в обход стандартных Windows-ограничений.
func allowSetForegroundWindowAny() error {
	if r1, _, err := procAllowSetForegroundWindow.Call(asfwAny); r1 == 0 {
		return err
	}
	return nil
}

// applyUpdatePlatform выполняет apply для Windows:
// 1. Rename текущего exe → exe.old-<prev> (Windows позволяет rename работающего exe)
// 2. Copy pending → exe (не rename — staging может быть на другом volume)
// 3. Start нового exe в отдельной process group
// 4. Возврат — UpdateService вызовет quit (main.go: app.Quit())
//
// Failure recovery: если copy упал после rename — пытаемся rename обратно.
func applyUpdatePlatform(log *slog.Logger, exe, pending, prevVersion string) error {
	backup := exe + ".old-" + prevVersion

	// Удалить существующий backup с тем же именем (single slot, replace older)
	_ = os.Remove(backup)

	// Step 1: rename
	if err := os.Rename(exe, backup); err != nil {
		return fmt.Errorf("rename exe → backup: %w", err)
	}
	log.Info("update: renamed current exe to backup",
		slog.String("backup", backup))

	// Step 2: copy pending → exe
	if err := copyFile(pending, exe); err != nil {
		// Rollback
		if rerr := os.Rename(backup, exe); rerr != nil {
			return fmt.Errorf("copy failed: %v; rollback also failed: %v", err, rerr)
		}
		return fmt.Errorf("copy pending → exe: %w", err)
	}

	log.Info("update: new binary in place, starting new process",
		slog.String("exe", exe))

	// Step 3: запускаем через cmd /C start — полностью независимый процесс
	if err := startDetached(log, exe); err != nil {
		_ = os.Remove(exe)
		_ = os.Rename(backup, exe)
		return fmt.Errorf("start new exe: %w", err)
	}
	log.Info("update: new process started")
	return nil
}

// rollbackUpdatePlatform — откат на предыдущую версию (Windows).
// 1. Current exe → exe.failed-<current>
// 2. Backup (exe.old-*) → exe
// 3. Start нового exe
func rollbackUpdatePlatform(log *slog.Logger, exe, backup, currentVersion string) error {
	failed := exe + ".failed-" + currentVersion
	_ = os.Remove(failed)

	if err := os.Rename(exe, failed); err != nil {
		return fmt.Errorf("rename exe → failed: %w", err)
	}
	if err := os.Rename(backup, exe); err != nil {
		// Rollback rollback
		_ = os.Rename(failed, exe)
		return fmt.Errorf("rename backup → exe: %w", err)
	}

	log.Info("rollback: restored previous binary",
		slog.String("exe", exe),
		slog.String("failed_kept_at", failed))

	if err := startDetached(log, exe); err != nil {
		return fmt.Errorf("start restored exe: %w", err)
	}
	return nil
}

// updateWaitPidFlagUnix экспорт для main.go (общий между unix и windows).
// Реализован здесь потому что main.go общий, а unix файл не компилируется
// на Windows. Константа одна и та же — оба apply используют её.
var _ = updateWaitPidFlag

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
