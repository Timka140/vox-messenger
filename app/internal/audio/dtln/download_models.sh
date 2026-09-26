#!/usr/bin/env bash
# Качает DTLN модели (model_1.onnx / model_2.onnx) для audio_dtln.go.
#
# ВАЖНО: оригинальный репо breizhn/DTLN публикует только .h5 / .tflite веса,
# официальных .onnx нет. Модели нужно либо конвертировать локально из .h5
# через tf2onnx, либо скачать из зеркала, где конвертация уже сделана.
#
# По умолчанию скрипт пытается зеркало, URL которого задаётся через ENV:
#   DTLN_MODEL_1_URL  — прямой URL на model_1.onnx
#   DTLN_MODEL_2_URL  — прямой URL на model_2.onnx
#
# Если переменные не заданы — скрипт печатает инструкцию по ручной конвертации
# и завершается с кодом 2. Это сознательный выбор: мы не хардкодим
# third-party зеркала, которые могут исчезнуть.
#
# Ручная конвертация (один раз):
#
#   git clone https://github.com/breizhn/DTLN.git /tmp/dtln
#   cd /tmp/dtln
#   pip install tensorflow==2.12 tf2onnx
#   python - <<'PY'
#   import tensorflow as tf, tf2onnx
#   from DTLN_model import DTLN_model
#   m = DTLN_model()
#   m.build_DTLN_model(norm_stft=True)
#   m.model.load_weights('pretrained_model/model.h5')
#   # model.h5 содержит обе подсети — режем на две для потоковой обработки.
#   # См. notebooks в issues репозитория DTLN для полного рецепта.
#   PY
#
# и положить model_1.onnx / model_2.onnx в эту директорию.
#
# Альтернатива: использовать -tags nx_onnx без моделей — Deep опция просто
# деградирует до RNN с предупреждением в логе (см. audio_dtln.go).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [[ -z "${DTLN_MODEL_1_URL:-}" || -z "${DTLN_MODEL_2_URL:-}" ]]; then
  cat <<'EOF'
[dtln] DTLN_MODEL_1_URL / DTLN_MODEL_2_URL не заданы.
       Официальных ONNX релизов DTLN не существует; смотри комментарий
       в начале этого скрипта для инструкции по ручной конвертации из .h5.
       После получения model_1.onnx и model_2.onnx положи их в:
         app/internal/audio/dtln/
EOF
  exit 2
fi

echo "[dtln] downloading model_1.onnx"
curl -fL -o "$SCRIPT_DIR/model_1.onnx" "$DTLN_MODEL_1_URL"
echo "[dtln] downloading model_2.onnx"
curl -fL -o "$SCRIPT_DIR/model_2.onnx" "$DTLN_MODEL_2_URL"
echo "[dtln] done: $SCRIPT_DIR/{model_1,model_2}.onnx"
