//go:build !windows

package services

import "image"

// drawCursorOverlay — stub для не-Windows платформ.
//
// На Linux: курсор рисуется отдельно через XFixes (XFixesGetCursorImage),
// требует отдельный XOpen + ext check; пока без него.
// На macOS: NSCursor.currentCursor + drawAtPoint, нужно cgo через AppKit.
// Оба случая откладываем до запросов от пользователей других ОС.
func drawCursorOverlay(_ *image.RGBA, _ image.Rectangle) {}

func drawCursorOverlayScaled(_ *image.RGBA, _ image.Rectangle, _, _ float64) {}
