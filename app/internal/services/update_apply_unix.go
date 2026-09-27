//go:build !windows

package services

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
)

const updateRelaunchFlagUnix = "--from-update"
const updateWaitPidFlagUnix = "--wait-pid"

// applyUpdatePlatform выполняет apply для Linux/macOS:
// 1. Rename текущего exe → exe.old-<prev> (inode-based, работает с запущенным процессом)
// 2. Copy pending → exe + chmod +x
// 3. Start нового exe, detach
func applyUpdatePlatform(log *slog.Logger, exe, pending, prevVersion string) error {
	backup := exe + ".old-" + prevVersion
	_ = os.Remove(backup)

	if err := os.Rename(exe, backup); err != nil {
		return fmt.Errorf("rename exe → backup: %w", err)
	}
	log.Info("update: renamed current exe to backup",
		slog.String("backup", backup))

	if err := copyFile(pending, exe); err != nil {
		if rerr := os.Rename(backup, exe); rerr != nil {
			return fmt.Errorf("copy failed: %v; rollback also failed: %v", err, rerr)
		}
		return fmt.Errorf("copy pending → exe: %w", err)
	}
	if err := os.Chmod(exe, 0755); err != nil {
		log.Warn("update: chmod failed", slog.String("error", err.Error()))
	}

	log.Info("update: new binary in place, starting new process",
		slog.String("exe", exe))

	if err := startDetached(log, exe); err != nil {
		_ = os.Remove(exe)
		_ = os.Rename(backup, exe)
		return fmt.Errorf("start new exe: %w", err)
	}
	return nil
}

// rollbackUpdatePlatform — откат на предыдущую версию (Unix).
func rollbackUpdatePlatform(log *slog.Logger, exe, backup, currentVersion string) error {
	failed := exe + ".failed-" + currentVersion
	_ = os.Remove(failed)

	if err := os.Rename(exe, failed); err != nil {
		return fmt.Errorf("rename exe → failed: %w", err)
	}
	if err := os.Rename(backup, exe); err != nil {
		_ = os.Rename(failed, exe)
		return fmt.Errorf("rename backup → exe: %w", err)
	}
	if err := os.Chmod(exe, 0755); err != nil {
		log.Warn("rollback: chmod failed", slog.String("error", err.Error()))
	}

	log.Info("rollback: restored previous binary",
		slog.String("exe", exe),
		slog.String("failed_kept_at", failed))

	if err := startDetached(log, exe); err != nil {
		return fmt.Errorf("start restored exe: %w", err)
	}
	return nil
}

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

// startDetached запускает exe заново с --from-update --wait-pid <pid> (Unix).
func startDetached(_ *slog.Logger, exe string) error {
	cmd := exec.Command(exe, updateRelaunchFlagUnix, updateWaitPidFlagUnix, strconv.Itoa(os.Getpid()))
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	return nil
}
