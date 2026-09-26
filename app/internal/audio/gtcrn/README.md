# GTCRN ONNX models

Streaming neural speech enhancement от [Xiaobin-Rong/gtcrn](https://github.com/Xiaobin-Rong/gtcrn)
(ICASSP 2024). MIT-лицензия.

## Файлы

| Файл | Размер | Параметры | MACs/sec | Датасет |
|---|---|---|---|---|
| `gtcrn.onnx` | 344 KB | 48.2 K | 33 M | DNS3 (Microsoft) |
| `gtcrn_simple.onnx` | 523 KB | ~50 K | ~33 M | Упрощённая архитектура |

GTCRN в ~23× легче FastEnhancer Large (1.1M params) и в ~500× легче DFN3.
CPU нагрузка ожидается < 1% даже на слабых процессорах.

## Streaming API (spectrogram-domain)

GTCRN работает в частотной области — в отличие от FastEnhancer
(wav→wav). На Go-стороне нужно делать STFT/iSTFT вокруг inference.

**Входы:**
- `mix` `[1, 257, 1, 2]` — complex STFT frame (257 freq bins, 1 time step, real+imag)
- `conv_cache` `[2, 1, 16, 16, 33]` — temporal conv state (encoder/decoder)
- `tra_cache` `[2, 3, 1, 1, 16]` — transformer/attention hidden states
- `inter_cache` `[2, 1, 33, 16]` — dual-path RNN intermediate

**Выходы:** `enh` (обработанный spectrogram frame) + 3 обновлённых cache.

## STFT параметры

- **n_fft**: 512
- **hop_length**: 256 (50% overlap)
- **sample_rate**: 16000 Hz
- **window**: `hann(512) ** 0.5` (sqrt от Hann, НЕ стандартный Hann!)
  — при 50% overlap это даёт точную реконструкцию через overlap-add
  (sum of sqrt-Hann^2 = Hann, sum(Hann) = const).

## Получение

`download_models.sh` качает обе `.onnx` из репо gtcrn.
