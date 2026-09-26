#!/bin/bash
# Fetch logs from debug service on VPS (port :9750), либо напрямую по SSH
# из ~/vox/debug-logs/ если debug-server недоступен.
#
# Usage:
#   ./fetch-logs.sh [output_dir] [--latest|--all] [--ssh]
#
# По умолчанию тянет ВСЕ файлы каждого источника, объединённые сервером в
# хронологическом порядке. --latest — только последний файл (быстрее).
# --ssh принудительно использует SSH (минуя debug-server на :9750).

set -u

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/node.sh
source "$SCRIPT_DIR/lib/node.sh"
load_node "${NODE:-vps1}" || exit 1

API="http://$NODE_IP:9750"
SSH_HOST="$REMOTE"
SSH_PORT="$PORT"
REMOTE_LOG_DIR='~/vox/debug-logs'
SIGNAL_DEBUG_LOCAL="http://localhost:9742"

MODE="all"
OUT_DIR=""
USE_SSH=0
for arg in "$@"; do
    case "$arg" in
        --latest) MODE="latest" ;;
        --all)    MODE="all" ;;
        --ssh)    USE_SSH=1 ;;
        *)        OUT_DIR="$arg" ;;
    esac
done
OUT_DIR="${OUT_DIR:-$SCRIPT_DIR/../.local/logs}"
mkdir -p "$OUT_DIR"

sanitize_name() {
    # zeroes символы, которые мы не хотим видеть в именах локальных файлов
    # (match для сервера: [^a-zA-Z0-9#_-] → '_'; плюс '#' и ' ' → '_' для локалки)
    local s="$1"
    s="${s// /_}"
    s="${s//\#/_}"
    echo "$s"
}

# Проверяем debug-server; при недоступности — fallback на SSH
if [ "$USE_SSH" -eq 0 ]; then
    if ! curl -sSf --max-time 3 "$API/api/status" > /dev/null 2>&1; then
        echo "⚠️  debug-server на $API не отвечает — fallback на SSH" >&2
        USE_SSH=1
    fi
fi

# ---------- SSH path ----------
if [ "$USE_SSH" -eq 1 ]; then
    echo "Pulling logs via SSH from $SSH_HOST ..."
    # Триггерим сбор с онлайн-клиентов через signal-server debug API (localhost only)
    ssh -p "$SSH_PORT" "$SSH_HOST" "curl -sS -X POST $SIGNAL_DEBUG_LOCAL/collect" > /dev/null 2>&1 \
        || echo "    (collect trigger не сработал — signal-server возможно не отвечает)" >&2

    echo "Waiting 4s for logs to arrive..."
    sleep 4

    STAGE="$OUT_DIR/_remote"
    mkdir -p "$STAGE"
    if ! rsync -az --delete -e "ssh -p $SSH_PORT" "$SSH_HOST:$REMOTE_LOG_DIR/" "$STAGE/"; then
        echo "❌ rsync не сработал" >&2
        exit 1
    fi

    echo ""
    echo "Sources (via SSH):"
    found=0
    for d in "$STAGE"/*/; do
        [ -d "$d" ] || continue
        src="$(basename "$d")"
        safe="$(sanitize_name "$src")"
        out="$OUT_DIR/${safe}.log"
        : > "$out"
        # Файлы в директории: <timestamp>.jsonl — сортировка по имени = хронология
        find "$d" -maxdepth 1 -type f | sort | while read -r f; do
            cat "$f" >> "$out"
        done
        lines=$(wc -l < "$out")
        size=$(wc -c < "$out")
        nfiles=$(find "$d" -maxdepth 1 -type f | wc -l)
        printf "  %s -> %s (%s lines, %sB, %s files)\n" "$src" "$out" "$lines" "$size" "$nfiles"
        found=1
    done
    if [ "$found" -eq 0 ]; then
        echo "  (пусто — никто не пушил логи)"
    fi

    echo ""
    echo "Done. Logs saved to $OUT_DIR/ (raw per-session files в $STAGE/)"
    exit 0
fi

# ---------- HTTP path ----------
echo "Collecting logs from peers..."
curl -s -X POST "$API/api/collect" > /dev/null
echo ""

echo "Waiting 4s for logs to arrive..."
sleep 4

SOURCES=$(curl -sSf --max-time 5 "$API/api/sources" || true)
if [ -z "$SOURCES" ] || [ "$SOURCES" = "[]" ] || [ "$SOURCES" = "null" ]; then
    echo "⚠️  sources пустой — на VPS нет сохранённых логов." >&2
    echo "    Проверить напрямую: $0 --ssh" >&2
    exit 1
fi

echo "Sources:"
echo "$SOURCES" | python3 -m json.tool 2>/dev/null || echo "$SOURCES"
echo ""

echo "Mode: $MODE  (use --latest for last session only)"
echo ""

echo "$SOURCES" | python3 -c "
import json, sys, urllib.parse
sources = json.load(sys.stdin)
for s in sources:
    name = s['name']
    safe = name.replace('#', '_').replace(' ', '_')
    encoded = urllib.parse.quote(name, safe='')
    print(f'{encoded}|{safe}|{name}|{s[\"total_bytes\"]}|{s[\"files\"]}')
" 2>/dev/null | while IFS='|' read -r encoded safe display size files; do
    FILE="$OUT_DIR/${safe}.log"
    if [ "$MODE" = "latest" ]; then
        curl -s "$API/api/logs/$encoded/latest" > "$FILE"
    else
        curl -s "$API/api/logs/$encoded" > "$FILE"
    fi
    LINES=$(wc -l < "$FILE")
    echo "  $display -> $FILE ($LINES lines, ${size}B, ${files} files)"
done

echo ""
echo "Done. Logs saved to $OUT_DIR/"
