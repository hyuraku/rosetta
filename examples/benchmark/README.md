# Benchmark Example

This example provides tools for performance testing and benchmarking Rosetta clusters.

## Overview

The benchmark tool allows you to:
- Test write performance (PUT operations)
- Test read performance (GET operations)
- Test mixed workloads
- Measure latency distributions (p50, p95, p99)
- Measure throughput
- Test with various concurrency levels

## Prerequisites

- Go 1.19 or later
- Running Rosetta cluster (see [simple-cluster example](../simple-cluster/))

## Quick Start

### 1. Start a Cluster

```bash
cd ../simple-cluster
./start.sh
cd ../benchmark
```

Before benchmarking, verify all nodes are up and a leader is elected:

```bash
for port in 9080 9081 9082; do
  curl -s http://localhost:$port/status | jq '{node_id, term, is_leader, log_size}'
done

curl -s http://localhost:9080/leader | jq
```

### 2. Run Basic Benchmark

```bash
# Build the benchmark tool
go build -o benchmark benchmark.go

# Run default benchmark (mixed workload)
./benchmark -url=http://localhost:9080

# Run write-heavy benchmark
./benchmark -url=http://localhost:9080 -ops=10000 -concurrency=10 -read-ratio=0.2

# Run read-heavy benchmark
./benchmark -url=http://localhost:9080 -ops=10000 -concurrency=50 -read-ratio=0.8
```

## Command Line Options

```
  -url string
        Rosetta node URL (default "http://localhost:9080")

  -ops int
        Total number of operations (default 1000)

  -concurrency int
        Number of concurrent clients (default 10)

  -read-ratio float
        Ratio of read operations (0.0 to 1.0) (default 0.5)

  -key-size int
        Size of keys in bytes (default 16)

  -value-size int
        Size of values in bytes (default 100)

  -duration int
        Benchmark duration in seconds (0 = use ops count) (default 0)

  -report-interval int
        Progress report interval in ops (default 1000)
```

## Benchmark Scenarios

### Scenario 1: Write Performance

Test maximum write throughput:

```bash
./benchmark \
  -url=http://localhost:9080 \
  -ops=5000 \
  -concurrency=10 \
  -read-ratio=0.0 \
  -value-size=1024
```

**Expected Output:**
```
=== Rosetta Benchmark ===
URL: http://localhost:9080
Operations: 5000
Concurrency: 10
Read Ratio: 0.00
Key Size: 16 bytes
Value Size: 1024 bytes

Running benchmark...
Progress: 1000/5000 ops (20.0%) - Elapsed: 2.1s
Progress: 2000/5000 ops (40.0%) - Elapsed: 4.3s
Progress: 3000/5000 ops (60.0%) - Elapsed: 6.4s
Progress: 4000/5000 ops (80.0%) - Elapsed: 8.6s
Progress: 5000/5000 ops (100.0%) - Elapsed: 10.7s

=== Results ===
Total Operations: 5000
Successful: 5000
Failed: 0
Duration: 10.7s
Throughput: 467 ops/sec

Write Latency:
  Min: 15.2ms
  Max: 234.5ms
  Mean: 45.3ms
  P50: 42.1ms
  P95: 78.4ms
  P99: 125.6ms
```

### Scenario 2: Read Performance

Test read throughput:

```bash
# First, populate data
./benchmark \
  -url=http://localhost:9080 \
  -ops=1000 \
  -concurrency=1 \
  -read-ratio=0.0

# Then run read benchmark
./benchmark \
  -url=http://localhost:9080 \
  -ops=10000 \
  -concurrency=50 \
  -read-ratio=1.0
```

### Scenario 3: Mixed Workload

Realistic workload with 80% reads:

```bash
./benchmark \
  -url=http://localhost:9080 \
  -ops=10000 \
  -concurrency=20 \
  -read-ratio=0.8
```

### Scenario 4: Time-Based Benchmark

Run for fixed duration:

