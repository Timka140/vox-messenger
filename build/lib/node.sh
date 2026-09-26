#!/usr/bin/env bash
# lib/node.sh — общие функции скриптов, работающих с узлами кластера Vox.
#
# Узел описывается файлом build/nodes/<name>.conf (в git, без секретов).
# Секреты узла лежат в build/remote/<name>/ (.env, sfu.env, vox-signal.key,
# ssh.conf с NODE_SSH / NODE_SSH_PORT / NODE_ROOT_SSH / NODE_IP), каталог в .gitignore.
# Пример ssh.conf — build/nodes/ssh.conf.example.
#
# Использование:
#   source "$SCRIPT_DIR/lib/node.sh"
#   load_node "${NODE:-vps1}"
#   node_ssh "cd $REMOTE_DIR && ./start-signal.sh"
#   node_put "$REMOTE_DIR/" file1 file2
#
# DRY_RUN=1 — node_ssh / node_put / node_put_r только печатают команду.
# node_get (чтение с узла) выполняется всегда.

NODE_LIB_BUILD_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

NODE_SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=10)

# load_node <name> — читает конфиг узла и выставляет:
#   NODE_NAME, REMOTE (user@host), PORT, REMOTE_DIR, SECRETS_DIR,
#   плюс все NODE_* из конфига.
load_node() {
    local name="$1"
    local conf="$NODE_LIB_BUILD_DIR/nodes/$name.conf"
    if [ ! -f "$conf" ]; then
        echo "error: неизвестный узел '$name' (нет $conf)" >&2
        return 1
    fi

    NODE_NAME="$name"
    NODE_SSH=""
    NODE_SSH_PORT=22
    NODE_DIR=""
    NODE_IP=""
    NODE_HOST=""
    NODE_WS_PORT=9741
    NODE_SOFT_LIMIT=400
    NODE_PUBKEY=""
    NODE_PINNED=0
    NODE_SETUP=manual
    NODE_ROOT_SSH=""
    NODE_TURN_MIN_PORT=""
    NODE_TURN_MAX_PORT=""

    # shellcheck disable=SC1090
    source "$conf"
    local ssh_conf="$NODE_LIB_BUILD_DIR/remote/$name/ssh.conf"
    # shellcheck disable=SC1090
    [ -f "$ssh_conf" ] && source "$ssh_conf"

    if [ -z "$NODE_SSH" ]; then
        echo "error: нет NODE_SSH для узла $name (ожидается в $ssh_conf)" >&2
        return 1
    fi
    if [ -z "$NODE_DIR" ] || [ -z "$NODE_HOST" ]; then
        echo "error: в $conf должны быть NODE_DIR, NODE_HOST" >&2
        return 1
    fi
    if [ -z "$NODE_IP" ]; then
        echo "error: нет NODE_IP для узла $name (ожидается в $ssh_conf)" >&2
        return 1
    fi

    REMOTE="$NODE_SSH"
    PORT="$NODE_SSH_PORT"
    REMOTE_DIR="$NODE_DIR"
    SECRETS_DIR="$NODE_LIB_BUILD_DIR/remote/$name"
}

# node_secret <file> — путь к секрету текущего узла; ошибка, если файла нет.
node_secret() {
    local path="$SECRETS_DIR/$1"
    if [ ! -f "$path" ]; then
        echo "error: нет секрета $path для узла $NODE_NAME" >&2
        return 1
    fi
    printf '%s\n' "$path"
}

node_ssh() {
    if [ "${DRY_RUN:-0}" = "1" ]; then
        echo "[dry-run] ssh -p $PORT $REMOTE $*"
        return 0
    fi
    ssh "${NODE_SSH_OPTS[@]}" -p "$PORT" "$REMOTE" "$@"
}

# node_reachable — узел отвечает по SSH (выполняется и в DRY_RUN).
node_reachable() {
    ssh "${NODE_SSH_OPTS[@]}" -p "$PORT" "$REMOTE" true 2>/dev/null
}

# node_put <remote_dest> <local files...>
node_put() {
    local dest="$1"; shift
    if [ "${DRY_RUN:-0}" = "1" ]; then
        echo "[dry-run] scp -P $PORT $* $REMOTE:$dest"
        return 0
    fi
    scp "${NODE_SSH_OPTS[@]}" -P "$PORT" "$@" "$REMOTE:$dest"
}

# node_put_r <remote_dest> <local files/dirs...> — рекурсивно.
node_put_r() {
    local dest="$1"; shift
    if [ "${DRY_RUN:-0}" = "1" ]; then
        echo "[dry-run] scp -r -P $PORT $* $REMOTE:$dest"
        return 0
    fi
    scp "${NODE_SSH_OPTS[@]}" -r -P "$PORT" "$@" "$REMOTE:$dest"
}

# node_get <remote_src> <local_dest> — чтение, выполняется и в DRY_RUN.
node_get() {
    scp "${NODE_SSH_OPTS[@]}" -P "$PORT" "$REMOTE:$1" "$2"
}
