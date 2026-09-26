#!/bin/bash
# Stop sfu-server
DIR="$(cd "$(dirname "$0")" && pwd)"
PIDFILE="$DIR/sfu-server.pid"

if [ ! -f "$PIDFILE" ]; then
    echo "sfu-server not running (no pidfile)"
    exit 0
fi

PID=$(cat "$PIDFILE")
if kill -0 "$PID" 2>/dev/null; then
    kill "$PID"
    echo "sfu-server stopped (PID $PID)"
else
    echo "sfu-server not running (stale pidfile)"
fi
rm -f "$PIDFILE"
