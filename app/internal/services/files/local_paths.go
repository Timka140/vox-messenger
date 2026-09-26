package files

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// VoxDownloadsDir определена в local_paths_release.go (release-сборка) и в
// local_paths_voxdebug.go (debug-сборка с build-tag `voxdebug`). В debug-режиме
// каждая копия приложения (bin/, bin2/) пишет в свою отдельную папку
// "Vox Downloads (bin)" / "Vox Downloads (bin2)" — иначе при одновременном
// запуске двух копий конфликты при rename .partial → final.

func ensureVoxDownloadsDir() (string, error) {
	dir := VoxDownloadsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("ensureVoxDownloadsDir: %w", err)
	}
	return dir, nil
}

// uniqueDownloadPath возвращает свободный путь в Vox Downloads. Если name
// занят — добавляем "_1", "_2" перед расширением (Windows-style).
//
// Пример: "Obsidian.exe" → если занят, "Obsidian_1.exe" → "Obsidian_2.exe" → ...
func uniqueDownloadPath(name string) (string, error) {
	dir, err := ensureVoxDownloadsDir()
	if err != nil {
		return "", err
	}
	base := sanitizeDownloadName(name)
	if base == "" {
		base = "file"
	}
	candidate := filepath.Join(dir, base)
	if _, err := os.Stat(candidate); os.IsNotExist(err) {
		return candidate, nil
	}
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 1; i < 10000; i++ {
		candidate = filepath.Join(dir, fmt.Sprintf("%s_%d%s", stem, i, ext))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("uniqueDownloadPath: too many collisions for %s", name)
}

// sanitizeDownloadName — убирает path separators и control chars из имени,
// которое пришло от sender'а (потенциально враждебное). Прячет попытку
// "../etc/passwd" в одно "passwd".
func sanitizeDownloadName(name string) string {
	if name == "" {
		return ""
	}
	name = filepath.Base(name) // защита от path traversal
	var b strings.Builder
	for _, r := range name {
		if r < 32 || r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' {
			b.WriteRune('_')
		} else {
			b.WriteRune(r)
		}
	}
	res := b.String()
	if res == "." || res == ".." {
		return ""
	}
	return res
}

// FileExists — true если файл реально на диске. Используется для lazy
// integrity check (юзер мог удалить через Проводник).
func FileExists(path string) bool {
	if path == "" {
		return false
	}
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// partialSubdirName — папка для in-progress загрузок внутри Vox Downloads.
// Префикс "." на macOS/Linux делает её скрытой, на Windows — нет, но чёткое
// имя ".vox-tmp" даёт юзеру понять что это служебная папка приложения.
const partialSubdirName = ".vox-tmp"

// partialDownloadPath строит путь .vox-partial рядом с финальным, в подпапке
// .vox-tmp/. Это убирает мусорные partial-файлы из главного списка Vox
// Downloads пока загрузка идёт. На complete делаем os.Rename из subdir в
// финальный путь — те же файловая система, атомарно.
//
// Имя partial-файла = basename финального + ".partial" (без префикса дотов
// чтобы не плодить точечные имена внутри уже точечной папки).
func partialDownloadPath(finalPath string) (string, error) {
	dir := filepath.Dir(finalPath)
	tmpDir := filepath.Join(dir, partialSubdirName)
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return "", fmt.Errorf("partialDownloadPath: mkdir tmp: %w", err)
	}
	return filepath.Join(tmpDir, filepath.Base(finalPath)+".partial"), nil
}
