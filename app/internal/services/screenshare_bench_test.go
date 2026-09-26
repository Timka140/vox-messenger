package services

import (
	"os"
	"testing"
	"time"
)

// TestScreenCaptureBench — локальный бенч реального pipeline захвата+кодирования
// на этой машине. НЕ запускается в обычном go test (нужен VOX_SCREEN_BENCH=1 и
// активная desktop-сессия). Даёт реальные цифры: capture latency, размер JPEG по
// quality, и оценку Mbps на разных fps — чтобы откалибровать adaptive-диапазоны.
//
//	VOX_SCREEN_BENCH=1 go test ./internal/services/ -run TestScreenCaptureBench -v -count=1
func TestScreenCaptureBench(t *testing.T) {
	if os.Getenv("VOX_SCREEN_BENCH") == "" {
		t.Skip("set VOX_SCREEN_BENCH=1 to run the live screen-capture benchmark")
	}

	const maxWidth = 1280
	cap, err := openScreenCapture(0, maxWidth)
	if err != nil {
		t.Fatalf("openScreenCapture: %v", err)
	}
	defer cap.Close()

	// Прогрев + замер capture latency (capture+resize+cursor).
	const warm = 3
	for range warm {
		if _, err := cap.Capture(); err != nil {
			t.Fatalf("warmup capture: %v", err)
		}
	}

	const N = 20
	var capTotal time.Duration
	var lastFrame *screenFrame
	for i := range N {
		start := time.Now()
		f, err := cap.Capture()
		if err != nil {
			t.Fatalf("capture %d: %v", i, err)
		}
		capTotal += time.Since(start)
		lastFrame = f
	}
	avgCap := capTotal / N
	t.Logf("capture: %dx%d, avg capture+resize+cursor = %v over %d frames",
		lastFrame.Width, lastFrame.Height, avgCap, N)

	// Размер JPEG и время encode по quality на реальном последнем кадре.
	qualities := []int{20, 25, 30, 35, 40, 45, 50, 55}
	t.Logf("%-8s %-12s %-10s %-22s", "quality", "jpeg_bytes", "enc_ms", "Mbps @ 5/10/12 fps")
	for _, q := range qualities {
		// усредняем encode по нескольким проходам (JPEG детерминирован по размеру,
		// но время скачет)
		var encTotal time.Duration
		var size int
		const enc = 5
		for range enc {
			s := time.Now()
			payload, err := encodeJPEG(lastFrame, q)
			if err != nil {
				t.Fatalf("encodeJPEG q=%d: %v", q, err)
			}
			encTotal += time.Since(s)
			size = len(payload)
		}
		encMs := float64(encTotal/enc) / float64(time.Millisecond)
		mbps := func(fps int) float64 { return float64(size) * 8 * float64(fps) / 1e6 }
		t.Logf("%-8d %-12d %-10.1f %.2f / %.2f / %.2f",
			q, size, encMs, mbps(5), mbps(10), mbps(12))
	}

	// Реалистичный потолок fps = 1 / (avgCap + encode@40).
	s := time.Now()
	_, _ = encodeJPEG(lastFrame, 40)
	enc40 := time.Since(s)
	frameBudget := avgCap + enc40
	t.Logf("single-thread ceiling ≈ %.1f fps (cap %v + enc@40 %v = %v/frame)",
		float64(time.Second)/float64(frameBudget), avgCap, enc40, frameBudget)
}
