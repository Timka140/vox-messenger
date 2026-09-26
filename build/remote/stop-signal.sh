#!/bin/bash
# Stop signal-server
DIR="$(cd "$(dirname "$0")" && pwd)"
PIDFILE="$DIR/signal-server.pid"

if [ ! -f "$PIDFILE" ]; then
    echo "signal-server not running (no pidfile)"
    exit 0
fi

PID=$(cat "$PIDFILE")
if kill -0 "$PID" 2>/dev/null; then
    kill "$PID"
    echo "signal-server stopped (PID $PID)"
else
    echo "signal-server not running (stale pidfile)"
fi
rm -f "$PIDFILE"
