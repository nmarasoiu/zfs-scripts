#!/bin/bash
# USB Queue Monitor launcher and error checker

TOOL_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MONITOR="$TOOL_DIR/usb-queue-monitor"
LOG_FILE="$TOOL_DIR/usb-monitor.log"
PID_FILE="$TOOL_DIR/usb-monitor.pid"

start() {
    if [ -f "$PID_FILE" ] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
        echo "Monitor already running (PID: $(cat "$PID_FILE"))"
        return 1
    fi

    echo "Starting USB queue monitor in batch mode..."
    nohup "$MONITOR" -batch >> "$LOG_FILE" 2>&1 &
    echo $! > "$PID_FILE"
    echo "Started (PID: $!)"
    echo "Log file: $LOG_FILE"
}

stop() {
    if [ ! -f "$PID_FILE" ]; then
        echo "Monitor not running (no PID file)"
        return 1
    fi

    PID=$(cat "$PID_FILE")
    if kill -0 "$PID" 2>/dev/null; then
        echo "Stopping monitor (PID: $PID)..."
        kill "$PID"
        rm -f "$PID_FILE"
        echo "Stopped"
    else
        echo "Monitor not running (stale PID file)"
        rm -f "$PID_FILE"
    fi
}

status() {
    if [ -f "$PID_FILE" ] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
        PID=$(cat "$PID_FILE")
        echo "Monitor is running (PID: $PID)"

        # Show recent stats
        echo ""
        echo "Recent statistics (last 20 lines):"
        tail -20 "$LOG_FILE"

        return 0
    else
        echo "Monitor is not running"
        [ -f "$PID_FILE" ] && rm -f "$PID_FILE"
        return 1
    fi
}

errors() {
    if [ ! -f "$LOG_FILE" ]; then
        echo "No log file found"
        return 1
    fi

    echo "Checking for errors in log file..."
    ERROR_COUNT=$(grep -c "ERROR:" "$LOG_FILE" 2>/dev/null || true)
    ERROR_COUNT=${ERROR_COUNT:-0}

    if [ "$ERROR_COUNT" -eq 0 ]; then
        echo "No errors found - monitor running cleanly!"
    else
        echo "Found $ERROR_COUNT error(s):"
        grep "ERROR:" "$LOG_FILE"
    fi
}

tail_log() {
    if [ ! -f "$LOG_FILE" ]; then
        echo "No log file found"
        return 1
    fi

    echo "Following log file (Ctrl+C to stop)..."
    tail -f "$LOG_FILE"
}

case "$1" in
    start)
        start
        ;;
    stop)
        stop
        ;;
    restart)
        stop
        sleep 1
        start
        ;;
    status)
        status
        ;;
    errors)
        errors
        ;;
    tail|follow)
        tail_log
        ;;
    *)
        echo "USB Queue Monitor Control Script"
        echo ""
        echo "Usage: $0 {start|stop|restart|status|errors|tail}"
        echo ""
        echo "  start   - Start monitor in background"
        echo "  stop    - Stop running monitor"
        echo "  restart - Restart monitor"
        echo "  status  - Check if running and show recent stats"
        echo "  errors  - Check log file for any errors"
        echo "  tail    - Follow log file in real-time"
        echo ""
        echo "Log file: $LOG_FILE"
        exit 1
        ;;
esac
