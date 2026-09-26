#!/bin/bash
# Stop debug-server
DIR="$(cd "$(dirname "$0")" && pwd)"
PIDFILE="$DIR/debug-server.pid"

if [ ! -f "$PIDFILE" ]; then
    echo "debug-server not running (no pidfile)"
    exit 0
fi

PID=$(cat "$PIDFILE")
if kill -0 "$PID" 2>/dev/null; then
    kill "$PID"
    echo "debug-server stopped (PID $PID)"
else
    echo "debug-server not running (stale pidfile)"
fi
rm -f "$PIDFILE"
