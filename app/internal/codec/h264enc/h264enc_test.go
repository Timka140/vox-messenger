//go:build (windows && amd64) || (linux && amd64)

package h264enc

import (
	"image"
	"image/color"
	"os"
	"testing"
	"time"
)

// synthFrame — «экранный» контент: сетка, текст-подобные полосы и движущийся
// прямоугольник (иначе P-кадры почти пустые).
func synthFrame(w, h, seq int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := uint8((x/8 + y/8) % 2 * 200)
			if (y/3)%7 == 0 && (x/5)%3 == 0 {
				v = 30
			}
			img.SetRGBA(x, y, color.RGBA{v, v ^ 0x40, uint8(x), 255})
		}
	}
	bx := (seq * 17) % (w - 200)
	by := (seq * 11) % (h - 120)
	for y := by; y < by+120; y++ {
		for x := bx; x < bx+200; x++ {
			img.SetRGBA(x, y, color.RGBA{255, 80, 40, 255})
		}
	}
	return img
}

func has(types []int, want int) bool {
	for _, t := range types {
		if t == want {
			return true
		}
	}
	return false
}

func TestEncodeKeyframeAndDelta(t *testing.T) {
	const w, h = 1280, 720
	enc, err := New(Config{Width: w, Height: h, FPS: 30, Bitrate: 2_500_000})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer enc.Close()
	yuv := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)

	RGBAToI420(synthFrame(w, h, 0), yuv)
	out, key, err := enc.Encode(yuv, false)
	if err != nil {
		t.Fatalf("encode #0: %v", err)
	}
	types := NALTypes(out)
	if !key || !has(types, 7) || !has(types, 8) || !has(types, 5) {
		t.Fatalf("первый кадр должен быть keyframe с SPS/PPS/IDR: key=%v types=%v", key, types)
	}
	if dump := os.Getenv("VOX_H264_DUMP"); dump != "" {
		_ = os.WriteFile(dump, out, 0o644) // для проверки WebCodecs через CDP
	}

	RGBAToI420(synthFrame(w, h, 1), yuv)
	out, key, err = enc.Encode(yuv, false)
	if err != nil {
		t.Fatalf("encode #1: %v", err)
	}
	if key || has(NALTypes(out), 5) {
		t.Fatalf("второй кадр неожиданно keyframe")
	}

	RGBAToI420(synthFrame(w, h, 2), yuv)
	out, key, err = enc.Encode(yuv, true)
	if err != nil {
		t.Fatalf("encode #2: %v", err)
	}
	if !key {
		t.Fatalf("forceKey не дал IDR: %v", NALTypes(out))
	}
	enc.SetBitrate(1_000_000)
	enc.Close()
	enc.Close() // idempotent
	if _, _, err := enc.Encode(yuv, false); err == nil {
		t.Fatalf("Encode после Close должен вернуть ошибку")
	}
}

func TestRGBAToI420_OddSizes(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 7, 5))
	for i := range src.Pix {
		src.Pix[i] = uint8(i * 31)
	}
	dst := image.NewYCbCr(src.Rect, image.YCbCrSubsampleRatio420)
	RGBAToI420(src, dst) // не должно паниковать на нечётных размерах
	if dst.Y[0] == 0 && dst.Y[len(dst.Y)-1] == 0 {
		t.Fatalf("Y-плоскость не заполнена")
	}
}

// TestBenchModes — стоимость конверсии и encode на целевых режимах.
func TestBenchModes(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	for _, tc := range []struct{ w, h, fps, bitrate, n int }{
		{1280, 720, 30, 2_500_000, 60},
		{1920, 1080, 30, 4_000_000, 45},
		{1920, 1080, 60, 6_000_000, 45},
	} {
		frames := make([]*image.RGBA, 8)
		for i := range frames {
			frames[i] = synthFrame(tc.w, tc.h, i)
		}
		enc, err := New(Config{Width: tc.w, Height: tc.h, FPS: tc.fps, Bitrate: tc.bitrate})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		yuv := image.NewYCbCr(image.Rect(0, 0, tc.w, tc.h), image.YCbCrSubsampleRatio420)
		var conv, encT, maxT time.Duration
		var bytes int
		for i := 0; i < tc.n; i++ {
			st := time.Now()
			RGBAToI420(frames[i%len(frames)], yuv)
			c := time.Since(st)
			st = time.Now()
			out, _, err := enc.Encode(yuv, false)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			e := time.Since(st)
			conv += c
			encT += e
			if c+e > maxT {
				maxT = c + e
			}
			bytes += len(out)
		}
		enc.Close()
		t.Logf("%dx%d@%d: conv=%.1fms enc=%.1fms max=%.1fms kbps=%.0f budget=%.1fms",
			tc.w, tc.h, tc.fps,
			float64(conv.Microseconds())/1000/float64(tc.n),
			float64(encT.Microseconds())/1000/float64(tc.n),
			float64(maxT.Microseconds())/1000,
			float64(bytes)*8/1000/float64(tc.n)*float64(tc.fps),
			1000/float64(tc.fps))
	}
}
