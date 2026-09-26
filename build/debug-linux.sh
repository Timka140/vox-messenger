#!/bin/bash
# Build debug service for Linux (run from WSL)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
OUT_DIR="$SCRIPT_DIR/out"

mkdir -p "$OUT_DIR"

echo "Building debug service for Linux..."
cd "$PROJECT_ROOT/server/debug"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$OUT_DIR/debug-server" ./cmd/start

echo "Done: $OUT_DIR/debug-server"
