#!/usr/bin/env bash
# Качает GTCRN ONNX модели в эту директорию.
# Источник: https://github.com/Xiaobin-Rong/gtcrn (MIT)

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASE_URL="https://github.com/Xiaobin-Rong/gtcrn/raw/main/stream/onnx_models"

for name in gtcrn gtcrn_simple; do
  echo "[gtcrn] downloading ${name}.onnx"
  curl -fL -o "$SCRIPT_DIR/${name}.onnx" "$BASE_URL/${name}.onnx"
done

echo "[gtcrn] done:"
ls -lh "$SCRIPT_DIR"/*.onnx
