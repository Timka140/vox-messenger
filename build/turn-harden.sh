#!/usr/bin/env bash
# turn-harden.sh — запрещает coturn релеить в частные сети на узле, который
# настроен вручную (vps1). Без этого любой, кто знает пароль TURN, через relay
# достаёт до WireGuard, docker и localhost узла.
#
# Запуск НА УЗЛЕ под sudo (нужен пароль, поэтому руками):
#   (NODE_SSH и NODE_SSH_PORT — из build/remote/vps1/ssh.conf)
#   scp -P $NODE_SSH_PORT build/turn-harden.sh $NODE_SSH:~/
#   ssh -t -p $NODE_SSH_PORT $NODE_SSH "sudo bash ~/turn-harden.sh"
#
# Идемпотентен: дописывает только недостающие строки, перед правкой делает
# бэкап /etc/turnserver.conf.bak-<время>, затем restart coturn.
# Интерфейсы, на которых слушает coturn, не меняет.
# Откат: sudo cp /etc/turnserver.conf.bak-<время> /etc/turnserver.conf && sudo systemctl restart coturn
set -euo pipefail

CONF=/etc/turnserver.conf

if [ "$(id -u)" != "0" ]; then
    echo "error: запускайте через sudo" >&2
    exit 1
fi
if [ ! -f "$CONF" ]; then
    echo "error: нет $CONF" >&2
    exit 1
fi

LINES=(
    "no-multicast-peers"
    "denied-peer-ip=0.0.0.0-0.255.255.255"
    "denied-peer-ip=10.0.0.0-10.255.255.255"
    "denied-peer-ip=100.64.0.0-100.127.255.255"
    "denied-peer-ip=127.0.0.0-127.255.255.255"
    "denied-peer-ip=169.254.0.0-169.254.255.255"
    "denied-peer-ip=172.16.0.0-172.31.255.255"
    "denied-peer-ip=192.168.0.0-192.168.255.255"
    "denied-peer-ip=224.0.0.0-255.255.255.255"
    "denied-peer-ip=::1"
    "denied-peer-ip=fc00::-fdff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"
    "denied-peer-ip=fe80::-febf:ffff:ffff:ffff:ffff:ffff:ffff:ffff"
)

missing=()
for l in "${LINES[@]}"; do
    grep -qxF "$l" "$CONF" || missing+=("$l")
done

if [ ${#missing[@]} -eq 0 ]; then
    echo "уже закрыто: все строки есть в $CONF"
else
    BAK="$CONF.bak-$(date +%Y%m%d%H%M%S)"
    cp -p "$CONF" "$BAK"
    {
        echo "# Relay только в публичный интернет (build/turn-harden.sh)"
        printf '%s\n' "${missing[@]}"
    } >> "$CONF"
    echo "бэкап: $BAK"
    echo "добавлено строк: ${#missing[@]}"
    systemctl restart coturn
    sleep 1
fi

echo "coturn: $(systemctl is-active coturn)"
echo "denied-peer-ip в конфиге: $(grep -c '^denied-peer-ip' "$CONF")"
