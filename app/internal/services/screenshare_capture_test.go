package services

import (
	"image"
	"testing"
	"time"
)

func TestMatchOutput(t *testing.T) {
	left := image.Rect(-2560, 1, 0, 1441) // монитор левее основного
	main := image.Rect(0, 0, 2560, 1440)
	cases := []struct {
		name   string
		bounds []image.Rectangle
		want   image.Rectangle
		idx    int
	}{
		{"точное совпадение", []image.Rectangle{main}, main, 0},
		{"порядок DXGI другой", []image.Rectangle{main, left}, left, 1},
		{"порядок DXGI другой, основной", []image.Rectangle{main, left}, main, 0},
		{"нет такого монитора", []image.Rectangle{main}, left, -1},
		{"только левый верх совпал", []image.Rectangle{image.Rect(0, 0, 1920, 1080)}, main, -1},
		{"неоткрытый выход пропущен", []image.Rectangle{{}, left}, left, 1},
		{"пустой не совпадает с пустым", []image.Rectangle{{}}, image.Rectangle{}, -1},
	}
	for _, c := range cases {
		if got := matchOutput(c.bounds, c.want); got != c.idx {
			t.Errorf("%s: matchOutput = %d, ждали %d", c.name, got, c.idx)
		}
	}
}

func TestShouldFallbackDXGI(t *testing.T) {
	now := time.Now()
	if shouldFallbackDXGI(time.Time{}, now, "dxgi") {
		t.Error("без потери доступа переключаться нельзя")
	}
	if shouldFallbackDXGI(now.Add(-screenDXGIGiveUp/2), now, "dxgi") {
		t.Error("рано: доступ потерян меньше screenDXGIGiveUp")
	}
	if !shouldFallbackDXGI(now.Add(-screenDXGIGiveUp), now, "dxgi") {
		t.Error("пора на BitBlt")
	}
	if shouldFallbackDXGI(now.Add(-time.Minute), now, "bitblt") {
		t.Error("уже на BitBlt — переключать нечего")
	}
}
