package services

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"time"

	"github.com/kbinani/screenshot"
	xdraw "golang.org/x/image/draw"
)

// screenFrame — захваченный кадр в RGBA. image/jpeg ест RGBA нативно
// (без temporary копии при Encode), поэтому держим именно его.
type screenFrame struct {
	Width  int
	Height int
	Img    *image.RGBA
}

// screenCapture — открытый источник захвата экрана.
type screenCapture interface {
	Capture() (*screenFrame, error)
	// Size — размер выходного кадра (после даунскейла); энкодер создаётся
	// под него один раз.
	Size() (w, h int)
	Close() error
}

// openBestScreenCapture — DXGI Desktop Duplication, где доступен (Windows,
// ~6 мс/кадр на 2560×1440 против ~30 мс у BitBlt, замер 2026-09-08), иначе
// BitBlt/kbinani. Возвращает имя backend'а для логов.
func openBestScreenCapture(displayIdx, maxWidth int) (screenCapture, string, error) {
	if cap, err := openDXGICapture(displayIdx, maxWidth); err == nil {
		return cap, "dxgi", nil
	} else if err != errDXGIUnavailable {
		// DXGI есть на платформе, но не завёлся (RDP-сессия, старая видеокарта,
		// защищённый контент) — падаем на BitBlt, но скажем почему.
		fallbackLog(err)
	}
	cap, err := openScreenCapture(displayIdx, maxWidth)
	return cap, "bitblt", err
}

// fallbackLog — hook для лога причины отказа DXGI (ставится сервисом).
var fallbackLog = func(error) {}

// errDXGIAccessLost — DXGI потерял доступ к выходу (полноэкранная игра, смена
// режима, защищённый рабочий стол). dxgiCapture пересоздаёт дублирование сам;
// если это не помогает screenDXGIGiveUp подряд — captureLoop уходит на BitBlt.
var errDXGIAccessLost = errors.New("dxgi: access lost")

// screenDXGIGiveUp — сколько терпеть потерю доступа DXGI без единого кадра.
const screenDXGIGiveUp = 2 * time.Second

// shouldFallbackDXGI — пора ли переключить захват с DXGI на BitBlt.
// firstLost — время первой потери доступа после последнего удачного кадра.
func shouldFallbackDXGI(firstLost, now time.Time, backend string) bool {
	return backend == "dxgi" && !firstLost.IsZero() && now.Sub(firstLost) >= screenDXGIGiveUp
}

// matchOutput — индекс выхода DXGI с теми же физическими границами, что у
// монитора из kbinani (список и превью в «Показать экран»), или −1. Номера
// выходов DXGI и порядок EnumDisplayMonitors не совпадают — только границы.
// Похожий, но не тот монитор не подставляем: лучше BitBlt, чем чужой экран.
func matchOutput(bounds []image.Rectangle, want image.Rectangle) int {
	for i, b := range bounds {
		if b == want && !b.Empty() {
			return i
		}
	}
	return -1
}

// listScreenDisplays возвращает список доступных экранов (для UI выбора).
func listScreenDisplays() []DisplayInfo {
	n := screenshot.NumActiveDisplays()
	out := make([]DisplayInfo, 0, n)
	for i := range n {
		b := screenshot.GetDisplayBounds(i)
		out = append(out, DisplayInfo{
			Index:  i,
			Name:   fmt.Sprintf("Display %d (%d×%d)", i+1, b.Dx(), b.Dy()),
			Width:  b.Dx(),
			Height: b.Dy(),
		})
	}
	return out
}