```bash
./benchmark \
  -url=http://localhost:9080 \
  -duration=60 \
  -concurrency=10 \
  -read-ratio=0.5
```

### Scenario 5: Large Values

Test with larger payloads:

```bash
./benchmark \
  -url=http://localhost:9080 \
  -ops=1000 \
  -concurrency=5 \
  -value-size=10240 \
  -read-ratio=0.3
```

## Using Apache Bench (ab)

For quick HTTP benchmarking:

### Write Benchmark

```bash
# Create request body
cat > put.json <<EOF
{"key":"bench-key","value":"bench-value-123456789"}
EOF

# Run benchmark
ab -n 1000 -c 10 \
  -p put.json \
  -T application/json \
  http://localhost:9080/kv

# Results show:
# - Requests per second
# - Time per request
# - Connection times (min, mean, max)
# - Percentage served within time
```

### Read Benchmark

```bash
# First insert a key
curl -X PUT http://localhost:9080/kv \
  -d '{"key":"test","value":"benchmark"}'

# Benchmark reads
ab -n 10000 -c 50 http://localhost:9080/kv/test
```

## Using wrk

For advanced HTTP benchmarking:

### Install wrk

```bash
# macOS
brew install wrk

# Linux
git clone https://github.com/wg/wrk.git
cd wrk
make
sudo cp wrk /usr/local/bin/
```

### Write Benchmark Script

Create `put.lua`:

```lua
wrk.method = "PUT"
wrk.headers["Content-Type"] = "application/json"

request = function()
  local id = math.random(1, 100000)
  local body = string.format('{"key":"key-%d","value":"value-%d"}', id, id)
  return wrk.format(nil, "/kv", nil, body)
end
```

Run:
```bash
wrk -t4 -c100 -d30s -s put.lua http://localhost:9080
```

### Read Benchmark

```bash
wrk -t4 -c100 -d30s http://localhost:9080/kv/test
```

## Interpreting Results

### Latency

- **p50 (median)**: Typical request latency
- **p95**: 95% of requests complete within this time
- **p99**: 99% of requests complete within this time

**Good Values:**
- Writes: p50 < 50ms, p99 < 200ms (local cluster)
- Reads: p50 < 5ms, p99 < 20ms

**If higher:**
- Check network latency between nodes
- Check CPU/memory usage
- Consider cluster size (fewer nodes = faster writes)

### Throughput

**Expected Throughput (3-node local cluster):**
- Writes: 200-500 ops/sec
- Reads: 5000+ ops/sec

**Factors Affecting Throughput:**
1. Cluster size (more nodes = slower writes)
2. Network latency
3. Value size
4. Concurrency level

**Why cluster size matters:**

Every write must be acknowledged by a majority (quorum) before it commits:

```
3 nodes: leader + 1 follower ack (tolerates 1 failure)
5 nodes: leader + 2 follower acks (tolerates 2 failures)
7 nodes: leader + 3 follower acks (tolerates 3 failures)
```

Write latency is roughly one round trip to the slowest follower in the
quorum, so adding nodes improves fault tolerance but never speeds up writes.
Reads are cheaper: when the leader has recently confirmed its leadership via
heartbeats, GET requests are served directly from its local state without
going through Raft consensus.

### Error Rate

- **0% errors**: Healthy cluster
- **Low errors (<1%)**: May be transient (retries should succeed)
- **High errors (>5%)**: Investigate cluster health

**Common Errors:**
- `503 Not Leader`: Redirect to leader or wait for election. The response
  includes an `X-Raft-Leader` header, and `GET /leader` returns
  `{"leader":"<node-id>"}`.
- `Timeout`: Increase timeout or reduce load
- `Connection Refused`: Node is down

## Performance Tuning

### Based on Results

**If Write Latency is High:**
1. Reduce network latency (colocate nodes)
2. Use fewer nodes (3 instead of 5)
3. Batch operations (if supported)
4. Check leader CPU usage

