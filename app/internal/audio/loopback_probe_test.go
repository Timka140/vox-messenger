package audio

import (
	"encoding/base64"
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
	"unicode/utf16"
)

// Ручная проверка на живой системе (играет тихий тон 1 с):
// VOX_LOOPBACK_PROBE=1 go test -run TestLoopbackProbe -v ./internal/audio/
func TestLoopbackProbe(t *testing.T) {
	if os.Getenv("VOX_LOOPBACK_PROBE") != "1" {
		t.Skip("ручная проверка")
	}
	lb := NewLoopback()
	defer lb.Close()
	if err := lb.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	// rms — общий уровень, tone — амплитуда на 1 кГц (20 периодов на кадр 960).
	measure := func(d time.Duration) (rms float64, frames int) {
		var sum, toneSum float64
		var n int
		deadline := time.Now().Add(d)
		for time.Now().Before(deadline) {
			f := lb.ReadFrame()
			if f == nil {
				time.Sleep(5 * time.Millisecond)
				continue
			}
			frames++
			var re, im float64
			for k := 0; k < len(f)/4; k++ {
				v := float64(int16(binary.LittleEndian.Uint16(f[k*4:])))
				sum += v * v
				n++
				w := 2 * math.Pi * 20 * float64(k) / 960
				re += v * math.Cos(w)
				im -= v * math.Sin(w)
			}
			toneSum += 2 * math.Hypot(re, im) / 960
		}
		if n > 0 {
			rms = math.Sqrt(sum / float64(n))
		}
		if frames > 0 {
			t.Logf("  тон 1 кГц: амплитуда %.0f", toneSum/float64(frames))
		}
		return
	}

	tone := func(amp float64, frames int) []byte {
		b := make([]byte, frames*4)
		for i := 0; i < frames; i++ {
			v := uint16(int16(amp * math.Sin(2*math.Pi*1000*float64(i)/48000)))
			binary.LittleEndian.PutUint16(b[i*4:], v)
			binary.LittleEndian.PutUint16(b[i*4+2:], v)
		}
		return b
	}

	base, bf := measure(500 * time.Millisecond)
	t.Logf("фон: rms=%.1f кадров=%d", base, bf)

	pb := NewStereoPlayback()
	if err := pb.Start(-1); err != nil {
		t.Fatalf("playback: %v", err)
	}
	pb.Write(tone(3000, 48000))
	own, of := measure(900 * time.Millisecond)
	pb.Close()
	t.Logf("свой процесс: rms=%.1f кадров=%d (ждём ~фон)", own, of)

	// Тот же тон из другого процесса.
	wav := filepath.Join(t.TempDir(), "tone.wav")
	pcm := tone(3000, 48000)
	hdr := make([]byte, 44)
	copy(hdr, "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:], uint32(36+len(pcm)))
	copy(hdr[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(hdr[16:], 16)
	binary.LittleEndian.PutUint16(hdr[20:], 1)
	binary.LittleEndian.PutUint16(hdr[22:], 2)
	binary.LittleEndian.PutUint32(hdr[24:], 48000)
	binary.LittleEndian.PutUint32(hdr[28:], 48000*4)
	binary.LittleEndian.PutUint16(hdr[32:], 4)
	binary.LittleEndian.PutUint16(hdr[34:], 16)
	copy(hdr[36:], "data")
	binary.LittleEndian.PutUint32(hdr[40:], uint32(len(pcm)))
	if err := os.WriteFile(wav, append(hdr, pcm...), 0o644); err != nil {
		t.Fatal(err)
	}
	// Через WMI: процесс-потомок теста попал бы в исключаемое дерево.
	inner := "powershell -NoProfile -Command (New-Object Media.SoundPlayer '" + wav + "').PlaySync()"
	script := "Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{CommandLine=\"" + inner + "\"} | Out-Null"
	u := utf16.Encode([]rune(script))
	enc := make([]byte, len(u)*2)
	for i, c := range u {
		binary.LittleEndian.PutUint16(enc[i*2:], c)
	}
	cmd := exec.Command("powershell", "-NoProfile", "-EncodedCommand", base64.StdEncoding.EncodeToString(enc))
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	ext, ef := measure(2500 * time.Millisecond)
	t.Logf("другой процесс: rms=%.1f кадров=%d (ждём тон ~3000)", ext, ef)
}
