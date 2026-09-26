#!/bin/bash
# Build SFU service for Linux (run from WSL)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
OUT_DIR="$SCRIPT_DIR/out"

mkdir -p "$OUT_DIR"

echo "Building SFU service for Linux..."
cd "$PROJECT_ROOT/server/sfu"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$OUT_DIR/sfu-server" ./cmd/start

echo "Done: $OUT_DIR/sfu-server"