**If Read Latency is High:**
1. Enable client-side caching
2. Use connection pooling
3. Make sure reads go to the leader (followers return `503 Not Leader`;
   follower reads are not supported yet)
4. Check node CPU usage

**If Throughput is Low:**
1. Increase concurrency
2. Optimize network (10GbE, low latency)
3. Use pipelining (HTTP/2)
4. Profile and optimize hot paths

### System-Level Optimizations

```bash
# Before benchmarking, optimize system
sudo sysctl -w net.core.somaxconn=65535
sudo sysctl -w net.ipv4.tcp_max_syn_backlog=8192
sudo sysctl -w net.core.rmem_max=134217728
sudo sysctl -w net.core.wmem_max=134217728

# Increase file descriptors
ulimit -n 65535
```

## Comparing Configurations

### Test Different Cluster Sizes

```bash
# 3-node cluster
./benchmark -url=http://localhost:9080 -ops=5000 -concurrency=10

# 5-node cluster (after starting 2 more nodes)
./benchmark -url=http://localhost:9080 -ops=5000 -concurrency=10

# Compare results
```

### Test Network Impact

```bash
# Local cluster (< 1ms RTT)
./benchmark -url=http://localhost:9080 -ops=5000

# Add artificial latency
sudo tc qdisc add dev lo root netem delay 10ms

# Rerun benchmark
./benchmark -url=http://localhost:9080 -ops=5000

# Remove latency
sudo tc qdisc del dev lo root
```

## Continuous Performance Testing

### Integration with CI/CD

Create `benchmark.sh`:

```bash
#!/bin/bash
set -e

# Start cluster
cd ../simple-cluster
./start.sh
sleep 3

# Run benchmark
cd ../benchmark
./benchmark -ops=1000 -concurrency=10 > results.txt

# Check for regressions
THROUGHPUT=$(grep "Throughput:" results.txt | awk '{print $2}')
MIN_THROUGHPUT=100

if (( $(echo "$THROUGHPUT < $MIN_THROUGHPUT" | bc -l) )); then
  echo "Performance regression detected!"
  exit 1
fi

echo "Performance OK: $THROUGHPUT ops/sec"

# Cleanup
cd ../simple-cluster
./stop.sh
```

### Baseline Tracking

```bash
# Establish baseline
./benchmark -ops=5000 > baseline.txt

# Compare after changes
./benchmark -ops=5000 > current.txt
diff baseline.txt current.txt
```

## Advanced Scenarios

### Testing Leader Failover Impact

```bash
# Start benchmark in background
./benchmark -ops=10000 -concurrency=10 > results.txt &
BENCH_PID=$!

# Wait a bit
sleep 5

# Kill leader
LEADER=$(curl -s http://localhost:9080/leader | jq -r '.leader')
pkill -f "rosetta.*-id=$LEADER"

# Wait for completion
wait $BENCH_PID

# Analyze results (look for error spike during failover)
cat results.txt
```

**Expected behavior:** writes fail for roughly one election timeout
(150-300ms, randomized per node) until a new leader is elected. Election
and heartbeat timeouts are currently hardcoded in `raft/state.go`, so they
cannot be tuned via flags or the config file.

### Testing Network Partition

```bash
# Partition network
sudo iptables -A INPUT -s <node2-ip> -j DROP
sudo iptables -A OUTPUT -d <node2-ip> -j DROP

# Run benchmark (should fail or show high error rate)
./benchmark -ops=1000

# Remove partition
sudo iptables -D INPUT -s <node2-ip> -j DROP
sudo iptables -D OUTPUT -d <node2-ip> -j DROP
```

## Resources

- [API Reference](../../docs/api.md)
- [Persistence Details](../../docs/persistence.md)
- [Simple Cluster Example](../simple-cluster/)

## Sample Benchmark Tool

See `benchmark.go` for the complete implementation.

Key features:
- Concurrent worker goroutines
- Latency histogram tracking
- Progress reporting
- Comprehensive results output
- Support for various workload patterns
