//go:build windows

package services

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"syscall"
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
func startDetached(exe string) (int, error) {
	parentPid := strconv.Itoa(os.Getpid())
	// Кавычки вокруг "" — это title параметр для start (обязательный когда есть кавычки в exe пути)
	cmd := exec.Command("cmd.exe", "/C", "start", "", "/B", exe, updateRelaunchFlag, updateWaitPidFlag, parentPid)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := 0
	if cmd.Process != nil {
		pid = cmd.Process.Pid
	}
	_ = cmd.Wait() // start сразу возвращается — Wait очищает zombie cmd.exe
	return pid, nil
}

// allowSetForegroundWindow разрешает целевому процессу (PID) забрать foreground
// в обход стандартных Windows-ограничений. Без этого новый exe после
// автообновления откроется на заднем плане под другими окнами.
func allowSetForegroundWindow(pid int) error {
	user32, err := syscall.LoadDLL("user32.dll")
	if err != nil {
		return err
	}
	proc, err := user32.FindProc("AllowSetForegroundWindow")
	if err != nil {
		return err
	}
	r1, _, callErr := proc.Call(uintptr(uint32(pid)))
	if r1 == 0 {
		return callErr
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
	pid, err := startDetached(exe)
	if err != nil {
		_ = os.Remove(exe)
		_ = os.Rename(backup, exe)
		return fmt.Errorf("start new exe: %w", err)
	}
	log.Info("update: new process started", slog.Int("intermediary_pid", pid))
	// Разрешаем foreground для интермедиара cmd.exe (он передаст право child через CreateProcess)
	if pid > 0 {
		if err := allowSetForegroundWindow(pid); err != nil {
			log.Debug("AllowSetForegroundWindow failed (non-fatal)", slog.String("error", err.Error()))
		}
	}
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

	pid, err := startDetached(exe)
	if err != nil {
		return fmt.Errorf("start restored exe: %w", err)
	}
	if pid > 0 {
		if err := allowSetForegroundWindow(pid); err != nil {
			log.Debug("AllowSetForegroundWindow failed (non-fatal)", slog.String("error", err.Error()))
		}
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
