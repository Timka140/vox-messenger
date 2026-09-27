package services

import (
	"fmt"
	"os"
)

// relaunchSelf запускает текущий exe заново (тем же путём, что и автообновление:
// --from-update --wait-pid — новый процесс дождётся выхода этого и поднимет окно).
// Сам процесс не завершает: вызывающий зовёт quit (main.go: app.Quit()).
func relaunchSelf() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("relaunch: executable: %w", err)
	}
	if _, err := startDetached(exe); err != nil {
		return fmt.Errorf("relaunch: start: %w", err)
	}
	return nil
}
