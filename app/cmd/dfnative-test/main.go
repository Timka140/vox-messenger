//go:build nx_dfn

// dfnative-test — оффлайн прогон WAV через upstream DeepFilterNet3 (cgo над
// third_party/libdf). Аналог старого dfn-test, но использует dfnative вместо Go-порта.
//
// Сборка:
//
//	cd app
//	go build -tags nx_dfn -o bin/dfnative-test ./cmd/dfnative-test
//
// Запуск:
//
//	./bin/dfnative-test \
//	    -model ../.local/models/DeepFilterNet3.tar.gz \
//	    -in ../tools/ns-reference/noisy_raw_48k.wav \
//	    -out ../tools/ns-reference/dfnative_out.wav
//
// Сравнение с референсом:
//
//	./bin/dfnative-test -model ... -in ... -out ... -ref ../tools/ns-reference/reference_out/noisy_raw_48k.wav
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"

	"vox_app/internal/audio/dfnative"
)

func main() {
	modelPath := flag.String("model", "", "путь к DeepFilterNet3.tar.gz")
	inFile := flag.String("in", "", "входной WAV (48 kHz, mono, 16-bit PCM)")
	outFile := flag.String("out", "", "выходной WAV")
	refFile := flag.String("ref", "", "опционально: reference WAV для сравнения (RefSNR в дБ)")
	attenLim := flag.Float64("atten-lim", 100, "attenuation limit в дБ (>=100 = без ограничения, full DFN; <0.01 = passthrough; 20-30 = мягкое подавление)")
	pfBeta := flag.Float64("pf-beta", 0, "post-filter beta (0 = выключен)")
	logEvery := flag.Int("log-every", 100, "период лога LSNR в кадрах (0 = выключен)")
	flag.Parse()

	if *modelPath == "" || *inFile == "" || *outFile == "" {
		fmt.Fprintln(os.Stderr, "usage: dfnative-test -model MODEL.tar.gz -in IN.wav -out OUT.wav [-ref REF.wav]")
		os.Exit(2)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	samples, sr, err := readWAVMono16(*inFile)
	if err != nil {
		die("read wav: %v", err)
	}
	logger.Info("input loaded",
		slog.Int("samples", len(samples)),
		slog.Int("sr", sr),
		slog.Float64("duration_s", float64(len(samples))/float64(sr)))
	if sr != dfnative.SampleRate {
		die("expected %d Hz, got %d Hz", dfnative.SampleRate, sr)
	}

	proc, err := dfnative.New(dfnative.Config{
		ModelPath:      *modelPath,
		AttenLimDb:     float32(*attenLim),
		PostFilterBeta: float32(*pfBeta),
	})
	if err != nil {
		die("dfnative.New: %v", err)
	}
	defer proc.Close()
	logger.Info("dfnative initialized", slog.String("model", *modelPath))

	n := len(samples)
	padded := ((n + dfnative.FrameSize - 1) / dfnative.FrameSize) * dfnative.FrameSize
	if padded > n {
		samples = append(samples, make([]float32, padded-n)...)
	}
	out := make([]float32, padded)

	frames := padded / dfnative.FrameSize
	var lsnrSum, lsnrMin, lsnrMax float64
	lsnrMin = math.Inf(1)
	lsnrMax = math.Inf(-1)
	for i := 0; i < frames; i++ {
		start := i * dfnative.FrameSize
		lsnr, err := proc.ProcessFrame(
			samples[start:start+dfnative.FrameSize],
			out[start:start+dfnative.FrameSize],
		)
		if err != nil {
			die("ProcessFrame[%d]: %v", i, err)
		}
		l := float64(lsnr)
		lsnrSum += l
		if l < lsnrMin {
			lsnrMin = l
		}
		if l > lsnrMax {
			lsnrMax = l
		}
		if *logEvery > 0 && i%*logEvery == 0 {
			logger.Debug("frame", slog.Int("i", i), slog.Float64("lsnr_db", l))
		}
	}
	logger.Info("processing done",
		slog.Int("frames", frames),
		slog.Float64("lsnr_mean_db", lsnrSum/float64(frames)),
		slog.Float64("lsnr_min_db", lsnrMin),
		slog.Float64("lsnr_max_db", lsnrMax))

	out = out[:n]
	if err := writeWAVMono16(*outFile, out, sr); err != nil {
		die("write wav: %v", err)
	}
	logger.Info("output written", slog.String("path", *outFile))

	if *refFile != "" {
		refSamples, refSr, err := readWAVMono16(*refFile)
		if err != nil {
			logger.Warn("ref load failed", slog.String("err", err.Error()))
			return
		}
		if refSr != sr {
			logger.Warn("ref sr mismatch", slog.Int("ref", refSr), slog.Int("cur", sr))
			return
		}
		cmp := n
		if len(refSamples) < cmp {
			cmp = len(refSamples)
		}
		var refSumSq, diffSq float64
		for i := 0; i < cmp; i++ {
			diff := float64(out[i] - refSamples[i])
			refSumSq += float64(refSamples[i]) * float64(refSamples[i])
			diffSq += diff * diff
		}
		mse := diffSq / float64(cmp)
		var snr float64
		if diffSq > 0 && refSumSq > 0 {
			snr = 10 * math.Log10(refSumSq/diffSq)
		}
		logger.Info("reference comparison",
			slog.Int("samples_compared", cmp),
			slog.Float64("mse", mse),
			slog.Float64("snr_vs_ref_db", snr))
	}
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[error] "+format+"\n", args...)
	os.Exit(1)
}

type wavHeader struct {
	ChunkID       [4]byte
	ChunkSize     uint32
	Format        [4]byte
	Subchunk1ID   [4]byte
	Subchunk1Size uint32
	AudioFormat   uint16
	NumChannels   uint16
	SampleRate    uint32
	ByteRate      uint32
	BlockAlign    uint16
	BitsPerSample uint16
}

func readWAVMono16(path string) ([]float32, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	var h wavHeader
	if err := binary.Read(f, binary.LittleEndian, &h); err != nil {
		return nil, 0, fmt.Errorf("read header: %w", err)
	}
	if string(h.ChunkID[:]) != "RIFF" || string(h.Format[:]) != "WAVE" {
		return nil, 0, fmt.Errorf("not a RIFF/WAVE file")
	}
	if h.AudioFormat != 1 {
		return nil, 0, fmt.Errorf("expected PCM, got format=%d", h.AudioFormat)
	}
	if h.NumChannels != 1 {
		return nil, 0, fmt.Errorf("expected mono, got %d channels", h.NumChannels)
	}
	if h.BitsPerSample != 16 {
		return nil, 0, fmt.Errorf("expected 16-bit, got %d-bit", h.BitsPerSample)
	}

	var tag [4]byte
	var size uint32
	for {
		if _, err := io.ReadFull(f, tag[:]); err != nil {
			return nil, 0, fmt.Errorf("find data chunk: %w", err)
		}
		if err := binary.Read(f, binary.LittleEndian, &size); err != nil {
			return nil, 0, err
		}
		if string(tag[:]) == "data" {
			break
		}
		if _, err := f.Seek(int64(size), io.SeekCurrent); err != nil {
			return nil, 0, err
		}
	}

	nSamples := int(size / 2)
	raw := make([]int16, nSamples)
	if err := binary.Read(f, binary.LittleEndian, &raw); err != nil {
		return nil, 0, fmt.Errorf("read samples: %w", err)
	}
	out := make([]float32, nSamples)
	const norm = 1.0 / 32768.0
	for i, s := range raw {
		out[i] = float32(s) * norm
	}
	return out, int(h.SampleRate), nil
}

func writeWAVMono16(path string, samples []float32, sampleRate int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	n := len(samples)
	dataSize := uint32(n * 2)
	h := wavHeader{
		ChunkID:       [4]byte{'R', 'I', 'F', 'F'},
		ChunkSize:     36 + dataSize,
		Format:        [4]byte{'W', 'A', 'V', 'E'},
		Subchunk1ID:   [4]byte{'f', 'm', 't', ' '},
		Subchunk1Size: 16,
		AudioFormat:   1,
		NumChannels:   1,
		SampleRate:    uint32(sampleRate),
		ByteRate:      uint32(sampleRate * 2),
		BlockAlign:    2,
		BitsPerSample: 16,
	}
	if err := binary.Write(f, binary.LittleEndian, &h); err != nil {
		return err
	}
	if _, err := f.Write([]byte{'d', 'a', 't', 'a'}); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, dataSize); err != nil {
		return err
	}

	pcm := make([]int16, n)
	for i, s := range samples {
		if s > 1 {
			s = 1
		} else if s < -1 {
			s = -1
		}
		pcm[i] = int16(s * 32767)
	}
	return binary.Write(f, binary.LittleEndian, pcm)
}
