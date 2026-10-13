#!/usr/bin/env bash
# setup-node.sh <node> — одноразовая настройка нового узла кластера под root.
#
# Что делает (идемпотентно, можно перезапускать):
#   1. Пользователь сервисов (из NODE_SSH, например vox) + ваш SSH-ключ.
#   2. Каталог NODE_DIR, владелец — этот пользователь.
#   3. coturn: слушает и релеит только на NODE_IP, relay-порты
#      NODE_TURN_MIN_PORT..NODE_TURN_MAX_PORT, denied-peer-ip на все частные
#      сети (иначе через relay можно дотянуться до VPN/docker/localhost узла).
#      Учётные данные TURN — из build/remote/<node>/.env.
#   4. ufw: только ДОБАВЛЯЕТ правила Vox, существующие не трогает.
#
# Не ставит fail2ban: на узле могут жить чужие сервисы.
# Для узлов с NODE_SETUP=manual (vps1) отказывается работать.
#
# Usage: ./build/setup-node.sh vps2
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/node.sh
source "$SCRIPT_DIR/lib/node.sh"

if [ $# -ne 1 ]; then
    echo "usage: $0 <node>" >&2
    exit 2
fi
load_node "$1"

if [ "$NODE_SETUP" != "auto" ]; then
    echo "error: узел $NODE_NAME настраивается вручную (NODE_SETUP=$NODE_SETUP)" >&2
    exit 1
fi
if [ -z "$NODE_ROOT_SSH" ] || [ -z "$NODE_TURN_MIN_PORT" ] || [ -z "$NODE_TURN_MAX_PORT" ]; then
    echo "error: в конфиге $NODE_NAME нужны NODE_ROOT_SSH, NODE_TURN_MIN_PORT, NODE_TURN_MAX_PORT" >&2
    exit 1
fi

ENV_FILE="$(node_secret .env)"
TURN_URL="$(grep -E '^TURN_URL=' "$ENV_FILE" | cut -d= -f2-)"
TURN_USERNAME="$(grep -E '^TURN_USERNAME=' "$ENV_FILE" | cut -d= -f2-)"
TURN_CREDENTIAL="$(grep -E '^TURN_CREDENTIAL=' "$ENV_FILE" | cut -d= -f2-)"
if [ -z "$TURN_URL" ] || [ -z "$TURN_USERNAME" ] || [ -z "$TURN_CREDENTIAL" ]; then
    echo "error: в $ENV_FILE нужны TURN_URL, TURN_USERNAME, TURN_CREDENTIAL" >&2
    exit 1
fi
TURN_PORT="${TURN_URL##*:}"

SERVICE_USER="${NODE_SSH%@*}"
AUTH_KEY_FILE="${AUTH_KEY_FILE:-$HOME/.ssh/id_ed25519.pub}"
if [ ! -f "$AUTH_KEY_FILE" ]; then
    echo "error: нет публичного ключа $AUTH_KEY_FILE (задайте AUTH_KEY_FILE)" >&2
    exit 1
fi
AUTH_KEY="$(head -1 "$AUTH_KEY_FILE")"

echo "=== Настройка узла $NODE_NAME ($NODE_IP) через $NODE_ROOT_SSH ==="
echo "Пользователь сервисов: $SERVICE_USER, каталог: $NODE_DIR"
echo "TURN: порт $TURN_PORT, relay $NODE_TURN_MIN_PORT-$NODE_TURN_MAX_PORT, user $TURN_USERNAME"

{
    printf 'SERVICE_USER=%q\n' "$SERVICE_USER"
    printf 'SERVICE_DIR=%q\n' "$NODE_DIR"
    printf 'AUTH_KEY=%q\n' "$AUTH_KEY"
    printf 'NODE_IP=%q\n' "$NODE_IP"
    printf 'TURN_PORT=%q\n' "$TURN_PORT"
    printf 'TURN_MIN=%q\n' "$NODE_TURN_MIN_PORT"
    printf 'TURN_MAX=%q\n' "$NODE_TURN_MAX_PORT"
    printf 'TURN_USERNAME=%q\n' "$TURN_USERNAME"
    printf 'TURN_CREDENTIAL=%q\n' "$TURN_CREDENTIAL"
    cat <<'REMOTE'
set -euo pipefail

echo "--- 1. пользователь $SERVICE_USER ---"
if ! id "$SERVICE_USER" >/dev/null 2>&1; then
    useradd -m -s /bin/bash "$SERVICE_USER"
    echo "создан"
else
    echo "уже есть"
fi
HOME_DIR="$(getent passwd "$SERVICE_USER" | cut -d: -f6)"
install -d -m 700 -o "$SERVICE_USER" -g "$SERVICE_USER" "$HOME_DIR/.ssh"
AK="$HOME_DIR/.ssh/authorized_keys"
touch "$AK"
grep -qxF "$AUTH_KEY" "$AK" || echo "$AUTH_KEY" >> "$AK"
chown "$SERVICE_USER:$SERVICE_USER" "$AK"
chmod 600 "$AK"

echo "--- 2. каталог $SERVICE_DIR ---"
install -d -m 755 -o "$SERVICE_USER" -g "$SERVICE_USER" "$SERVICE_DIR"

echo "--- 3. coturn ---"
if ! command -v turnserver >/dev/null 2>&1; then
    DEBIAN_FRONTEND=noninteractive apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq coturn
else
    echo "coturn уже установлен"
fi

CONF=/etc/turnserver.conf
TMP="$(mktemp)"
cat > "$TMP" <<EOF
# Vox TURN — сгенерировано build/setup-node.sh, правки вручную перезапишутся.
listening-port=$TURN_PORT
listening-ip=$NODE_IP
relay-ip=$NODE_IP
external-ip=$NODE_IP
min-port=$TURN_MIN
max-port=$TURN_MAX
realm=vox
user=$TURN_USERNAME:$TURN_CREDENTIAL
lt-cred-mech
fingerprint
no-tls
no-dtls
no-cli
no-multicast-peers
# Relay только в публичный интернет: VPN, docker, localhost узла закрыты.
denied-peer-ip=0.0.0.0-0.255.255.255
denied-peer-ip=10.0.0.0-10.255.255.255
denied-peer-ip=100.64.0.0-100.127.255.255
denied-peer-ip=127.0.0.0-127.255.255.255
denied-peer-ip=169.254.0.0-169.254.255.255
denied-peer-ip=172.16.0.0-172.31.255.255
denied-peer-ip=192.168.0.0-192.168.255.255
denied-peer-ip=224.0.0.0-255.255.255.255
denied-peer-ip=::1
denied-peer-ip=fc00::-fdff:ffff:ffff:ffff:ffff:ffff:ffff:ffff
denied-peer-ip=fe80::-febf:ffff:ffff:ffff:ffff:ffff:ffff:ffff
EOF
if [ -f "$CONF" ] && cmp -s "$TMP" "$CONF"; then
    echo "конфиг coturn не изменился"
    rm -f "$TMP"
else
    [ -f "$CONF" ] && cp -p "$CONF" "$CONF.bak-$(date +%Y%m%d%H%M%S)"
    install -m 640 -o root -g turnserver "$TMP" "$CONF"
    rm -f "$TMP"
    echo "конфиг coturn записан"
fi
systemctl enable coturn >/dev/null 2>&1
systemctl restart coturn
sleep 1
echo "coturn: $(systemctl is-active coturn)"

echo "--- 4. ufw ---"
if command -v ufw >/dev/null 2>&1; then
    ufw allow "$TURN_PORT/tcp" comment 'Vox TURN'
    ufw allow "$TURN_PORT/udp" comment 'Vox TURN'
    ufw allow "$TURN_MIN:$TURN_MAX/udp" comment 'Vox TURN relay'
    ufw allow 9741/tcp comment 'Vox signal WS'
    ufw allow 9743/tcp comment 'Vox signal WSS'
    ufw allow 9760/tcp comment 'Vox SFU'
    ufw allow 9762/tcp comment 'Vox SFU TLS'
    # debug API (9750) наружу не открываем: слушает 127.0.0.1, логи — через SSH
else
    echo "ufw не установлен — правила не добавлены"
fi

echo "--- итог ---"
ss -tulnp | grep -E ":$TURN_PORT\b" || echo "WARN: $TURN_PORT не слушается"
ufw status numbered 2>/dev/null || true
REMOTE
} | ssh "${NODE_SSH_OPTS[@]}" "$NODE_ROOT_SSH" 'bash -s'

echo
echo "Узел $NODE_NAME настроен. Дальше:"
echo "  NODE=$NODE_NAME ./build/deploy-signal.sh"
echo "  NODE=$NODE_NAME ./build/deploy-sfu.sh"
echo "  NODE=$NODE_NAME ./build/deploy-debug.sh"
echo "  ./build/sync-releases.sh $NODE_NAME"
