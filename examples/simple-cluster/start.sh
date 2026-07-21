#!/bin/bash

# Simple Cluster Start Script
# Starts a 3-node Rosetta cluster on localhost

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Configuration
BINARY="../../rosetta"
LOG_DIR="./logs"

# Check if binary exists
if [ ! -f "$BINARY" ]; then
    echo -e "${RED}Error: Rosetta binary not found at $BINARY${NC}"
    echo "Please build it first: go build -o rosetta ../../main.go"
    exit 1
fi

# Create log directory
mkdir -p $LOG_DIR

# Clean up old logs
rm -f $LOG_DIR/*.log

echo -e "${GREEN}Starting 3-node Rosetta cluster...${NC}"
echo ""

# Start Node 1
echo -e "${YELLOW}Starting Node 1...${NC}"
$BINARY \
  -id=node1 \
  -listen=localhost:8080 \
  -http=localhost:9080 \
  -peers=node2:localhost:8081,node3:localhost:8082 \
  > $LOG_DIR/node1.log 2>&1 &
NODE1_PID=$!
echo "Node 1 started (PID: $NODE1_PID)"

# Start Node 2
echo -e "${YELLOW}Starting Node 2...${NC}"
$BINARY \
  -id=node2 \
  -listen=localhost:8081 \
  -http=localhost:9081 \
  -peers=node1:localhost:8080,node3:localhost:8082 \
  > $LOG_DIR/node2.log 2>&1 &
NODE2_PID=$!
echo "Node 2 started (PID: $NODE2_PID)"

# Start Node 3
echo -e "${YELLOW}Starting Node 3...${NC}"
$BINARY \
  -id=node3 \
  -listen=localhost:8082 \
  -http=localhost:9082 \
  -peers=node1:localhost:8080,node2:localhost:8081 \
  > $LOG_DIR/node3.log 2>&1 &
NODE3_PID=$!
echo "Node 3 started (PID: $NODE3_PID)"

echo ""
echo -e "${GREEN}All nodes started!${NC}"
echo ""

# Wait for nodes to be ready
echo -e "${YELLOW}Waiting for cluster to be ready...${NC}"
sleep 2

# Check cluster status
echo ""
echo -e "${GREEN}Cluster Status:${NC}"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

for port in 9080 9081 9082; do
    if curl -s http://localhost:$port/status > /dev/null 2>&1; then
        STATUS=$(curl -s http://localhost:$port/status)
        NODE_ID=$(echo $STATUS | jq -r '.node_id')
        STATE=$(echo $STATUS | jq -r '.state')
        TERM=$(echo $STATUS | jq -r '.term')
        IS_LEADER=$(echo $STATUS | jq -r '.is_leader')

        if [ "$IS_LEADER" = "true" ]; then
            echo -e "${GREEN}✓ $NODE_ID${NC} - State: ${GREEN}$STATE${NC} (Term: $TERM) - HTTP: :$port"
        else
            echo -e "${GREEN}✓ $NODE_ID${NC} - State: $STATE (Term: $TERM) - HTTP: :$port"
        fi
    else
        echo -e "${RED}✗ Node on port $port is not responding${NC}"
    fi
done

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

# Get leader info
LEADER_INFO=$(curl -s http://localhost:9080/leader 2>/dev/null || echo '{}')
LEADER_ID=$(echo $LEADER_INFO | jq -r '.leader_id // "unknown"')

if [ "$LEADER_ID" != "unknown" ] && [ "$LEADER_ID" != "null" ]; then
    echo ""
    echo -e "${GREEN}Current Leader: $LEADER_ID${NC}"
else
    echo ""
    echo -e "${YELLOW}No leader elected yet (this is normal, wait a moment)${NC}"
fi

echo ""
echo -e "${GREEN}Cluster is ready!${NC}"
echo ""
echo "Process IDs:"
echo "  Node 1: $NODE1_PID"
echo "  Node 2: $NODE2_PID"
echo "  Node 3: $NODE3_PID"
echo ""
echo "Logs are available in: $LOG_DIR/"
echo ""
echo "Example commands:"
echo -e "  ${YELLOW}# Check status${NC}"
echo "  curl http://localhost:9080/status | jq"
echo ""
echo -e "  ${YELLOW}# Store data${NC}"
echo "  curl -X PUT http://localhost:9080/kv -d '{\"key\":\"hello\",\"value\":\"world\"}'"
echo ""
echo -e "  ${YELLOW}# Retrieve data${NC}"
echo "  curl http://localhost:9080/kv/hello"
echo ""
echo -e "  ${YELLOW}# Stop cluster${NC}"
echo "  ./stop.sh"
echo ""
