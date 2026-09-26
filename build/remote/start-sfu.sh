#!/bin/bash
# Start sfu-server in background
DIR="$(cd "$(dirname "$0")" && pwd)"
PIDFILE="$DIR/sfu-server.pid"

if [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
    echo "sfu-server already running (PID $(cat "$PIDFILE"))"
    exit 0
fi

cd "$DIR"
set -a
source "$DIR/sfu.env"
set +a
nohup ./sfu-server > sfu-server.log 2>&1 &
echo $! > "$PIDFILE"
echo "sfu-server started (PID $!)"
