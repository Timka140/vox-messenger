package services

import "testing"

const (
	tMin = 400_000
	tMax = 5_000_000
)

const tRaise = 60 // raiseTicks в тестах (2 с на 30 fps)

// high-WM для битрейта b: max(b/8*screenSatHighMillis/1000, screenSatHighMin).
func highWM(b int) uint64 {
	h := uint64(b) / 8 * screenSatHighMillis / 1000
	if h < screenSatHighMin {
		h = screenSatHighMin
	}
	return h
}

func TestAdaptBitrateCutsOnCongestion(t *testing.T) {
	b, gt, skip := adaptBitrate(highWM(2_500_000)+1, 2_500_000, tMin, tMax, 5, tRaise)
	if b != 2_000_000 || gt != 0 || !skip {
		t.Fatalf("got b=%d gt=%d skip=%v, want 2000000/0/true", b, gt, skip)
	}
}

func TestAdaptBitrateNeverBelowFloor(t *testing.T) {
	b, gt := 2_500_000, 0
	for i := 0; i < 100; i++ {
		b, gt, _ = adaptBitrate(1<<30, b, tMin, tMax, gt, tRaise)
	}
	if b != tMin {
		t.Fatalf("после 100 тиков конгестии b=%d, want floor %d", b, tMin)
	}
}

func TestAdaptBitrateRaisesSlowly(t *testing.T) {
	b, gt := 1_000_000, 0
	for i := 0; i < tRaise-1; i++ {
		b, gt, _ = adaptBitrate(0, b, tMin, tMax, gt, tRaise)
	}
	if b != 1_000_000 || gt != tRaise-1 {
		t.Fatalf("до порога битрейт не должен расти: b=%d gt=%d", b, gt)
	}
	b, gt, skip := adaptBitrate(0, b, tMin, tMax, gt, tRaise)
	if b != 1_100_000 || gt != 0 || skip {
		t.Fatalf("на пороге ожидали +10%%: b=%d gt=%d skip=%v", b, gt, skip)
	}
}

func TestAdaptBitrateNeverAboveCeiling(t *testing.T) {
	b, gt := 4_900_000, 0
	for i := 0; i < tRaise*10; i++ {
		b, gt, _ = adaptBitrate(0, b, tMin, tMax, gt, tRaise)
	}
	if b != tMax {
		t.Fatalf("b=%d, want ceiling %d", b, tMax)
	}
}

func TestAdaptBitrateMidZoneResetsGoodTicks(t *testing.T) {
	// Между low и high — держим и сбрасываем накопленные «здоровые» тики.
	mid := highWM(2_500_000) / 2
	b, gt, skip := adaptBitrate(mid, 2_500_000, tMin, tMax, 40, tRaise)
	if b != 2_500_000 || gt != 0 || skip {
		t.Fatalf("mid zone: b=%d gt=%d skip=%v", b, gt, skip)
	}
}

func TestAdaptBitrateWatermarkFloor(t *testing.T) {
	// На низком битрейте high-WM не вырождается ниже screenSatHighMin:
	// 48 KiB в буфере при 400 kbps — ещё не конгестия.
	b, _, skip := adaptBitrate(48*1024, tMin, tMin, tMax, 0, tRaise)
	if skip || b != tMin {
		t.Fatalf("низкий битрейт: skip=%v b=%d", skip, b)
	}
}

func TestAdaptBitrateClampsInput(t *testing.T) {
	b, _, _ := adaptBitrate(0, 10, tMin, tMax, 0, tRaise)
	if b < tMin {
		t.Fatalf("битрейт ниже min не должен пройти: %d", b)
	}
}
