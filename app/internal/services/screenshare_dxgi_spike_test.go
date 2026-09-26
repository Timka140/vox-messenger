//go:build windows

package services

// P0-спайк: DXGI Desktop Duplication (kirides/go-d3d, pure Go) против BitBlt.
// Требует живой desktop-сессии: VOX_SCREEN_BENCH=1 go test ./internal/services/ -run TestDXGISpike -v

import (
	"errors"
	"image"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/kirides/go-d3d/d3d11"
	"github.com/kirides/go-d3d/outputduplication"
	"github.com/kirides/go-d3d/win"
)

func TestDXGISpike_Capture(t *testing.T) {
	if os.Getenv("VOX_SCREEN_BENCH") != "1" {
		t.Skip("VOX_SCREEN_BENCH=1 — живой захват экрана")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if win.IsValidDpiAwarenessContext(win.DpiAwarenessContextPerMonitorAwareV2) {
		if _, err := win.SetThreadDpiAwarenessContext(win.DpiAwarenessContextPerMonitorAwareV2); err != nil {
			t.Logf("SetThreadDpiAwarenessContext: %v", err)
		}
	}
	device, devCtx, err := d3d11.NewD3D11Device()
	if err != nil {
		t.Fatalf("NewD3D11Device: %v", err)
	}
	defer device.Release()
	defer devCtx.Release()

	dup, err := outputduplication.NewIDXGIOutputDuplication(device, devCtx, 0)
	if err != nil {
		t.Fatalf("NewIDXGIOutputDuplication: %v", err)
	}
	defer dup.Release()
	dup.DrawPointer = true
	bounds, err := dup.GetBounds()
	if err != nil {
		t.Fatalf("GetBounds: %v", err)
	}
	img := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	t.Logf("display 0: %dx%d", bounds.Dx(), bounds.Dy())

	// 3 секунды: считаем кадры, таймауты (нет изменений на экране) и время
	// GetImage. DXGI отдаёт кадр только при изменении — двигай мышью во время
	// теста, иначе будут одни таймауты.
	deadline := time.Now().Add(3 * time.Second)
	var frames, timeouts, other int
	var total, maxD time.Duration
	for time.Now().Before(deadline) {
		st := time.Now()
		err := dup.GetImage(img, 100)
		d := time.Since(st)
		switch {
		case err == nil:
			frames++
			total += d
			if d > maxD {
				maxD = d
			}
		case errors.Is(err, outputduplication.ErrNoImageYet):
			timeouts++
		default:
			other++
			t.Logf("GetImage err: %v", err)
		}
	}
	avg := 0.0
	if frames > 0 {
		avg = float64(total.Microseconds()) / 1000 / float64(frames)
	}
	t.Logf("DXGI: frames=%d timeouts=%d errors=%d avg=%.1fms max=%.1fms (fps≈%.0f)",
		frames, timeouts, other, avg, float64(maxD.Microseconds())/1000, float64(frames)/3)
	if frames == 0 && other > 0 {
		t.Fatalf("DXGI не отдал ни одного кадра")
	}

	// Сравнение: текущий BitBlt-путь (kbinani) в нативном разрешении.
	cap, err := openScreenCapture(0, 0)
	if err != nil {
		t.Fatalf("openScreenCapture: %v", err)
	}
	defer cap.Close()
	var bbTotal time.Duration
	const bbN = 20
	for i := 0; i < bbN; i++ {
		st := time.Now()
		if _, err := cap.Capture(); err != nil {
			t.Fatalf("BitBlt capture: %v", err)
		}
		bbTotal += time.Since(st)
	}
	t.Logf("BitBlt: avg=%.1fms", float64(bbTotal.Microseconds())/1000/bbN)
}
