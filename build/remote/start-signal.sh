#!/bin/bash
# Start signal-server in background
DIR="$(cd "$(dirname "$0")" && pwd)"
PIDFILE="$DIR/signal-server.pid"

if [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
    echo "signal-server already running (PID $(cat "$PIDFILE"))"
    exit 0
fi

cd "$DIR"
set -a
source "$DIR/.env"
# Список узлов кластера генерирует build/deploy-signal.sh. Нет файла —
# одиночный сервер.
if [ -f "$DIR/cluster.env" ]; then
    source "$DIR/cluster.env"
fi
set +a
nohup ./signal-server > signal-server.log 2>&1 &
echo $! > "$PIDFILE"
echo "signal-server started (PID $!)"
