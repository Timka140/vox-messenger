//go:build windows

package services

import (
	"image"
	"runtime"
	"syscall"
	"unsafe"
)

// drawCursorOverlay рисует системный курсор поверх захваченного RGBA-кадра
// (на месте, in-place). BitBlt-захват экрана на Windows принципиально НЕ
// включает курсор — Windows рендерит его hardware-overlay'ем поверх DWM.
// Поэтому мы запрашиваем GetCursorInfo + GetIconInfo для получения mask/color
// bitmap'ов и рисуем мини-битмапу класса 32×32 поверх RGBA вручную.
//
// Без cgo — все вызовы через syscall.LazyDLL/LazyProc. user32 + gdi32 уже
// загружены любым GUI-процессом, sys cost минимален (<10 µs на проход).
//
// Если курсор скрыт (CURSOR_SHOWING flag не выставлен) — функция no-op.
// В случае ошибки любого WinAPI-шага молча возвращаемся без overlay'а
// (degraded experience лучше чем краш в audio/voice-критичном пути).
//
// rect — координаты захваченного экрана в global desktop coords (важно для
// multi-monitor: курсор отдаётся в global, мы переводим в локальные).
//
// Thread affinity: GDI DC (Device Context) привязан к OS-потоку, в котором
// был создан. Goroutines свободно мигрируют между потоками. Если scheduler
// переключил нас между CreateCompatibleDC и DrawIconEx — DC оказывается на
// чужом потоке, дальнейшие GDI вызовы повреждают process state (логи 22:29:
// access violation в чужой goroutine 1666 / FFT). LockOSThread+UnlockOSThread
// гарантирует что вся цепочка GDI выполнится на одном thread'е. Любую
// нестандартную ошибку GDI ловим recover'ом, чтобы один битый кадр не
// убивал весь процесс.
func drawCursorOverlay(rgba *image.RGBA, rect image.Rectangle) {
	drawCursorOverlayScaled(rgba, rect, 1, 1)
}

