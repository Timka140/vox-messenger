//go:build windows

package services

import (
	"errors"
	"fmt"
	"image"
	"time"

	"github.com/kbinani/screenshot"
	d3d "github.com/kirides/go-d3d"
	"github.com/kirides/go-d3d/d3d11"
	"github.com/kirides/go-d3d/outputduplication"
	"github.com/kirides/go-d3d/win"
	xdraw "golang.org/x/image/draw"
)

// errDXGIUnavailable — на этой платформе DXGI нет (см. *_other.go).
var errDXGIUnavailable = errors.New("dxgi: unavailable on this platform")

// dxgiReopenInterval — не чаще одного пересоздания дублирования: при
// полноэкранной игре ACCESS_LOST идёт на каждый кадр.
const dxgiReopenInterval = time.Second

// dxgiMaxOutputs — предел перебора выходов адаптера.
const dxgiMaxOutputs = 16

// dxgiCapture — захват через DXGI Desktop Duplication (kirides/go-d3d, pure
// Go). Кадр приходит только при изменении экрана; при таймауте отдаём
// предыдущий кадр повторно, чтобы энкодер держал темп (P-кадр по одинаковой
// картинке почти пустой). Курсор рисуем сами поверх выходного кадра
// (drawCursorOverlayScaled), т.к. он двигается и без изменений экрана.
//
// Все вызовы должны идти из горутины, создавшей устройство (COM): captureLoop
// делает LockOSThread.
type dxgiCapture struct {
	device     *d3d11.ID3D11Device
	deviceCtx  *d3d11.ID3D11DeviceContext
	dup        *outputduplication.OutputDuplicator // nil — потерян, ждёт пересоздания
	bounds     image.Rectangle                     // экран в desktop-координатах (для курсора и поиска выхода)
	native     *image.RGBA                         // кадр нативного разрешения
	outW       int
	outH       int
	needScale  bool
	scaled     *image.RGBA // переиспользуемый буфер даунскейла
	haveFrame  bool
	lastReopen time.Time
}

// openDXGICapture — displayIdx в нумерации kbinani (список и превью в
// «Показать экран»). Выход DXGI ищется по границам этого монитора: номера
// выходов адаптера идут в другом порядке, и прямой индекс показывал чужой экран.
func openDXGICapture(displayIdx, maxWidth int) (screenCapture, error) {
	if win.IsValidDpiAwarenessContext(win.DpiAwarenessContextPerMonitorAwareV2) {
		// Без PerMonitorV2 DuplicateOutput1 отказывает на HiDPI-мониторах.
		_, _ = win.SetThreadDpiAwarenessContext(win.DpiAwarenessContextPerMonitorAwareV2)
	}
	if n := screenshot.NumActiveDisplays(); displayIdx < 0 || displayIdx >= n {
		displayIdx = 0
	}
	want := screenshot.GetDisplayBounds(displayIdx)
	if want.Dx() <= 0 || want.Dy() <= 0 {
		return nil, fmt.Errorf("dxgi: display %d: пустые границы", displayIdx)
	}
	device, deviceCtx, err := d3d11.NewD3D11Device()
	if err != nil {
		return nil, fmt.Errorf("dxgi: create d3d11 device: %w", err)
	}
	dup, err := findDXGIOutput(device, deviceCtx, want)
	if err != nil {
		deviceCtx.Release()
		device.Release()
		return nil, fmt.Errorf("dxgi: display %d %v: %w", displayIdx, want, err)
	}
	nw, nh := want.Dx(), want.Dy()
	outW, outH := nw, nh
	if maxWidth > 0 && outW > maxWidth {
		outW = maxWidth
		outH = nh * maxWidth / nw
	}
	outW &^= 1
	outH &^= 1
	c := &dxgiCapture{
		device:    device,
		deviceCtx: deviceCtx,
		dup:       dup,
		bounds:    want,
		native:    image.NewRGBA(image.Rect(0, 0, nw, nh)),
		outW:      outW,
		outH:      outH,
		needScale: outW != nw || outH != nh,
	}
	if c.needScale {
		c.scaled = image.NewRGBA(image.Rect(0, 0, outW, outH))
	}
	return c, nil
}

