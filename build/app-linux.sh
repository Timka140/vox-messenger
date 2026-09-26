#!/bin/bash
# Build Wails app for Linux (run from WSL)
# Requires: Go, GCC (for CGO/SQLCipher), libgtk-3-dev, libwebkit2gtk-4.1-dev
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
OUT_DIR="$SCRIPT_DIR/out"

mkdir -p "$OUT_DIR"

echo "Building Vox app for Linux..."
cd "$PROJECT_ROOT/app"
CGO_ENABLED=1 go build -o "$OUT_DIR/vox" .

echo "Done: $OUT_DIR/vox"
