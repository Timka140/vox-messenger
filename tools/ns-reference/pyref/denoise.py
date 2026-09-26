"""
Reference DeepFilterNet3 denoiser — используется как эталон для сравнения
с нашей Go реализацией (app/internal/audio/dfn).

Запуск:
    python denoise.py --in ../noisy_raw_48k.wav --out ../clean_reference.wav

Что делает:
  1. Загружает предобученную модель DFN3 через df.enhance.init_df()
  2. Читает input WAV (любой SR — модель сама ресэмплит до 48 kHz)
  3. Прогоняет через enhance()
  4. Сохраняет результат

Дополнительные флаги:
  --dump-features  печатает feat_erb/feat_spec/mask/coefs для первых N кадров
                   (помогает сравнить с моими Go логами)
"""

import argparse
import sys
import time
from pathlib import Path

import torch

from df.enhance import enhance, init_df, load_audio, save_audio


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--in", dest="infile", required=True, help="входной WAV")
    ap.add_argument("--out", dest="outfile", required=True, help="выходной WAV")
    ap.add_argument("--model", default="DeepFilterNet3", help="имя модели (DeepFilterNet3 default)")
    ap.add_argument("--dump-features", type=int, default=0, metavar="N",
                    help="печатать статистику фич/масок для первых N кадров")
    args = ap.parse_args()

    in_path = Path(args.infile).resolve()
    out_path = Path(args.outfile).resolve()

    if not in_path.exists():
        print(f"[error] input not found: {in_path}", file=sys.stderr)
        sys.exit(1)

    print(f"[info] loading model: {args.model}", file=sys.stderr)
    t0 = time.time()
    model, df_state, _ = init_df(args.model)
    print(f"[info] model loaded in {time.time()-t0:.2f}s, sr={df_state.sr()}", file=sys.stderr)

    print(f"[info] reading {in_path}", file=sys.stderr)
    audio, _ = load_audio(str(in_path), sr=df_state.sr())
    print(f"[info] input: shape={tuple(audio.shape)} sr={df_state.sr()} "
          f"duration={audio.shape[-1] / df_state.sr():.2f}s", file=sys.stderr)

    t0 = time.time()
    enhanced = enhance(model, df_state, audio)
    print(f"[info] enhanced in {time.time()-t0:.2f}s", file=sys.stderr)

    save_audio(str(out_path), enhanced, df_state.sr())
    print(f"[info] wrote {out_path}", file=sys.stderr)

    # RMS статистика для сравнения с Go реализацией
    in_rms = torch.sqrt((audio ** 2).mean()).item()
    out_rms = torch.sqrt((enhanced ** 2).mean()).item()
    print(f"[stats] in_rms={in_rms:.6f} out_rms={out_rms:.6f} "
          f"suppression_db={20 * (out_rms / in_rms).__pow__(1) if in_rms > 0 else 0:.2f}",
          file=sys.stderr)
    import math
    if in_rms > 0:
        supp_db = 20 * math.log10(out_rms / in_rms) if out_rms > 0 else -99
        print(f"[stats] suppression: {supp_db:+.2f} dB", file=sys.stderr)

    if args.dump_features:
        dump_features(model, df_state, audio, n_frames=args.dump_features)


def dump_features(model, df_state, audio, n_frames=5):
    """Прогоняем первые N фреймов вручную и печатаем feat_erb, mask, coefs.

    Используется для пошагового сравнения с Go реализацией — показывает
    эталонные значения, которые должны совпадать с моими Go логами.
    """
    print(f"\n[dump] первые {n_frames} кадров detailed features:", file=sys.stderr)

    try:
        from df.modules import MaskOnlyFD
    except ImportError:
        MaskOnlyFD = None

    # DFN state features вычисляются внутри df_state (Rust библиотека).
    # Проще — руками STFT через torch и посмотреть что модель даёт.
    import numpy as np

    model.eval()
    with torch.no_grad():
        # Берём первый кадр и смотрим mask вывод
        # enhance() работает чанками, для простоты прогоним весь аудио и заберём
        # промежуточные через hook
        hooks = {}
        captured = {}

        def make_hook(name):
            def h(module, inp, out):
                captured[name] = out.detach().cpu() if hasattr(out, "detach") else out
            return h

        # Пробуем найти ERB decoder и df decoder в модели
        for name, module in model.named_modules():
            if "erb_dec" in name.lower() and name.count(".") == 1:
                hooks[name] = module.register_forward_hook(make_hook(f"erb_dec_{name}"))
            if "df_dec" in name.lower() and name.count(".") == 1:
                hooks[name] = module.register_forward_hook(make_hook(f"df_dec_{name}"))

        _ = enhance(model, df_state, audio[..., :48000])  # ~1 сек чтобы не тормозить

        for h in hooks.values():
            h.remove()

        if not captured:
            print("[dump] не удалось захватить промежуточные тензоры (hooks не сработали)",
                  file=sys.stderr)
            print("[dump] доступные модули:", file=sys.stderr)
            for name, _ in model.named_modules():
                print(f"    {name}", file=sys.stderr)
            return

        for key, t in captured.items():
            if isinstance(t, tuple):
                for i, ti in enumerate(t):
                    print(f"[dump] {key}[{i}]: shape={tuple(ti.shape)} "
                          f"min={ti.min().item():.4f} max={ti.max().item():.4f} "
                          f"mean={ti.mean().item():.4f}", file=sys.stderr)
            else:
                print(f"[dump] {key}: shape={tuple(t.shape)} "
                      f"min={t.min().item():.4f} max={t.max().item():.4f} "
                      f"mean={t.mean().item():.4f}", file=sys.stderr)


if __name__ == "__main__":
    main()
