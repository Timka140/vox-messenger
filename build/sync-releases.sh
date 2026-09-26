#!/usr/bin/env bash
# sync-releases.sh <dst> [src=vps1] — копирует releases/ целиком с узла src
# на узел dst потоком tar через локальную машину, затем сверяет manifest.
#
# Нужен для первичного заполнения нового узла и для догонки узла, который
# был недоступен во время release.sh. Замена атомарная: releases.new → releases.
#
# Usage: ./build/sync-releases.sh vps2
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/node.sh
source "$SCRIPT_DIR/lib/node.sh"

if [ $# -lt 1 ]; then
    echo "usage: $0 <dst> [src=vps1]" >&2
    exit 2
fi
DST="$1"
SRC="${2:-vps1}"
if [ "$DST" = "$SRC" ]; then
    echo "error: src и dst совпадают" >&2
    exit 1
fi

load_node "$SRC"
S_REMOTE="$REMOTE"; S_PORT="$PORT"; S_DIR="$REMOTE_DIR"
load_node "$DST"
D_REMOTE="$REMOTE"; D_PORT="$PORT"; D_DIR="$REMOTE_DIR"

echo "=== releases: $SRC ($S_REMOTE:$S_DIR) → $DST ($D_REMOTE:$D_DIR) ==="

ssh "${NODE_SSH_OPTS[@]}" -p "$S_PORT" "$S_REMOTE" "cd $S_DIR/releases && tar cf - ." \
    | ssh "${NODE_SSH_OPTS[@]}" -p "$D_PORT" "$D_REMOTE" \
        "set -e; mkdir -p $D_DIR && cd $D_DIR && rm -rf releases.new && mkdir releases.new && tar xf - -C releases.new && rm -rf releases.old && if [ -d releases ]; then mv releases releases.old; fi && mv releases.new releases && rm -rf releases.old"

S_SUM="$(ssh "${NODE_SSH_OPTS[@]}" -p "$S_PORT" "$S_REMOTE" "sha256sum $S_DIR/releases/manifest.json" | cut -d' ' -f1)"
D_SUM="$(ssh "${NODE_SSH_OPTS[@]}" -p "$D_PORT" "$D_REMOTE" "sha256sum $D_DIR/releases/manifest.json" | cut -d' ' -f1)"
echo "manifest $SRC: $S_SUM"
echo "manifest $DST: $D_SUM"
if [ "$S_SUM" != "$D_SUM" ]; then
    echo "error: manifest на узлах различается" >&2
    exit 1
fi
ssh "${NODE_SSH_OPTS[@]}" -p "$D_PORT" "$D_REMOTE" "du -sh $D_DIR/releases"
echo "Готово."
