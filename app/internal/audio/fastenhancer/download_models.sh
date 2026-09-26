#!/usr/bin/env bash
# Качает 5 wav2wav ONNX моделей FastEnhancer (DNS 16kHz) в эту директорию.
# Идемпотентно — повторный запуск перекачает существующие файлы.
#
# Источник: https://github.com/aask1357/fastenhancer/releases/tag/onnx-dns-v1.0.0
# Лицензия моделей: MIT (см. LICENSE репо).
#
# Запуск:
#   bash app/internal/audio/fastenhancer/download_models.sh

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASE_URL="https://github.com/aask1357/fastenhancer/releases/download/onnx-dns-v1.0.0"

for v in t b s m l; do
  echo "[fastenhancer] downloading fastenhancer_${v}.onnx"
  curl -fL -o "$SCRIPT_DIR/fastenhancer_${v}.onnx" "$BASE_URL/fastenhancer_${v}.onnx"
done

echo "[fastenhancer] done:"
ls -lh "$SCRIPT_DIR"/fastenhancer_*.onnx
