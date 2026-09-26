package services

import (
	"fmt"
	"image/png"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

// CaptureScreenshot — «Снимок экрана» из скрепки (фаза 7e): один кадр экрана
// displayIdx в PNG во временный файл. Путь уходит в обычный path-flow
// вложений, как у вставленной картинки: тот же каталог vox-paste и та же
// чистка суточных файлов (SavePastedImage).
//
// BitBlt, не DXGI: Desktop Duplication отдаёт кадр только при изменении
// экрана — на статичном снимка не дождаться (см. screenshare_thumbs.go).
func (s *ScreenShareService) CaptureScreenshot(displayIdx int) (string, error) {
	const op = "ScreenShareService.CaptureScreenshot"
	c, err := openScreenCapture(displayIdx, 0)
	if err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}
	frame, err := c.Capture()
	_ = c.Close()
	if err != nil {
		return "", fmt.Errorf("%s: capture: %w", op, err)
	}

	dir := filepath.Join(os.TempDir(), "vox-paste")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("%s: mkdir: %w", op, err)
	}
	cleanupPasteDir(dir)
	path := filepath.Join(dir, "screenshot-"+time.Now().Format("20060102-150405")+"-"+uuid.New().String()[:8]+".png")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("%s: create: %w", op, err)
	}
	// BestSpeed: 4K-кадр со сжатием по умолчанию кодируется заметно дольше.
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(f, frame.Img); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("%s: encode: %w", op, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("%s: close: %w", op, err)
	}
	s.log.Info("screenshot captured",
		slog.Int("display", displayIdx),
		slog.Int("w", frame.Width),
		slog.Int("h", frame.Height))
	return path, nil
}
