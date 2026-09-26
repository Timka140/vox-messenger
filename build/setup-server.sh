#!/bin/bash
# First-time VPS setup: coturn + firewall
# Run on the server: sudo bash setup-server.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ENV_FILE="$SCRIPT_DIR/.env"

if [ ! -f "$ENV_FILE" ]; then
    echo "Error: $ENV_FILE not found. TURN credentials are read from .env"
    exit 1
fi

# Read TURN credentials from .env (single source of truth)
source "$ENV_FILE"

if [ -z "${TURN_URL:-}" ] || [ -z "${TURN_USERNAME:-}" ] || [ -z "${TURN_CREDENTIAL:-}" ]; then
    echo "Error: TURN_URL, TURN_USERNAME, TURN_CREDENTIAL must be set in .env"
    exit 1
fi

# Extract port from TURN_URL (turn:host:port → port)
TURN_PORT=$(echo "$TURN_URL" | grep -oP ':\K[0-9]+$' || echo "3478")
EXTERNAL_IP=$(hostname -I | awk '{print $1}')

echo "=== Nexus Server Setup ==="
echo "External IP: $EXTERNAL_IP"
echo "TURN port:   $TURN_PORT"
echo "TURN user:   $TURN_USERNAME"

# Install coturn
if ! command -v turnserver &>/dev/null; then
    echo "Installing coturn..."
    apt update -qq
    apt install -y coturn
else
    echo "coturn already installed"
fi

# Configure coturn
echo "Configuring coturn..."
cat > /etc/turnserver.conf << EOF
listening-port=$TURN_PORT
external-ip=$EXTERNAL_IP
realm=vox
user=$TURN_USERNAME:$TURN_CREDENTIAL
lt-cred-mech
no-tls
no-dtls
fingerprint
min-port=49152
max-port=65535
no-cli
EOF

# Enable and restart
systemctl enable coturn
systemctl restart coturn

echo "coturn started on port $TURN_PORT"

# Firewall (ufw)
if command -v ufw &>/dev/null; then
    echo "Configuring firewall..."
    ufw allow $TURN_PORT/tcp
    ufw allow $TURN_PORT/udp
    ufw allow 49152:65535/udp
    ufw allow 9750/tcp comment 'Debug service HTTP'
    # Сигналка и SFU: без этих правил клиенты не подключаются (2026-09-07 —
    # после ребута VPS порт 9741 оказался закрыт, скрипт его не открывал).
    ufw allow 9741/tcp comment 'Vox signal server WS'
    ufw allow 9760/tcp comment 'Vox SFU'
    echo "Firewall rules added"
fi

# Install fail2ban if not present
if ! command -v fail2ban-client &>/dev/null; then
    echo "Installing fail2ban..."
    apt install -y fail2ban
    systemctl enable --now fail2ban
fi

echo ""
echo "=== Setup complete ==="
echo "TURN: turn:${EXTERNAL_IP}:${TURN_PORT}"
echo "User: $TURN_USERNAME"
echo "Next: deploy signal-server with ./deploy-signal.sh"
