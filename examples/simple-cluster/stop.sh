#!/bin/bash

# Simple Cluster Stop Script
# Stops all Rosetta nodes

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${YELLOW}Stopping Rosetta cluster...${NC}"

# Find and kill all rosetta processes
PIDS=$(pgrep -f "rosetta.*-id=node" || true)

if [ -z "$PIDS" ]; then
    echo -e "${YELLOW}No running Rosetta nodes found${NC}"
    exit 0
fi

echo "Found running nodes with PIDs: $PIDS"

# Kill processes gracefully
for PID in $PIDS; do
    echo "Stopping process $PID..."
    kill $PID 2>/dev/null || true
done

# Wait for processes to stop
sleep 1

# Force kill if still running
REMAINING=$(pgrep -f "rosetta.*-id=node" || true)
if [ ! -z "$REMAINING" ]; then
    echo -e "${YELLOW}Force stopping remaining processes...${NC}"
    for PID in $REMAINING; do
        kill -9 $PID 2>/dev/null || true
    done
fi

# Verify all stopped
sleep 1
STILL_RUNNING=$(pgrep -f "rosetta.*-id=node" || true)

if [ -z "$STILL_RUNNING" ]; then
    echo -e "${GREEN}All nodes stopped successfully!${NC}"
else
    echo -e "${RED}Warning: Some nodes may still be running${NC}"
    echo "PIDs: $STILL_RUNNING"
fi

# Check ports are free
echo ""
echo "Port status:"
for port in 8080 8081 8082 9080 9081 9082; do
    if lsof -i :$port > /dev/null 2>&1; then
        echo -e "  Port $port: ${RED}STILL IN USE${NC}"
    else
        echo -e "  Port $port: ${GREEN}FREE${NC}"
    fi
done

echo ""
echo -e "${GREEN}Cleanup complete!${NC}"