// drawCursorOverlayScaled — то же, но для кадра, отмасштабированного
// относительно rect в sx/sy раз (DXGI-захват в 720p/1080p-режимах): позиция
// курсора масштабируется, сама иконка рисуется в нативном размере — на
// уменьшенном кадре она чуть крупнее, зато видна.
func drawCursorOverlayScaled(rgba *image.RGBA, rect image.Rectangle, sx, sy float64) {
	if rgba == nil {
		return
	}
	if sx <= 0 || sy <= 0 {
		sx, sy = 1, 1
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer func() {
		if r := recover(); r != nil {
			_ = r // умышленно проглатываем — кадр без курсора лучше чем краш
		}
	}()
	var ci cursorInfo
	ci.cbSize = uint32(unsafe.Sizeof(ci))
	if r1, _, _ := procGetCursorInfo.Call(uintptr(unsafe.Pointer(&ci))); r1 == 0 {
		return
	}
	if ci.flags&cursorShowing == 0 || ci.hCursor == 0 {
		return
	}

	var ii iconInfo
	if r1, _, _ := procGetIconInfo.Call(uintptr(ci.hCursor), uintptr(unsafe.Pointer(&ii))); r1 == 0 {
		return
	}
	defer func() {
		if ii.hbmMask != 0 {
			procDeleteObject.Call(uintptr(ii.hbmMask))
		}
		if ii.hbmColor != 0 {
			procDeleteObject.Call(uintptr(ii.hbmColor))
		}
	}()

	// Получаем размер курсора через GetObject(BITMAP) — стандартный курсор
	// 32×32, но в Windows 11 на high-DPI бывает 64×64.
	var bm bitmapStruct
	srcBitmap := ii.hbmColor
	if srcBitmap == 0 {
		// Чисто-mask курсор (черно-белая icon без alpha) — берём mask, его
		// высота вдвое больше реальной (XOR + AND части склеены).
		srcBitmap = ii.hbmMask
	}
	if r1, _, _ := procGetObject.Call(
		uintptr(srcBitmap),
		uintptr(unsafe.Sizeof(bm)),
		uintptr(unsafe.Pointer(&bm)),
	); r1 == 0 {
		return
	}
	cw := int(bm.bmWidth)
	ch := int(bm.bmHeight)
	if ii.hbmColor == 0 {
		ch /= 2 // mask содержит AND+XOR одна над другой
	}
	if cw <= 0 || ch <= 0 || cw > 256 || ch > 256 {
		return
	}

	// Координаты курсора → локальные пиксели в rect.
	cx := int(float64(int(ci.ptScreenPos.X)-rect.Min.X)*sx) - int(ii.xHotspot)
	cy := int(float64(int(ci.ptScreenPos.Y)-rect.Min.Y)*sy) - int(ii.yHotspot)

	// Создаём временный 32-bit DIB в памяти, копируем туда курсор через
	// DrawIconEx — самый надёжный способ корректно отрендерить mask + color
	// + alpha-channel курсора (включая anti-aliased современные курсоры).
	hdcScreen, _, _ := procGetDC.Call(0)
	if hdcScreen == 0 {
		return
	}
	defer procReleaseDC.Call(0, hdcScreen)
	hdcMem, _, _ := procCreateCompatibleDC.Call(hdcScreen)
	if hdcMem == 0 {
		return
	}
	defer procDeleteDC.Call(hdcMem)

	bi := bitmapInfo{
		bmiHeader: bitmapInfoHeader{
			biSize:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
			biWidth:       int32(cw),
			biHeight:      -int32(ch), // top-down DIB
			biPlanes:      1,
			biBitCount:    32,
			biCompression: 0, // BI_RGB
		},
	}
	var dibBits unsafe.Pointer
	hbmDIB, _, _ := procCreateDIBSection.Call(
		hdcMem,
		uintptr(unsafe.Pointer(&bi)),
		0, // DIB_RGB_COLORS
		uintptr(unsafe.Pointer(&dibBits)),
		0, 0,
	)
	if hbmDIB == 0 {
		return
	}
	defer procDeleteObject.Call(hbmDIB)

	hbmOld, _, _ := procSelectObject.Call(hdcMem, hbmDIB)

	// DrawIconEx рисует курсор в hdcMem, alpha-blended на наш DIB.
	procDrawIconEx.Call(
		hdcMem, 0, 0, uintptr(ci.hCursor),
		uintptr(cw), uintptr(ch), 0, 0,
		drawIconNormal,
	)

	procSelectObject.Call(hdcMem, hbmOld)

	// dibBits содержит BGRA top-down. Композитим на rgba.Pix (RGBA, top-down).
	// Pre-multiplied alpha: курсор уже идёт с alpha-каналом из DrawIconEx.
	// Простой "src over" для каждого пикселя курсора.
	dibLen := cw * ch * 4
	dibSlice := unsafe.Slice((*byte)(dibBits), dibLen)

	for y := range ch {
		dy := cy + y
		if dy < 0 || dy >= rgba.Rect.Dy() {
			continue
		}
		for x := range cw {
			dx := cx + x
			if dx < 0 || dx >= rgba.Rect.Dx() {
				continue
			}
			si := (y*cw + x) * 4
			b := dibSlice[si]
			g := dibSlice[si+1]
			r := dibSlice[si+2]
			a := dibSlice[si+3]
			if a == 0 {
				continue
			}
			di := dy*rgba.Stride + dx*4
			if a == 255 {
				rgba.Pix[di] = r
				rgba.Pix[di+1] = g
				rgba.Pix[di+2] = b
				rgba.Pix[di+3] = 255
			} else {
				inv := uint32(255 - a)
				rgba.Pix[di] = byte((uint32(r)*255 + uint32(rgba.Pix[di])*inv) / 255)
				rgba.Pix[di+1] = byte((uint32(g)*255 + uint32(rgba.Pix[di+1])*inv) / 255)
				rgba.Pix[di+2] = byte((uint32(b)*255 + uint32(rgba.Pix[di+2])*inv) / 255)
				rgba.Pix[di+3] = 255
			}
		}
	}
}

// --- Win32 bindings (без cgo) ---

const (
	cursorShowing  = 0x00000001
	drawIconNormal = 0x00000003 // DI_NORMAL = DI_IMAGE | DI_MASK
)

type pointStruct struct {
	X, Y int32
}

type cursorInfo struct {
	cbSize      uint32
	flags       uint32
	hCursor     syscall.Handle
	ptScreenPos pointStruct
}

type iconInfo struct {
	fIcon    int32
	xHotspot uint32
	yHotspot uint32
	hbmMask  syscall.Handle
	hbmColor syscall.Handle
}

type bitmapStruct struct {
	bmType       int32
	bmWidth      int32
	bmHeight     int32
	bmWidthBytes int32
	bmPlanes     uint16
	bmBitsPixel  uint16
	bmBits       uintptr
}

type bitmapInfoHeader struct {
	biSize          uint32
	biWidth         int32
	biHeight        int32
	biPlanes        uint16
	biBitCount      uint16
	biCompression   uint32
	biSizeImage     uint32
	biXPelsPerMeter int32
	biYPelsPerMeter int32
	biClrUsed       uint32
	biClrImportant  uint32
}

type bitmapInfo struct {
	bmiHeader bitmapInfoHeader
	bmiColors [1]uint32
}

var (
	user32 = syscall.NewLazyDLL("user32.dll")
	gdi32  = syscall.NewLazyDLL("gdi32.dll")

	procGetCursorInfo      = user32.NewProc("GetCursorInfo")
	procGetIconInfo        = user32.NewProc("GetIconInfo")
	procDrawIconEx         = user32.NewProc("DrawIconEx")
	procGetDC              = user32.NewProc("GetDC")
	procReleaseDC          = user32.NewProc("ReleaseDC")
	procCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	procDeleteDC           = gdi32.NewProc("DeleteDC")
	procCreateDIBSection   = gdi32.NewProc("CreateDIBSection")
	procSelectObject       = gdi32.NewProc("SelectObject")
	procDeleteObject       = gdi32.NewProc("DeleteObject")
	procGetObject          = gdi32.NewProc("GetObjectW")
)
