# FastEnhancer ONNX models

Streaming neural noise suppression от [aask1357/fastenhancer](https://github.com/aask1357/fastenhancer)
(ICASSP 2026). MIT-лицензия.

## Файлы

| Файл | Размер | hop | inferences/sec @ 16k | MACs/inf | MACs/sec |
|---|---|---|---|---|---|
| `fastenhancer_t.onnx` | 125 KB | 256 | 62.5  | 60 M  | 3.75 G  |
| `fastenhancer_b.onnx` | 413 KB | 256 | 62.5  | 262 M | 16.4 G  |
| `fastenhancer_s.onnx` | 833 KB | 256 | 62.5  | 664 M | 41.5 G  |
| `fastenhancer_m.onnx` | 2.0 MB | 160 | 100   | 2.9 G | 290 G   |
| `fastenhancer_l.onnx` | 4.5 MB | 100 | 160   | 12 G  | 1.92 T  |

Все обучены на DNS Challenge датасете для речи 16 kHz.

## Streaming API (одинаковый для всех)

**Входы:** `wav_in [1, hop]` + `cache_in_0..N-1` (N = 4..7 в зависимости от модели,
shape узнаётся при загрузке через ORT `GetInputs()`).
**Выходы:** `wav_out [1, hop]` + `cache_out_0..N-1`.

Cache на старте инициализируется нулями; `cache_out_*` каждого инференса
становится `cache_in_*` следующего. Размер cache варьируется:
- `cache_0/1` — wav-domain, размер коррелирует с задержкой analysis (256/352/412)
- `cache_2..N-1` — RNN hidden states `[1, freq_bins, channels]`

## Получение

`download_models.sh` качает все 5 файлов через curl из релиза `onnx-dns-v1.0.0`.
Запускать один раз — модели коммитятся в репо вместе с DFN/Silero.

## Адаптация к Vox-фрейму 320 samples

Vox capture/playback работают 20 ms = 320 samples @ 16 kHz, ни одна модель FE
не имеет такого hop. Поэтому в `audio_fastenhancer.go` есть ring buffer:
input копится → когда набралось >= hop, делаем inference → output копится →
выдаём 320 наружу. Доп. латентность ≤ 1 фрейм (20 ms).