// openScreenCapture открывает захват указанного экрана. displayIdx ∈ [0, NumActiveDisplays).
// maxWidth — даунскейл если нативная ширина больше (сохраняет aspect ratio).
//
// Backend: github.com/kbinani/screenshot — Win32 BitBlt на Windows,
// XShm/XComposite на Linux (X11), CoreGraphics на macOS. Без cgo сверху того
// что у нас уже есть (ONNX, RNNoise, sqlcipher). На Windows BitBlt 1080p →
// ~10-20 ms на кадр; для 15 fps в JPEG-режиме хватает.
//
// TODO для скорости/качества: DXGI Desktop Duplication на Windows и
// PipeWire на Linux/Wayland — оба через cgo, дают 60 fps + cursor compositing.
// Делать когда упрёмся в overhead BitBlt'а.
func openScreenCapture(displayIdx, maxWidth int) (screenCapture, error) {
	n := screenshot.NumActiveDisplays()
	if n == 0 {
		return nil, fmt.Errorf("нет доступных экранов")
	}
	if displayIdx < 0 || displayIdx >= n {
		displayIdx = 0
	}
	bounds := screenshot.GetDisplayBounds(displayIdx)
	if bounds.Dx() <= 0 || bounds.Dy() <= 0 {
		return nil, fmt.Errorf("display %d: пустые границы", displayIdx)
	}

	outW, outH := bounds.Dx(), bounds.Dy()
	if maxWidth > 0 && outW > maxWidth {
		outW = maxWidth
		outH = bounds.Dy() * maxWidth / bounds.Dx()
	}
	// Чётные размеры всегда, как у DXGI: H264 не любит нечётные, а при
	// переходе DXGI → BitBlt посреди показа размер кадра должен совпасть.
	outW &^= 1
	outH &^= 1

	return &kbinaniCapture{
		displayIdx: displayIdx,
		bounds:     bounds,
		outW:       outW,
		outH:       outH,
		needScale:  outW != bounds.Dx() || outH != bounds.Dy(),
		scaleBuf:   image.NewRGBA(image.Rect(0, 0, outW, outH)),
	}, nil
}

// kbinaniCapture — обёртка над screenshot.CaptureRect с опциональным даунскейлом.
type kbinaniCapture struct {
	displayIdx int
	bounds     image.Rectangle
	outW, outH int
	needScale  bool
	scaleBuf   *image.RGBA // переиспользуемый downscale-буфер
}

func (c *kbinaniCapture) Capture() (*screenFrame, error) {
	raw, err := screenshot.CaptureRect(c.bounds)
	if err != nil {
		return nil, fmt.Errorf("screenshot capture: %w", err)
	}
	// Курсор: BitBlt принципиально не захватывает hardware-cursor на Windows.
	// drawCursorOverlay — platform-specific (см. screenshare_cursor_*.go);
	// рисует поверх raw на месте, до даунскейла, чтобы и сам курсор тоже
	// корректно отскейлился, если он есть.
	drawCursorOverlay(raw, c.bounds)

	if !c.needScale {
		return &screenFrame{Width: c.outW, Height: c.outH, Img: raw}, nil
	}
	// ApproxBiLinear — компромисс качества и скорости. CatmullRom выглядит
	// чуть резче, но даёт +50% CPU на ресайз, что заметно на 15 fps на слабых
	// машинах. На 1080p→720p (типичный downscale) разница в качестве
	// неуловима после JPEG quality=60.
	xdraw.ApproxBiLinear.Scale(c.scaleBuf, c.scaleBuf.Bounds(), raw, raw.Bounds(), xdraw.Over, nil)
	// Возвращаем копию scaleBuf'а — мы его переиспользуем на следующем кадре,
	// но encodeJPEG может кодировать в фоне. Без копии — race на пиксельных
	// данных. Копия RGBA 1280×720 ≈ 3.5 MiB, ~1 ms memcpy.
	out := image.NewRGBA(c.scaleBuf.Rect)
	copy(out.Pix, c.scaleBuf.Pix)
	return &screenFrame{Width: c.outW, Height: c.outH, Img: out}, nil
}

func (c *kbinaniCapture) Size() (int, int) { return c.outW, c.outH }

func (c *kbinaniCapture) Close() error { return nil }

// encodeJPEG — image.RGBA в JPEG с заданным quality (1..100). Стандартный
// Go encoder, без cgo. На 720p RGBA → ~30-80 KiB JPEG (quality=60), типичное
// время ~3-5 ms на современном CPU.
func encodeJPEG(frame *screenFrame, quality int) ([]byte, error) {
	if frame == nil || frame.Img == nil {
		return nil, fmt.Errorf("screen frame nil")
	}
	if quality < 1 {
		quality = 1
	} else if quality > 100 {
		quality = 100
	}
	var buf bytes.Buffer
	buf.Grow(64 * 1024)
	if err := jpeg.Encode(&buf, frame.Img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, fmt.Errorf("jpeg encode: %w", err)
	}
	return buf.Bytes(), nil
}
