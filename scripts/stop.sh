#!/bin/bash
# stop.sh — Gracefully stop MCP Memory server and all connected services
# Usage: ./stop.sh

set -e

MCP_PORT="${MCP_PORT:-8899}"
MCP_HOST="${MCP_HOST:-127.0.0.1}"

echo "Stopping MCP Memory server..."

# Step 1: Graceful stop via HTTP endpoint
if curl -s --max-time 5 -X POST "http://${MCP_HOST}:${MCP_PORT}/stop" > /dev/null 2>&1; then
    echo "  ✓ Server accepted stop signal (graceful shutdown)"
    sleep 2
else
    echo "  ⚠ Server not responding on HTTP, using process kill"
fi

# Step 2: Kill mcp-memory processes
if pgrep -f "mcp-memory" > /dev/null 2>&1; then
    echo "  ✓ Sending SIGTERM to mcp-memory..."
    pkill -f "mcp-memory" 2>/dev/null || true
    sleep 3
fi

# Step 3: Force kill if still running
if pgrep -f "mcp-memory" > /dev/null 2>&1; then
    echo "  ⚠ Force killing mcp-memory..."
    pkill -9 -f "mcp-memory" 2>/dev/null || true
    sleep 1
fi

# Step 4: Stop llama.cpp servers
for port in 8080 8081; do
    if lsof -ti :$port > /dev/null 2>&1; then
        echo "  ✓ Stopping llama.cpp on port $port..."
        kill $(lsof -ti :$port) 2>/dev/null || true
    fi
done

# Step 5: Stop Hindsight
if pgrep -f "hindsight-api" > /dev/null 2>&1; then
    echo "  ✓ Stopping Hindsight..."
    pkill -f "hindsight-api" 2>/dev/null || true
fi

# Step 6: Clean up PID file
PROJECT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
rm -f "$PROJECT_DIR/logs/.mcp-pids.json"

# Step 7: Verify all ports are free
sleep 2
for port in "$MCP_PORT" 8080 8081 8888; do
    if lsof -ti :$port > /dev/null 2>&1; then
        echo "  ⚠ Port $port still in use — force killing..."
        kill -9 $(lsof -ti :$port) 2>/dev/null || true
    fi
done

echo "MCP Memory server stopped."
