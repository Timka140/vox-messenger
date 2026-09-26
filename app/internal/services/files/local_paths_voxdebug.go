//go:build voxdebug

package files

import (
	"os"
	"path/filepath"
)

// VoxDownloadsDir в debug-режиме — отдельная папка с суффиксом basename(cwd).
// Это нужно когда на одном ПК одновременно запущены две копии приложения
// (через bin/ и bin2/ в .vscode/launch.json) — каждая пишет в свою папку,
// rename .partial→final не конфликтует.
//
// Включается через build-tag `voxdebug`. См. .vscode/launch.json — buildFlags
// содержит `-tags=voxdebug,...` для App / App 2.
func VoxDownloadsDir() string {
	cwd, _ := os.Getwd()
	suffix := filepath.Base(cwd)
	if suffix == "" || suffix == "." {
		suffix = "debug"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", "Vox Downloads ("+suffix+")")
	}
	return filepath.Join(home, "Downloads", "Vox Downloads ("+suffix+")")
}
