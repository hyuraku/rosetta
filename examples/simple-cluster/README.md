# Simple Cluster Example

This example demonstrates how to set up and run a basic 3-node Rosetta cluster on your local machine.

## Overview

This setup will run three Rosetta nodes:
- **Node 1**: Raft on :8080, HTTP API on :9080
- **Node 2**: Raft on :8081, HTTP API on :9081
- **Node 3**: Raft on :8082, HTTP API on :9082

## Prerequisites

- Go 1.19 or later
- Rosetta binary built (`go build -o rosetta ../../main.go`)

## Quick Start

### Option 1: Using the Start Script

```bash
# Make script executable
chmod +x start.sh

# Start the cluster
./start.sh

# The script will:
# - Start 3 nodes in the background
# - Wait for leader election
# - Display cluster status
```

### Option 2: Manual Start

**Terminal 1 - Start Node 1:**
```bash
../../rosetta \
  -id=node1 \
  -listen=localhost:8080 \
  -http=localhost:9080 \
  -peers=node2:localhost:8081,node3:localhost:8082
```

**Terminal 2 - Start Node 2:**
```bash
../../rosetta \
  -id=node2 \
  -listen=localhost:8081 \
  -http=localhost:9081 \
  -peers=node1:localhost:8080,node3:localhost:8082
```

**Terminal 3 - Start Node 3:**
```bash
../../rosetta \
  -id=node3 \
  -listen=localhost:8082 \
  -http=localhost:9082 \
  -peers=node1:localhost:8080,node2:localhost:8081
```

## Testing the Cluster

### 1. Check Cluster Status

```bash
# Check each node's status
curl http://localhost:9080/status | jq
curl http://localhost:9081/status | jq
curl http://localhost:9082/status | jq

# Find the current leader
curl http://localhost:9080/leader | jq
```

### 2. Store Data

```bash
# Write to the cluster
curl -X PUT http://localhost:9080/kv \
  -H "Content-Type: application/json" \
  -d '{"key":"hello","value":"world"}'

curl -X PUT http://localhost:9080/kv \
  -H "Content-Type: application/json" \
  -d '{"key":"name","value":"rosetta"}'
```

### 3. Read Data

```bash
# Read from any node
curl http://localhost:9080/kv/hello
curl http://localhost:9081/kv/name
curl http://localhost:9082/kv/hello
```

### 4. Delete Data

```bash
# Delete a key
curl -X DELETE http://localhost:9080/kv/hello

# Verify deletion
curl http://localhost:9080/kv/hello
# Should return 404
```

## Testing Fault Tolerance

### Simulate Node Failure

1. **Kill a follower node:**
   ```bash
   # Find the node processes
   ps aux | grep rosetta

   # Kill a follower (not the leader)
   kill <pid-of-node2>
   ```

2. **Verify cluster still works:**
   ```bash
   # Should still succeed (2/3 nodes available)
   curl -X PUT http://localhost:9080/kv \
     -d '{"key":"test","value":"fault-tolerance"}'
   ```

3. **Restart the node:**
   ```bash
   ../../rosetta \
     -id=node2 \
     -listen=localhost:8081 \
     -http=localhost:9081 \
     -peers=node1:localhost:8080,node3:localhost:8082
   ```

4. **Verify node catches up:**
   ```bash
   # Check that node2 has the data
   curl http://localhost:9081/kv/test
   ```

### Simulate Leader Failure

1. **Find and kill the leader:**
   ```bash
   # Find leader
   LEADER=$(curl -s http://localhost:9080/leader | jq -r '.leader_id')
   echo "Leader is: $LEADER"

   # Kill the leader node
   kill <pid-of-leader>
   ```

2. **Wait for new election:**
   ```bash
   # Should complete in < 1 second
   sleep 2

   # Check new leader
   curl http://localhost:9080/leader | jq
   ```

3. **Verify cluster operation:**
   ```bash
   # Should work with new leader
   curl -X PUT http://localhost:9080/kv \
     -d '{"key":"after-failover","value":"success"}'
   ```

## Interactive Demo Script

Use the provided demo script for an interactive demonstration:

```bash
# Make executable
chmod +x demo.sh

# Run demo
./demo.sh
```

The demo script will:
1. Start the cluster
2. Demonstrate basic operations
3. Simulate failures
4. Show recovery
5. Clean up

## Cleanup

### Stop All Nodes

```bash
# Using the stop script
./stop.sh

# Or manually
pkill -f rosetta
```

### Verify Cleanup

```bash
# Check no processes running
ps aux | grep rosetta

# Check ports are free
lsof -i :8080
lsof -i :9080
```

## Troubleshooting

### Issue: Ports Already in Use

```bash
# Find what's using the port
lsof -i :8080

# Kill the process or choose different ports
../../rosetta -id=node1 -listen=localhost:8090 -http=localhost:9090 ...
```

### Issue: Nodes Can't Connect

- Verify firewall allows localhost connections
- Check all peer addresses are correct
- Ensure no typos in node IDs

### Issue: No Leader Elected

- Wait a few seconds (election timeout)
- Check logs for errors
- Verify at least 2/3 nodes are running

### Issue: "Not Leader" Errors

- Find current leader: `curl http://localhost:9080/leader`
- Send writes to leader node
- Or wait for automatic leader election

## Next Steps

- Explore [API Documentation](../../docs/api.md)
- Learn about [Deployment](../../docs/deployment.md)
- Read [Performance Tuning](../../docs/performance.md)
- Try the [Benchmark Example](../benchmark/)

## Architecture Diagram

```
┌─────────────────────────────────────────────────────┐
│                    Client                           │
└─────────────────────────────────────────────────────┘
                          │
                          ▼
        ┌─────────────────────────────────────┐
        │         HTTP API Layer              │
        │  :9080      :9081      :9082        │
        └─────────────────────────────────────┘
                          │
                          ▼
        ┌─────────────────────────────────────┐
        │         KV Store Layer              │
        │  (State Machine)                    │
        └─────────────────────────────────────┘
                          │
                          ▼
        ┌─────────────────────────────────────┐
        │         Raft Layer                  │
        │  :8080      :8081      :8082        │
        │                                     │
        │  ┌───────┐  ┌───────┐  ┌───────┐  │
        │  │ Node1 │──│ Node2 │──│ Node3 │  │
        │  │Leader │  │Fllwr  │  │Fllwr  │  │
        │  └───────┘  └───────┘  └───────┘  │
        └─────────────────────────────────────┘
```

## Learning Objectives

By completing this example, you will understand:

1. **Cluster Setup**: How to configure and start multiple Rosetta nodes
2. **Leader Election**: How Raft automatically elects a leader
3. **Replication**: How data is replicated across nodes
4. **Fault Tolerance**: How the cluster handles node failures
5. **Consistency**: How Raft ensures strong consistency
6. **API Usage**: How to interact with the cluster via HTTP API
