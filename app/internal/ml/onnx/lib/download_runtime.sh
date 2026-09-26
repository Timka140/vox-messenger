#!/usr/bin/env bash
# Качает нативный onnxruntime v1.24.1 для всех целевых платформ Nexus.
# Версия должна совпадать с ORT_API_VERSION из yalue/onnxruntime_go (сейчас 24 → ≥1.24).
# Результат:
#   lib/windows-amd64/onnxruntime.dll
#   lib/linux-amd64/libonnxruntime.so
#   lib/darwin-arm64/libonnxruntime.dylib
#
# Эти файлы .gitignored — скрипт запускается один раз локально / в CI перед сборкой
# с -tags nx_onnx. Runtime_loader.go находит их по пути <exeDir>/lib/<os>-<arch>/<libname>.
#
# Использование:
#   ./download_runtime.sh             # все платформы
#   ./download_runtime.sh windows     # только windows
#   ./download_runtime.sh linux
#   ./download_runtime.sh darwin
#
# Требует: curl, unzip, tar.

set -euo pipefail

VERSION="${ONNXRUNTIME_VERSION:-1.24.1}"
BASE="https://github.com/microsoft/onnxruntime/releases/download/v${VERSION}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

download_windows() {
  local out="$SCRIPT_DIR/windows-amd64"
  mkdir -p "$out"
  echo "[onnxruntime] downloading windows-amd64 v${VERSION}"
  curl -fL -o "$TMP/win.zip" "${BASE}/onnxruntime-win-x64-${VERSION}.zip"
  unzip -q -o "$TMP/win.zip" -d "$TMP/win"
  cp "$TMP/win/onnxruntime-win-x64-${VERSION}/lib/onnxruntime.dll" "$out/onnxruntime.dll"
  echo "[onnxruntime] -> $out/onnxruntime.dll"
}

download_linux() {
  local out="$SCRIPT_DIR/linux-amd64"
  mkdir -p "$out"
  echo "[onnxruntime] downloading linux-amd64 v${VERSION}"
  curl -fL -o "$TMP/linux.tgz" "${BASE}/onnxruntime-linux-x64-${VERSION}.tgz"
  tar -xzf "$TMP/linux.tgz" -C "$TMP"
  cp "$TMP/onnxruntime-linux-x64-${VERSION}/lib/libonnxruntime.so.${VERSION}" "$out/libonnxruntime.so"
  echo "[onnxruntime] -> $out/libonnxruntime.so"
}

download_darwin() {
  local out="$SCRIPT_DIR/darwin-arm64"
  mkdir -p "$out"
  echo "[onnxruntime] downloading darwin-arm64 v${VERSION}"
  curl -fL -o "$TMP/mac.tgz" "${BASE}/onnxruntime-osx-arm64-${VERSION}.tgz"
  tar -xzf "$TMP/mac.tgz" -C "$TMP"
  cp "$TMP/onnxruntime-osx-arm64-${VERSION}/lib/libonnxruntime.${VERSION}.dylib" "$out/libonnxruntime.dylib"
  echo "[onnxruntime] -> $out/libonnxruntime.dylib"
}

target="${1:-all}"
case "$target" in
  windows) download_windows ;;
  linux)   download_linux ;;
  darwin)  download_darwin ;;
  all)     download_windows; download_linux; download_darwin ;;
  *) echo "unknown target: $target (use windows|linux|darwin|all)"; exit 2 ;;
esac

echo "[onnxruntime] done"
