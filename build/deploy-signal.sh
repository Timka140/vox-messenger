#!/bin/bash
# Deploy signal-server на узел кластера (запуск из WSL или Git Bash).
#
#   ./build/deploy-signal.sh              # узел vps1
#   NODE=vps2 ./build/deploy-signal.sh    # другой узел из build/nodes/
#
# Секреты узла — build/remote/<node>/{.env,vox-signal.key}. Локальный ключ —
# source of truth: если его нет, он генерируется и заливается. Pubkey ключа
# сверяется с NODE_PUBKEY из конфига узла. Для узла с NODE_PINNED=1 pubkey
# синхронизируется в клиентский known_servers.go.
#
# Перед заменой бинаря на узле делается бэкап signal-server.bak-YYYYMMDD-HHMMSS
# и, при остановленном сервере, groups.db.bak-YYYYMMDD-HHMMSS (+ -wal/-shm) с той
# же меткой; бэкапов БД хранится три.
# Откат — обеими частями с одной меткой:
#   ./stop-signal.sh
#   cp signal-server.bak-STAMP signal-server
#   cp groups.db.bak-STAMP groups.db   # и -wal/-shm, если они есть в бэкапе
#   ./start-signal.sh
# Только бинарь откатывать нельзя, если между версиями менялась схема groups.db:
# старый бинарь не сможет вставить ни одного сообщения.
# Внимание: восстановленная старая БД откатывает журнал узла, и узел снова
# выдаст seq, которые уже есть у соседей и клиентов, — новые сообщения с
# такими номерами молча теряются. Это последнее средство, а не штатный откат:
# если схема не менялась, откатывайте только бинарь.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/node.sh
source "$SCRIPT_DIR/lib/node.sh"
load_node "${NODE:-vps1}"

BINARY="$SCRIPT_DIR/out/signal-server"
if [ ! -f "$BINARY" ]; then
    echo "Error: $BINARY not found. Run signal-linux.sh first."
    exit 1
fi

ENV_FILE="$(node_secret .env)"
PUBKEY="$(KEY_DIR="$SECRETS_DIR" UPDATE_CLIENT="$NODE_PINNED" "$SCRIPT_DIR/sync-signal-pubkey.sh" | tail -1)"
KEY_FILE="$(node_secret vox-signal.key)"
echo "Узел $NODE_NAME, signal pubkey: $PUBKEY"

if [ -n "$NODE_PUBKEY" ] && [ "$NODE_PUBKEY" != "$PUBKEY" ]; then
    echo "Error: pubkey ключа не совпадает с NODE_PUBKEY в build/nodes/$NODE_NAME.conf ($NODE_PUBKEY)"
    exit 1
fi
if [ -z "$NODE_PUBKEY" ]; then
    echo "Warning: впишите NODE_PUBKEY=$PUBKEY в build/nodes/$NODE_NAME.conf"
fi

# cluster.env — узлы кластера из build/nodes/*.conf (адреса и pubkey не
# секрет). Линки и адреса для клиентов — TLS-порт (wss, сертификат узла
# подтверждается его ключом при входе). Себя сервер из CLUSTER_PEERS пропускает сам. Узел без
# NODE_PUBKEY в список не попадает.
CLUSTER_ENV="$SECRETS_DIR/cluster.env"
PEERS=""
for conf in "$SCRIPT_DIR"/nodes/*.conf; do
    n="$(basename "$conf" .conf)"
    entry="$(load_node "$n" && [ -n "$NODE_PUBKEY" ] && echo "wss://$NODE_HOST:$NODE_WS_TLS_PORT/ws=$NODE_PUBKEY")" || entry=""
    if [ -z "$entry" ]; then
        echo "Warning: узел $n без NODE_PUBKEY — в cluster.env не попал"
        continue
    fi
    PEERS="${PEERS:+$PEERS;}$entry"
done
{
    echo "# Сгенерировано build/deploy-signal.sh — узлы кластера"
    echo "CLUSTER_SELF_ADDR='wss://$NODE_HOST:$NODE_WS_TLS_PORT/ws'"
    echo "CLUSTER_PEERS='$PEERS'"
    # SOFT_LIMIT=<n> ./build/deploy-signal.sh — временный лимит для проверки redirect.
    echo "CLUSTER_SOFT_LIMIT=${SOFT_LIMIT:-$NODE_SOFT_LIMIT}"
} > "$CLUSTER_ENV"
echo "cluster.env: $PEERS"

echo "Deploying signal-server to $REMOTE:$REMOTE_DIR ..."

# Стоп + бэкап текущего бинаря и БД групп с одной меткой времени. БД копируем
# при остановленном сервере — копия согласована. Бэкапов БД держим три.
node_ssh "mkdir -p $REMOTE_DIR && cd $REMOTE_DIR && { ./stop-signal.sh 2>/dev/null || true; } \
  && STAMP=\$(date +%Y%m%d-%H%M%S) \
  && if [ -f signal-server ]; then cp -p signal-server signal-server.bak-\$STAMP; fi \
  && DB=\$(grep -E '^GROUP_DB_PATH=' .env 2>/dev/null | cut -d= -f2) \
  && if [ -n \"\$DB\" ] && [ -f \"\$DB\" ]; then \
       for sfx in '' -wal -shm; do if [ -f \"\$DB\$sfx\" ]; then cp -p \"\$DB\$sfx\" \"\$DB.bak-\$STAMP\$sfx\"; fi; done; \
       ls -1t \"\$DB\".bak-*[0-9] 2>/dev/null | tail -n +4 | while read -r old; do rm -f \"\$old\" \"\$old-wal\" \"\$old-shm\"; done; \
     fi"

# scp перетирает vox-signal.key на узле — намеренно: локальный = source of truth.
echo "Uploading files..."
node_put "$REMOTE_DIR/" \
    "$BINARY" \
    "$KEY_FILE" \
    "$ENV_FILE" \
    "$CLUSTER_ENV" \
    "$SCRIPT_DIR/remote/start-signal.sh" \
    "$SCRIPT_DIR/remote/stop-signal.sh"

node_ssh "chmod +x $REMOTE_DIR/signal-server $REMOTE_DIR/start-signal.sh $REMOTE_DIR/stop-signal.sh && chmod 600 $REMOTE_DIR/vox-signal.key $REMOTE_DIR/.env && $REMOTE_DIR/start-signal.sh"

# Проверка: /health на WS-порту узла.
node_ssh "sleep 2; curl -fsS -m 5 http://127.0.0.1:\$(grep -E '^WS_PORT=' $REMOTE_DIR/.env | cut -d= -f2)/health && echo" \
    || echo "WARN: /health не ответил — смотрите $REMOTE_DIR/signal-server.log"

echo "Deploy complete."
if [ "$NODE_PINNED" = "1" ]; then
    echo
    echo "Не забудь:"
    echo "  1) git diff app/internal/network/known_servers.go  → закоммитить"
    echo "  2) build/release.sh vX.YY.YYY.YYYY \"changelog\" force"
fi
