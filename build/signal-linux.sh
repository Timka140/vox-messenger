#!/bin/bash
# Build signal service for Linux (run from WSL)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
OUT_DIR="$SCRIPT_DIR/out"

mkdir -p "$OUT_DIR"

echo "Building signal service for Linux..."
cd "$PROJECT_ROOT/server/signal"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$OUT_DIR/signal-server" ./cmd/start

echo "Done: $OUT_DIR/signal-server"
