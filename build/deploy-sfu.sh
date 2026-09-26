#!/bin/bash
# Deploy sfu-server на узел кластера (запуск из WSL или Git Bash).
#
#   ./build/deploy-sfu.sh              # узел vps1
#   NODE=vps2 ./build/deploy-sfu.sh
#
# Секрет узла — build/remote/<node>/sfu.env. SFU_SECRET в нём обязан совпадать
# с SFU_SECRET в .env сигналки того же узла (сигналка выдаёт его клиентам).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/node.sh
source "$SCRIPT_DIR/lib/node.sh"
load_node "${NODE:-vps1}"

BINARY="$SCRIPT_DIR/out/sfu-server"
if [ ! -f "$BINARY" ]; then
    echo "Error: $BINARY not found. Run sfu-linux.sh first."
    exit 1
fi

SFU_ENV="$(node_secret sfu.env)"
ENV_FILE="$(node_secret .env)"
if [ "$(grep -E '^SFU_SECRET=' "$SFU_ENV")" != "$(grep -E '^SFU_SECRET=' "$ENV_FILE")" ]; then
    echo "Error: SFU_SECRET в sfu.env и .env узла $NODE_NAME различается"
    exit 1
fi

echo "Deploying sfu-server to $REMOTE:$REMOTE_DIR ..."

node_ssh "mkdir -p $REMOTE_DIR && cd $REMOTE_DIR && { ./stop-sfu.sh 2>/dev/null || true; }"

echo "Uploading files..."
node_put "$REMOTE_DIR/" \
    "$BINARY" \
    "$SFU_ENV" \
    "$SCRIPT_DIR/remote/start-sfu.sh" \
    "$SCRIPT_DIR/remote/stop-sfu.sh"

node_ssh "chmod +x $REMOTE_DIR/sfu-server $REMOTE_DIR/start-sfu.sh $REMOTE_DIR/stop-sfu.sh && chmod 600 $REMOTE_DIR/sfu.env && $REMOTE_DIR/start-sfu.sh"

echo "Deploy complete."
