#!/usr/bin/env bash
# sync-signal-pubkey.sh — генерирует/читает vox-signal.key узла и печатает
# его pubkey.
#
# Идемпотентно: если ключ уже есть локально — просто читает pubkey.
# Если ключа нет — собирает локально signal-server, запускает с --print-pubkey
# чтобы он создал файл vox-signal.key (одно и то же что произойдёт на узле).
#
# Env:
#   KEY_DIR        каталог секретов узла (по умолчанию build/remote/vps1)
#
# Usage:
#   ./build/sync-signal-pubkey.sh
#   KEY_DIR=build/remote/vps2 ./build/sync-signal-pubkey.sh
#
# Output: hex pubkey последней строкой stdout.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
KEY_DIR="${KEY_DIR:-$SCRIPT_DIR/remote/vps1}"
KEY_FILE="$KEY_DIR/vox-signal.key"

mkdir -p "$KEY_DIR"

# Собираем локально signal-server (не Linux-кросс — родной для текущей машины).
TMPBIN="$(mktemp -t voxsig.XXXXXX)"
if [ "$(go env GOOS)" = "windows" ]; then
    rm -f "$TMPBIN"
    TMPBIN="$TMPBIN.exe"
fi
trap 'rm -f "$TMPBIN"' EXIT

(cd "$REPO_ROOT/server/signal" && go build -o "$TMPBIN" ./cmd/start)

# Если ключа нет — сгенерируем (signal-server создаст vox-signal.key в CWD)
if [ ! -f "$KEY_FILE" ]; then
    echo "vox-signal.key не найден в $KEY_DIR — генерирую новый..." >&2
    (cd "$KEY_DIR" && "$TMPBIN" --print-pubkey >/dev/null)
fi

# Получаем pubkey
PUBKEY="$(cd "$KEY_DIR" && "$TMPBIN" --print-pubkey | tr -d '\r')"

if ! echo "$PUBKEY" | grep -Eq '^[0-9a-f]{64}$'; then
    echo "Ошибка: невалидный pubkey '$PUBKEY'" >&2
    exit 1
fi

echo "$PUBKEY"
