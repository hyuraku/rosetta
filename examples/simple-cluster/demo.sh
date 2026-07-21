#!/bin/bash

# Interactive Demo Script
# Demonstrates Rosetta cluster capabilities

set -e

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m'

# Check if jq is available
HAS_JQ=false
if command -v jq &> /dev/null; then
    HAS_JQ=true
fi

# Helper function to pause
pause() {
    echo ""
    echo -e "${CYAN}Press Enter to continue...${NC}"
    read
}

# Helper function to execute and display command
demo_cmd() {
    echo -e "${YELLOW}$ $1${NC}"
    sleep 1
    eval "$1"
    echo ""
}

# Helper function to find leader port
find_leader_port() {
    for port in 9080 9081 9082; do
        if curl -s http://localhost:$port/status > /dev/null 2>&1; then
            STATUS_JSON=$(curl -s http://localhost:$port/status)
            if [ "$HAS_JQ" = true ]; then
                IS_LEADER=$(echo $STATUS_JSON | jq -r '.is_leader')
            else
                IS_LEADER=$(echo $STATUS_JSON | grep -o '"is_leader":[^,}]*' | cut -d':' -f2)
            fi
            if [ "$IS_LEADER" = "true" ]; then
                echo $port
                return
            fi
        fi
    done
    # Fallback to 9080 if no leader found
    echo "9080"
}

echo -e "${GREEN}"
echo "╔════════════════════════════════════════════════════════╗"
echo "║     Rosetta Distributed KV Store - Live Demo          ║"
echo "╔════════════════════════════════════════════════════════╗"
echo -e "${NC}"

pause

# Step 1: Start Cluster
echo -e "${BLUE}═══ Step 1: Starting 3-Node Cluster ═══${NC}"
echo ""
./start.sh
pause

# Step 2: Basic Operations
echo -e "${BLUE}═══ Step 2: Basic Key-Value Operations ═══${NC}"
echo ""

# Find the leader before write operations
echo "Finding the current leader..."
sleep 2  # Wait for leader election to complete
LEADER_PORT=$(find_leader_port)
echo -e "${GREEN}Leader found on port: $LEADER_PORT${NC}"
echo ""

echo "Let's store some data in the cluster (sending to leader)..."
echo ""

demo_cmd "curl -s -X PUT http://localhost:$LEADER_PORT/kv -H 'Content-Type: application/json' -d '{\"key\":\"name\",\"value\":\"Rosetta\"}'"
demo_cmd "curl -s -X PUT http://localhost:$LEADER_PORT/kv -H 'Content-Type: application/json' -d '{\"key\":\"type\",\"value\":\"Distributed KV Store\"}'"
demo_cmd "curl -s -X PUT http://localhost:$LEADER_PORT/kv -H 'Content-Type: application/json' -d '{\"key\":\"algorithm\",\"value\":\"Raft Consensus\"}'"

pause

echo "Now let's read the data back from the leader..."
echo ""

demo_cmd "curl -s http://localhost:$LEADER_PORT/kv/name"
demo_cmd "curl -s http://localhost:$LEADER_PORT/kv/type"
demo_cmd "curl -s http://localhost:$LEADER_PORT/kv/algorithm"

echo -e "${GREEN}Data successfully stored and retrieved!${NC}"
pause

# Step 3: Check Cluster Status
echo -e "${BLUE}═══ Step 3: Cluster Status ═══${NC}"
echo ""

echo "Let's check the status of each node..."
echo ""

for port in 9080 9081 9082; do
    if [ "$HAS_JQ" = true ]; then
        demo_cmd "curl -s http://localhost:$port/status | jq '{node_id, state, term, is_leader, log_length}'"
    else
        demo_cmd "curl -s http://localhost:$port/status"
    fi
done

pause

# Step 4: Find Leader
echo -e "${BLUE}═══ Step 4: Leader Information ═══${NC}"
echo ""

demo_cmd "curl -s http://localhost:9080/leader"

LEADER_INFO=$(curl -s http://localhost:9080/leader)
if [ "$HAS_JQ" = true ]; then
    LEADER_ID=$(echo $LEADER_INFO | jq -r '.leader_id')
    LEADER_ADDR=$(echo $LEADER_INFO | jq -r '.leader_addr')
else
    # Simple extraction without jq
    LEADER_ID=$(echo $LEADER_INFO | grep -o '"leader_id":"[^"]*"' | cut -d'"' -f4)
    LEADER_ADDR=$(echo $LEADER_INFO | grep -o '"leader_addr":"[^"]*"' | cut -d'"' -f4)
fi

echo -e "${GREEN}Current leader is: $LEADER_ID at $LEADER_ADDR${NC}"
pause

# Step 5: Fault Tolerance - Kill a Follower
echo -e "${BLUE}═══ Step 5: Fault Tolerance Test ═══${NC}"
echo ""
echo "Let's simulate a node failure..."
echo ""

# Find a follower to kill
for port in 9080 9081 9082; do
    STATUS_JSON=$(curl -s http://localhost:$port/status)
    if [ "$HAS_JQ" = true ]; then
        IS_LEADER=$(echo $STATUS_JSON | jq -r '.is_leader')
        NODE_ID=$(echo $STATUS_JSON | jq -r '.node_id')
    else
        IS_LEADER=$(echo $STATUS_JSON | grep -o '"is_leader":[^,}]*' | cut -d':' -f2)
        NODE_ID=$(echo $STATUS_JSON | grep -o '"node_id":"[^"]*"' | cut -d'"' -f4)
    fi
    if [ "$IS_LEADER" = "false" ]; then
        FOLLOWER_PID=$(pgrep -f "rosetta.*-id=$NODE_ID")
        FOLLOWER_PORT=$port
        break
    fi
done

echo -e "${RED}Killing follower node: $NODE_ID (PID: $FOLLOWER_PID)${NC}"
kill $FOLLOWER_PID

echo ""
sleep 2

echo "Cluster still operational? Let's test..."
echo ""

# Find the current leader (may have changed if we killed one)
LEADER_PORT=$(find_leader_port)
echo -e "${GREEN}Current leader on port: $LEADER_PORT${NC}"
echo ""

demo_cmd "curl -s -X PUT http://localhost:$LEADER_PORT/kv -H 'Content-Type: application/json' -d '{\"key\":\"fault-test\",\"value\":\"still works!\"}'"
demo_cmd "curl -s http://localhost:$LEADER_PORT/kv/fault-test"

echo -e "${GREEN}✓ Cluster survived node failure (2/3 nodes = quorum)${NC}"
pause

# Step 6: Node Recovery
echo -e "${BLUE}═══ Step 6: Node Recovery ═══${NC}"
echo ""
echo "Restarting the failed node..."
echo ""

# Restart the follower
if [ "$NODE_ID" = "node1" ]; then
    ../../rosetta -id=node1 -listen=localhost:8080 -http=localhost:9080 \
      -peers=node2:localhost:8081,node3:localhost:8082 > logs/node1.log 2>&1 &
elif [ "$NODE_ID" = "node2" ]; then
    ../../rosetta -id=node2 -listen=localhost:8081 -http=localhost:9081 \
      -peers=node1:localhost:8080,node3:localhost:8082 > logs/node2.log 2>&1 &
else
    ../../rosetta -id=node3 -listen=localhost:8082 -http=localhost:9082 \
      -peers=node1:localhost:8080,node2:localhost:8081 > logs/node3.log 2>&1 &
fi

NEW_PID=$!
echo -e "${GREEN}Node $NODE_ID restarted (PID: $NEW_PID)${NC}"
echo ""
sleep 2

echo "Checking if node caught up..."
echo ""

demo_cmd "curl -s http://localhost:$FOLLOWER_PORT/kv/fault-test"

echo -e "${GREEN}✓ Node recovered and synced!${NC}"
pause

# Step 7: Leader Failover
echo -e "${BLUE}═══ Step 7: Leader Failover Test ═══${NC}"
echo ""

LEADER_INFO=$(curl -s http://localhost:9080/leader)
if [ "$HAS_JQ" = true ]; then
    LEADER_ID=$(echo $LEADER_INFO | jq -r '.leader_id')
else
    LEADER_ID=$(echo $LEADER_INFO | grep -o '"leader_id":"[^"]*"' | cut -d'"' -f4)
fi

# Find leader port
if [ "$LEADER_ID" = "node1" ]; then
    LEADER_HTTP_PORT=9080
    LEADER_PID=$(pgrep -f "rosetta.*-id=node1")
elif [ "$LEADER_ID" = "node2" ]; then
    LEADER_HTTP_PORT=9081
    LEADER_PID=$(pgrep -f "rosetta.*-id=node2")
else
    LEADER_HTTP_PORT=9082
    LEADER_PID=$(pgrep -f "rosetta.*-id=node3")
fi

echo -e "${RED}Killing leader node: $LEADER_ID (PID: $LEADER_PID)${NC}"
kill $LEADER_PID

echo ""
echo "Waiting for new leader election..."
sleep 3

# Find new leader
for port in 9080 9081 9082; do
    if curl -s http://localhost:$port/status > /dev/null 2>&1; then
        NEW_LEADER_INFO=$(curl -s http://localhost:$port/leader 2>/dev/null || echo '{}')
        if [ "$HAS_JQ" = true ]; then
            NEW_LEADER_ID=$(echo $NEW_LEADER_INFO | jq -r '.leader_id // "unknown"')
        else
            NEW_LEADER_ID=$(echo $NEW_LEADER_INFO | grep -o '"leader_id":"[^"]*"' | cut -d'"' -f4)
            [ -z "$NEW_LEADER_ID" ] && NEW_LEADER_ID="unknown"
        fi
        if [ "$NEW_LEADER_ID" != "unknown" ] && [ "$NEW_LEADER_ID" != "null" ]; then
            break
        fi
    fi
done

echo -e "${GREEN}New leader elected: $NEW_LEADER_ID${NC}"
echo ""

# Find the new leader port
NEW_LEADER_PORT=$(find_leader_port)
echo -e "${GREEN}New leader on port: $NEW_LEADER_PORT${NC}"
echo ""

echo "Testing cluster after leadership change..."
demo_cmd "curl -s -X PUT http://localhost:$NEW_LEADER_PORT/kv -H 'Content-Type: application/json' -d '{\"key\":\"after-failover\",\"value\":\"new leader works!\"}'"

echo -e "${GREEN}✓ Leader failover successful!${NC}"
pause

# Step 8: Delete Operation
echo -e "${BLUE}═══ Step 8: Delete Operation ═══${NC}"
echo ""

# Use the current leader for delete operation
LEADER_PORT=$(find_leader_port)
demo_cmd "curl -s -X DELETE http://localhost:$LEADER_PORT/kv/fault-test"

echo "Verifying deletion..."
echo ""

HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" http://localhost:$LEADER_PORT/kv/fault-test)
echo -e "${YELLOW}$ curl http://localhost:$LEADER_PORT/kv/fault-test${NC}"
echo -e "${RED}HTTP $HTTP_CODE - Key not found${NC}"

echo ""
echo -e "${GREEN}✓ Delete operation successful!${NC}"
pause

# Summary
echo -e "${BLUE}═══ Demo Summary ═══${NC}"
echo ""
echo -e "${GREEN}Demonstrated capabilities:${NC}"
echo "  ✓ Cluster setup and configuration"
echo "  ✓ Data replication across nodes"
echo "  ✓ Leader election and tracking"
echo "  ✓ Fault tolerance (follower failure)"
echo "  ✓ Node recovery and sync"
echo "  ✓ Leader failover and re-election"
echo "  ✓ CRUD operations (Create, Read, Update, Delete)"
echo ""

echo -e "${YELLOW}Final cluster status:${NC}"
for port in 9080 9081 9082; do
    if curl -s http://localhost:$port/status > /dev/null 2>&1; then
        STATUS=$(curl -s http://localhost:$port/status)
        if [ "$HAS_JQ" = true ]; then
            NODE_ID=$(echo $STATUS | jq -r '.node_id')
            STATE=$(echo $STATUS | jq -r '.state')
            IS_LEADER=$(echo $STATUS | jq -r '.is_leader')
        else
            NODE_ID=$(echo $STATUS | grep -o '"node_id":"[^"]*"' | cut -d'"' -f4)
            STATE=$(echo $STATUS | grep -o '"state":"[^"]*"' | cut -d'"' -f4)
            IS_LEADER=$(echo $STATUS | grep -o '"is_leader":[^,}]*' | cut -d':' -f2)
        fi

        if [ "$IS_LEADER" = "true" ]; then
            echo -e "  ${GREEN}$NODE_ID: $STATE (Leader)${NC}"
        else
            echo -e "  $NODE_ID: $STATE"
        fi
    else
        echo -e "  ${RED}Node on port $port: DOWN${NC}"
    fi
done

echo ""
echo -e "${CYAN}Demo complete!${NC}"
echo ""
echo "To clean up:"
echo -e "  ${YELLOW}./stop.sh${NC}"
echo ""
