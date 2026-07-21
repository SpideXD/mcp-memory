#!/bin/bash
# start.sh — Build and start MCP Memory Server
# Usage: ./start.sh

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
BIN_DIR="$PROJECT_DIR/bin"
LOG_DIR="$PROJECT_DIR/logs"

cd "$PROJECT_DIR"

# Ensure log directory exists
mkdir -p "$LOG_DIR"

# Build binary
echo "Building mcp-memory..."
go build -o "$BIN_DIR/mcp-memory" .

# Kill any existing instances
"$SCRIPT_DIR/stop.sh" 2>/dev/null || true

# Start server
echo "Starting mcp-memory..."
"$BIN_DIR/mcp-memory" &
PID=$!
echo "Server started (PID: $PID)"
echo "Logs: $LOG_DIR/memory.log"

# Wait for healthy
echo "Waiting for services..."
for i in $(seq 1 120); do
    if curl -s http://localhost:8899/health > /dev/null 2>&1; then
        echo "Server ready (health: OK)"
        exit 0
    fi
    sleep 1
done

echo "ERROR: Server failed to start within 120s"
exit 1