// findDXGIOutput — дублирование выхода адаптера с границами want. Остальные
// открытые на время поиска выходы освобождаются. Монитор на другой видеокарте
// здесь не найдётся — openBestScreenCapture уйдёт на BitBlt.
func findDXGIOutput(device *d3d11.ID3D11Device, deviceCtx *d3d11.ID3D11DeviceContext, want image.Rectangle) (*outputduplication.OutputDuplicator, error) {
	var dups []*outputduplication.OutputDuplicator
	var bounds []image.Rectangle
	var lastErr error
	for i := uint(0); i < dxgiMaxOutputs; i++ {
		dup, err := outputduplication.NewIDXGIOutputDuplication(device, deviceCtx, i)
		if err != nil {
			if errors.Is(err, d3d.DXGI_ERROR_NOT_FOUND) {
				break // выходы кончились
			}
			// Выход есть, но не дублируется (занят, защищённый контент).
			lastErr = err
			dups = append(dups, nil)
			bounds = append(bounds, image.Rectangle{})
			continue
		}
		b, err := dup.GetBounds()
		if err != nil {
			lastErr = err
			b = image.Rectangle{}
		}
		dups = append(dups, dup)
		bounds = append(bounds, b)
	}
	idx := matchOutput(bounds, want)
	for i, d := range dups {
		if i != idx && d != nil {
			d.Release()
		}
	}
	if idx < 0 {
		if lastErr != nil {
			return nil, fmt.Errorf("нет выхода с такими границами среди %v: %w", bounds, lastErr)
		}
		return nil, fmt.Errorf("нет выхода с такими границами среди %v", bounds)
	}
	// Курсор рисуем сами (drawCursorOverlayScaled, GetCursorInfo/DrawIconEx —
	// проверенный путь из BitBlt-режима): DrawPointer у go-d3d на практике
	// курсор не показывал (Тим, 2026-09-08), да и при статичном экране DXGI
	// кадр не отдаёт, а мышь двигается.
	dups[idx].DrawPointer = false
	return dups[idx], nil
}

func (c *dxgiCapture) Size() (int, int) { return c.outW, c.outH }

// reopen — пересоздать дублирование после ACCESS_LOST, не чаще
// dxgiReopenInterval. Прежний кадр (haveFrame) остаётся: после восстановления
// статичный экран снова отдаёт его, а не ошибку.
func (c *dxgiCapture) reopen() {
	if time.Since(c.lastReopen) < dxgiReopenInterval {
		return
	}
	c.lastReopen = time.Now()
	if c.dup != nil {
		c.dup.Release()
		c.dup = nil
	}
	if dup, err := findDXGIOutput(c.device, c.deviceCtx, c.bounds); err == nil {
		c.dup = dup
	}
}

func (c *dxgiCapture) Capture() (*screenFrame, error) {
	if c.dup == nil {
		c.reopen()
		if c.dup == nil {
			return nil, errDXGIAccessLost
		}
	}
	// 100 мс ожидания нового кадра: при статичном экране DXGI отдаёт
	// ErrNoImageYet — повторяем предыдущий кадр, темп не ломаем.
	err := c.dup.GetImage(c.native, 100)
	if err != nil {
		if errors.Is(err, d3d.DXGI_ERROR_ACCESS_LOST) {
			c.reopen()
			return nil, fmt.Errorf("%w: %v", errDXGIAccessLost, err)
		}
		if !errors.Is(err, outputduplication.ErrNoImageYet) {
			return nil, fmt.Errorf("dxgi: get image: %w", err)
		}
		if !c.haveFrame {
			return nil, fmt.Errorf("dxgi: кадр ещё не получен")
		}
	} else {
		c.haveFrame = true
	}
	if !c.needScale {
		// native переиспользуется — копия, т.к. encode может идти после
		// следующего GetImage (mailbox latest-frame-wins).
		out := image.NewRGBA(c.native.Rect)
		copy(out.Pix, c.native.Pix)
		drawCursorOverlayScaled(out, c.bounds, 1, 1)
		return &screenFrame{Width: c.outW, Height: c.outH, Img: out}, nil
	}
	xdraw.ApproxBiLinear.Scale(c.scaled, c.scaled.Bounds(), c.native, c.native.Bounds(), xdraw.Over, nil)
	out := image.NewRGBA(c.scaled.Rect)
	copy(out.Pix, c.scaled.Pix)
	drawCursorOverlayScaled(out, c.bounds, float64(c.outW)/float64(c.bounds.Dx()), float64(c.outH)/float64(c.bounds.Dy()))
	return &screenFrame{Width: c.outW, Height: c.outH, Img: out}, nil
}

func (c *dxgiCapture) Close() error {
	if c.dup != nil {
		c.dup.Release()
		c.dup = nil
	}
	if c.deviceCtx != nil {
		c.deviceCtx.Release()
		c.deviceCtx = nil
	}
	if c.device != nil {
		c.device.Release()
		c.device = nil
	}
	return nil
}
