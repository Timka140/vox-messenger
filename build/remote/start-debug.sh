#!/bin/bash
# Start debug-server in background
DIR="$(cd "$(dirname "$0")" && pwd)"
PIDFILE="$DIR/debug-server.pid"

if [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
    echo "debug-server already running (PID $(cat "$PIDFILE"))"
    exit 0
fi

cd "$DIR"
set -a
source "$DIR/.env"
set +a

export LOG_DIR="$DIR/debug-logs"
export SIGNAL_DEBUG_ADDR="localhost:${DEBUG_PORT:-9742}"
export HTTP_PORT="${HTTP_PORT:-9750}"

nohup ./debug-server > debug-server.log 2>&1 &
echo $! > "$PIDFILE"
echo "debug-server started (PID $!)"
