package h264enc

import (
	"image"
	"runtime"
	"sync"
)

// NALTypes — типы NAL в Annex-B потоке (start code 00 00 01 / 00 00 00 01).
func NALTypes(b []byte) []int {
	var out []int
	for i := 0; i+3 < len(b); i++ {
		if b[i] != 0 || b[i+1] != 0 {
			continue
		}
		j := -1
		if b[i+2] == 1 {
			j = i + 3
		} else if b[i+2] == 0 && i+3 < len(b) && b[i+3] == 1 {
			j = i + 4
		}
		if j < 0 || j >= len(b) {
			continue
		}
		out = append(out, int(b[j]&0x1F))
		i = j
	}
	return out
}

// IsKeyframe — есть ли в потоке IDR-slice (NAL type 5). SPS/PPS (7/8)
// openh264 кладёт перед каждым IDR, поэтому такой кадр самодостаточен для
// декодера, стартующего с середины потока.
func IsKeyframe(annexB []byte) bool {
	for _, t := range NALTypes(annexB) {
		if t == 5 {
			return true
		}
	}
	return false
}

// RGBAToI420 — конверсия *image.RGBA → I420 (BT.601, коэффициенты как в
// image/color.RGBToYCbCr), строки делятся между горутинами. dst должен быть
// того же размера (4:2:0). Замер 2026-09-08: 1080p ≈ 2–3 мс на 16 CPU против
// ~6 мс у RGBA→I444→I420 в mediadevices.
func RGBAToI420(src *image.RGBA, dst *image.YCbCr) {
	w, h := src.Rect.Dx(), src.Rect.Dy()
	if w == 0 || h == 0 {
		return
	}
	workers := runtime.NumCPU()
	if workers > 8 {
		workers = 8
	}
	pairs := h / 2
	perWorker := (pairs + workers - 1) / workers
	if perWorker < 1 {
		perWorker = 1
	}
	var wg sync.WaitGroup
	for wk := 0; wk < workers; wk++ {
		y0 := wk * perWorker * 2
		y1 := y0 + perWorker*2
		if y1 > h {
			y1 = h
		}
		if y0 >= y1 {
			break
		}
		wg.Add(1)
		go func(y0, y1 int) {
			defer wg.Done()
			convertRows(src, dst, w, y0, y1)
		}(y0, y1)
	}
	wg.Wait()
}

func convertRows(src *image.RGBA, dst *image.YCbCr, w, y0, y1 int) {
	for y := y0; y+1 < y1 || (y < y1 && y+1 == y1); y += 2 {
		rows := 2
		if y+1 >= y1 {
			rows = 1
		}
		for x := 0; x < w; x += 2 {
			cols := 2
			if x+1 >= w {
				cols = 1
			}
			var sumCb, sumCr, n int
			for dy := 0; dy < rows; dy++ {
				row := src.Pix[(y+dy)*src.Stride:]
				yrow := dst.Y[(y+dy)*dst.YStride:]
				for dx := 0; dx < cols; dx++ {
					p := row[(x+dx)*4 : (x+dx)*4+4]
					r, g, b := int(p[0]), int(p[1]), int(p[2])
					yy := (19595*r + 38470*g + 7471*b + 1<<15) >> 16
					cb := (-11056*r - 21712*g + 32768*b + 1<<15) >> 16
					cr := (32768*r - 27440*g - 5328*b + 1<<15) >> 16
					yrow[x+dx] = uint8(yy)
					sumCb += cb
					sumCr += cr
					n++
				}
			}
			ci := (y/2)*dst.CStride + x/2
			dst.Cb[ci] = uint8(sumCb/n + 128)
			dst.Cr[ci] = uint8(sumCr/n + 128)
		}
	}
}
