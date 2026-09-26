//go:build !voxdebug

package files

import (
	"os"
	"path/filepath"
)

// VoxDownloadsDir — релизная папка ~/Downloads/Vox Downloads.
// Используется во всех production-сборках. Для одновременной работы двух
// debug-копий см. local_paths_voxdebug.go (с build-tag `voxdebug`).
func VoxDownloadsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", "Vox Downloads")
	}
	return filepath.Join(home, "Downloads", "Vox Downloads")
}
