package services

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// pasteMaxBytes — больше из буфера обмена не принимаем (скриншот 4K PNG ~10 МБ).
const pasteMaxBytes = 50 * 1024 * 1024

// pasteTTL — вставленные картинки старше суток удаляются при следующей вставке.
const pasteTTL = 24 * time.Hour

// SavePastedImage — картинка из буфера обмена (Ctrl+V в строке ввода) во
// временный файл; путь уходит в обычный path-flow вложений
// (PrepareAttachments → SendFilesByPath). dataBase64 — сырой base64 или data: URL.
func (s *MessagingService) SavePastedImage(dataBase64, mimeType string) (string, error) {
	const op = "MessagingService.SavePastedImage"
	if strings.HasPrefix(dataBase64, "data:") {
		if i := strings.Index(dataBase64, ","); i > 0 {
			dataBase64 = dataBase64[i+1:]
		}
	}
	raw, err := base64.StdEncoding.DecodeString(dataBase64)
	if err != nil {
		return "", fmt.Errorf("%s: base64: %w", op, err)
	}
	if len(raw) == 0 {
		return "", fmt.Errorf("%s: пустая картинка", op)
	}
	if len(raw) > pasteMaxBytes {
		return "", fmt.Errorf("%s: картинка больше %d МБ", op, pasteMaxBytes/1024/1024)
	}
	ext := ".png"
	switch strings.ToLower(mimeType) {
	case "image/jpeg", "image/jpg":
		ext = ".jpg"
	case "image/gif":
		ext = ".gif"
	case "image/webp":
		ext = ".webp"
	case "image/bmp":
		ext = ".bmp"
	}
	dir := filepath.Join(os.TempDir(), "vox-paste")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("%s: mkdir: %w", op, err)
	}
	cleanupPasteDir(dir)
	name := "screenshot-" + time.Now().Format("20060102-150405") + "-" + uuid.New().String()[:8] + ext
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", fmt.Errorf("%s: write: %w", op, err)
	}
	return path, nil
}

func cleanupPasteDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-pasteTTL)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}
