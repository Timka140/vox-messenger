#!/bin/bash
# Deploy debug-server на узел кластера (запуск из WSL или Git Bash).
#
#   ./build/deploy-debug.sh              # узел vps1
#   NODE=vps2 ./build/deploy-debug.sh
#
# debug-server читает .env сигналки узла — сначала deploy-signal.sh.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/node.sh
source "$SCRIPT_DIR/lib/node.sh"
load_node "${NODE:-vps1}"

BINARY="$SCRIPT_DIR/out/debug-server"
if [ ! -f "$BINARY" ]; then
    echo "Error: $BINARY not found. Run debug-linux.sh first."
    exit 1
fi

echo "Deploying debug-server to $REMOTE:$REMOTE_DIR ..."

if ! node_ssh "test -f $REMOTE_DIR/.env"; then
    echo "Error: на узле нет $REMOTE_DIR/.env — сначала NODE=$NODE_NAME ./build/deploy-signal.sh"
    exit 1
fi
node_ssh "cd $REMOTE_DIR && { ./stop-debug.sh 2>/dev/null || true; }"

echo "Uploading files..."
node_put "$REMOTE_DIR/" \
    "$BINARY" \
    "$SCRIPT_DIR/remote/start-debug.sh" \
    "$SCRIPT_DIR/remote/stop-debug.sh"

node_ssh "chmod +x $REMOTE_DIR/debug-server $REMOTE_DIR/start-debug.sh $REMOTE_DIR/stop-debug.sh && $REMOTE_DIR/start-debug.sh"

echo "Deploy complete."
