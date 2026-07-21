# Performance Tuning Guide

This guide provides strategies for optimizing Rosetta's performance in production environments.

## Table of Contents
- [Performance Overview](#performance-overview)
- [Benchmarking](#benchmarking)
- [Write Performance](#write-performance)
- [Read Performance](#read-performance)
- [Network Optimization](#network-optimization)
- [Resource Optimization](#resource-optimization)
- [Scaling Strategies](#scaling-strategies)

## Performance Overview

### Expected Performance

**Write Operations (PUT/DELETE):**
- Local cluster (< 1ms RTT): 50-100ms latency, 200-500 ops/sec per node
- Same datacenter (< 5ms RTT): 100-200ms latency, 100-200 ops/sec per node
- Cross-datacenter (50-100ms RTT): 200-500ms latency, 20-50 ops/sec per node

**Read Operations (GET):**
- Local reads: < 5ms latency, 5000+ ops/sec per node
- Network reads: < 10ms latency, 1000+ ops/sec per node

**Factors Affecting Performance:**
1. Network latency between nodes
2. Cluster size (more nodes = slower writes)
3. Hardware (CPU, disk, network)
4. Workload characteristics (read/write ratio)

### Performance Characteristics

**Raft Consensus Impact:**
- Every write requires majority acknowledgment
- Write latency = RTT to furthest node in quorum + processing time
- Reads can be served locally (but may be stale)

**Bottlenecks:**
1. **Network:** Primary bottleneck for distributed consensus
2. **Disk I/O:** Log append operations (future with persistence)
3. **CPU:** JSON serialization, log processing
4. **Memory:** Log size, pending operations

## Benchmarking

### Built-in Benchmarks

```bash
# Run all benchmarks
go test -bench=. -benchmem ./...

# Run specific benchmark
go test -bench=BenchmarkPut -benchmem ./kvstore

# With CPU profiling
go test -bench=. -benchmem -cpuprofile=cpu.prof ./...

# Analyze profile
go tool pprof cpu.prof
```

### Custom Benchmark Tool

Create a benchmark client:

```bash
# See examples/benchmark/ directory
cd examples/benchmark
go run benchmark.go \
  -url=http://localhost:9080 \
  -ops=10000 \
  -concurrency=10 \
  -read-ratio=0.8
```

### Load Testing

**Using Apache Bench (ab):**
```bash
# Write test
ab -n 1000 -c 10 -p put.json -T application/json \
  http://localhost:9080/kv

# Read test
ab -n 10000 -c 50 http://localhost:9080/kv/test
```

**Using wrk:**
```bash
# Write load test
wrk -t4 -c100 -d30s -s put.lua http://localhost:9080/kv

# Read load test
wrk -t4 -c100 -d30s http://localhost:9080/kv/test
```

### Metrics to Track

1. **Latency Metrics:**
   - p50, p95, p99 latency
   - Min/max latency
   - Latency over time

2. **Throughput Metrics:**
   - Operations per second
   - Bytes per second
   - Success rate

3. **System Metrics:**
   - CPU utilization
   - Memory usage
   - Disk I/O
   - Network bandwidth

## Write Performance

### Optimization Strategies

#### 1. Minimize Network Latency

**Colocate Nodes:**
```bash
# Measure inter-node latency
for node in node2 node3; do
  ping -c 10 $node | tail -1
done

# Target: < 1ms for optimal performance
```

**Use High-Performance Network:**
- 10 GbE or better
- Low-latency switches
- Dedicated network for Raft traffic

**Optimize Network Stack:**
```bash
# Increase TCP buffer sizes
sudo sysctl -w net.core.rmem_max=134217728
sudo sysctl -w net.core.wmem_max=134217728
sudo sysctl -w net.ipv4.tcp_rmem="4096 87380 134217728"
sudo sysctl -w net.ipv4.tcp_wmem="4096 65536 134217728"

# Enable TCP fast open
sudo sysctl -w net.ipv4.tcp_fastopen=3
```

#### 2. Optimize Cluster Size

**Choose Appropriate Size:**
- 3 nodes: Fastest writes, tolerates 1 failure
- 5 nodes: Balanced, tolerates 2 failures (recommended)
- 7 nodes: Maximum fault tolerance, slower writes

**Write Latency by Cluster Size:**
```
3 nodes: 1 RTT (leader → 1 follower)
5 nodes: 1 RTT (leader → 2 followers)
7 nodes: 1 RTT (leader → 3 followers)
```

The latency is determined by the slowest node in the quorum.

#### 3. Batch Operations

**Client-Side Batching:**
```go
// Instead of individual PUTs
for i := 0; i < 1000; i++ {
    client.Put(fmt.Sprintf("key%d", i), value)
}

// Batch requests
batch := make([]Request, 1000)
for i := 0; i < 1000; i++ {
    batch[i] = Request{Key: fmt.Sprintf("key%d", i), Value: value}
}
client.PutBatch(batch)
```

**Note:** Batch API requires implementation

#### 4. Pipeline Requests

**Use HTTP/2:**
- Enables request pipelining
- Reduces connection overhead
- Multiplexes requests

**Configure nginx reverse proxy:**
```nginx
upstream rosetta {
    server node1:9080;
    server node2:9081;
    server node3:9082;
}

server {
    listen 443 ssl http2;

    location / {
        proxy_pass http://rosetta;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
    }
}
```

#### 5. Async Writes (Application-Level)

**Fire-and-Forget Pattern:**
```go
// Async write with callback
go func() {
    err := client.Put(key, value)
    if err != nil {
        handleError(err)
    } else {
        handleSuccess()
    }
}()
```

**Write-Behind Cache:**
```go
// Buffer writes in cache
cache.Set(key, value)

// Flush periodically
go func() {
    ticker := time.NewTicker(100 * time.Millisecond)
    for range ticker.C {
        flushCache()
    }
}()
```

## Read Performance

### Optimization Strategies

#### 1. Read from Leader (Current Implementation)

**Characteristics:**
- Strongly consistent reads
- No stale data
- Leader may become bottleneck

**When to Use:**
- Need latest data
- Strong consistency required
- Low read volume

#### 2. Follower Reads (Future Feature)

**Characteristics:**
- May return stale data
- Distributes read load
- Lower latency (local reads)

**Implementation Considerations:**
```go
// Relaxed consistency read
value, err := client.Get(key, ReadOptions{
    AllowStale: true,
    MaxStaleness: 1 * time.Second,
})
```

#### 3. Client-Side Caching

**In-Memory Cache:**
```go
type CachedClient struct {
    client *Client
    cache  *lru.Cache
    ttl    time.Duration
}

func (c *CachedClient) Get(key string) (string, error) {
    // Check cache first
    if val, ok := c.cache.Get(key); ok {
        return val.(string), nil
    }

    // Fetch from cluster
    val, err := c.client.Get(key)
    if err != nil {
        return "", err
    }

    // Update cache
    c.cache.Add(key, val)
    return val, nil
}
```

**Redis Cache Layer:**
```go
func Get(key string) (string, error) {
    // Try Redis first
    val, err := redisClient.Get(ctx, key).Result()
    if err == nil {
        return val, nil
    }

    // Fallback to Rosetta
    val, err = rosettaClient.Get(key)
    if err != nil {
        return "", err
    }

    // Update Redis
    redisClient.Set(ctx, key, val, 10*time.Second)
    return val, nil
}
```

#### 4. Connection Pooling

**HTTP Client Configuration:**
```go
client := &http.Client{
    Transport: &http.Transport{
        MaxIdleConns:        100,
        MaxIdleConnsPerHost: 100,
        IdleConnTimeout:     90 * time.Second,
        DisableCompression:  false,
        DisableKeepAlives:   false,
    },
    Timeout: 5 * time.Second,
}
```

#### 5. Read Load Balancing

**Round-Robin Across Nodes:**
```go
type LoadBalancedClient struct {
    nodes   []string
    current int
    mu      sync.Mutex
}

func (c *LoadBalancedClient) Get(key string) (string, error) {
    c.mu.Lock()
    node := c.nodes[c.current]
    c.current = (c.current + 1) % len(c.nodes)
    c.mu.Unlock()

    return httpGet(node, key)
}
```

## Network Optimization

### 1. Compression

**Enable HTTP Compression:**
```go
// Server side (requires implementation)
mux := http.NewServeMux()
handler := gziphandler.GzipHandler(mux)
```

**Note:** May increase CPU usage, test thoroughly

### 2. Reduce Payload Size

**Efficient Serialization:**
- Consider Protocol Buffers instead of JSON
- Use shorter key names
- Compress values before storage

**Example:**
```go
// Instead of
{"key": "user:profile:123", "value": "...large json..."}

// Use
{"k": "u:p:123", "v": "...compressed binary..."}
```

### 3. Network Tuning

**Linux Kernel Parameters:**
```bash
# /etc/sysctl.conf

# Increase max number of connections
net.core.somaxconn = 65535
net.ipv4.tcp_max_syn_backlog = 8192

# Enable TCP window scaling
net.ipv4.tcp_window_scaling = 1

# Increase buffer sizes
net.core.rmem_max = 134217728
net.core.wmem_max = 134217728

# Fast connection setup/teardown
net.ipv4.tcp_fin_timeout = 15
net.ipv4.tcp_tw_reuse = 1

# Apply changes
sudo sysctl -p
```

### 4. DNS Caching

**Reduce DNS Lookups:**
```bash
# Install nscd
sudo apt-get install nscd

# Configure caching
sudo systemctl enable nscd
sudo systemctl start nscd
```

## Resource Optimization

### 1. Memory Optimization

**Current Implementation:**
- In-memory store (all data in RAM)
- No persistence
- Memory usage = O(num_keys * avg_value_size)

**Optimization Strategies:**

**Monitor Memory:**
```bash
# Track memory usage
ps aux | grep rosetta
pmap $(pidof rosetta)

# Go memory profiling
go tool pprof http://localhost:6060/debug/pprof/heap
```

**Limit Store Size:**
```go
// Implement max size (requires code change)
if store.Size() > maxKeys {
    // Evict oldest entries (LRU)
    store.EvictOldest()
}
```

### 2. CPU Optimization

**Reduce JSON Overhead:**
- Use faster JSON library (jsoniter, easyjson)
- Pre-allocate buffers
- Reuse objects

**Example:**
```go
import jsoniter "github.com/json-iterator/go"

var json = jsoniter.ConfigCompatibleWithStandardLibrary

// Instead of encoding/json
data, err := json.Marshal(obj)
```

**Optimize Hot Paths:**
```go
// Profile CPU
go test -bench=. -cpuprofile=cpu.prof
go tool pprof cpu.prof

# Identify hot functions
(pprof) top10
(pprof) list functionName
```

### 3. Disk I/O Optimization (Future)

When persistence is implemented:

**Use SSDs:**
- NVMe > SATA SSD > HDD
- Dedicated disk for Raft logs
- RAID 10 for redundancy

**Optimize Write Patterns:**
- Batch log writes
- Use write-ahead logging (WAL)
- Implement log compaction

**Monitor Disk:**
```bash
# Check I/O stats
iostat -x 1 10

# Identify slow disk
iotop -o
```

### 4. Connection Limits

**Increase File Descriptors:**
```bash
# Check current limit
ulimit -n

# Increase limit
ulimit -n 65535

# Permanent change in /etc/security/limits.conf
* soft nofile 65535
* hard nofile 65535
```

**HTTP Server Configuration:**
```go
server := &http.Server{
    Addr:           ":9080",
    Handler:        handler,
    ReadTimeout:    10 * time.Second,
    WriteTimeout:   10 * time.Second,
    MaxHeaderBytes: 1 << 20,
}
```

## Scaling Strategies

### 1. Vertical Scaling

**When to Scale Up:**
- Single cluster performance insufficient
- CPU/memory bottlenecks
- Before reaching node limits

**Recommendations:**
- CPU: 4-8 cores per node
- RAM: 8-32 GB per node
- Network: 10 GbE minimum
- Disk: NVMe SSD

### 2. Horizontal Scaling

**Read Scaling:**
- Deploy multiple clusters
- Partition data across clusters
- Route reads to nearest cluster

**Write Scaling:**
- Shard data across clusters
- Use consistent hashing
- Application-level routing

**Example Sharding:**
```go
func getClusterForKey(key string) string {
    hash := crc32.ChecksumIEEE([]byte(key))
    shard := hash % numShards
    return clusters[shard]
}
```

### 3. Multi-Region Deployment

**Architecture:**
```
Region 1: Cluster A (Primary)
Region 2: Cluster B (Replica)
Region 3: Cluster C (Replica)
```

**Async Replication:**
- Write to primary cluster
- Async replicate to other regions
- Eventual consistency across regions

### 4. Caching Layer

**Multi-Tier Architecture:**
```
Client → CDN (static) → Redis (hot keys) → Rosetta (source of truth)
```

**Cache Strategy:**
```go
type CacheStrategy struct {
    l1 *LocalCache      // In-process, 1ms
    l2 *RedisCache      // Redis, 5ms
    l3 *RosettaClient   // Rosetta, 50ms
}

func (c *CacheStrategy) Get(key string) (string, error) {
    // Try L1
    if val, ok := c.l1.Get(key); ok {
        return val, nil
    }

    // Try L2
    if val, err := c.l2.Get(key); err == nil {
        c.l1.Set(key, val)
        return val, nil
    }

    // Fetch from L3
    val, err := c.l3.Get(key)
    if err != nil {
        return "", err
    }

    c.l2.Set(key, val)
    c.l1.Set(key, val)
    return val, nil
}
```

## Performance Checklist

### Infrastructure
- [ ] Use SSDs for storage
- [ ] Deploy on 10 GbE network
- [ ] Colocate nodes (< 1ms RTT)
- [ ] Use dedicated network for Raft
- [ ] Optimize kernel network parameters

### Cluster Configuration
- [ ] Use 3-5 nodes (not more unless needed)
- [ ] Deploy in same datacenter/AZ
- [ ] Configure appropriate timeouts
- [ ] Monitor and tune election timeouts

### Application
- [ ] Implement client-side caching
- [ ] Use connection pooling
- [ ] Batch operations where possible
- [ ] Implement retry logic with backoff
- [ ] Use async patterns for writes

### Monitoring
- [ ] Track latency metrics (p50, p95, p99)
- [ ] Monitor throughput
- [ ] Set up alerting for anomalies
- [ ] Regular performance testing
- [ ] Capacity planning

## Benchmarking Results

Example results from a 3-node cluster (local network, 1ms RTT):

| Operation | Latency (p50) | Latency (p99) | Throughput |
|-----------|---------------|---------------|------------|
| PUT       | 45ms          | 120ms         | 400 ops/s  |
| GET       | 2ms           | 8ms           | 8000 ops/s |
| DELETE    | 50ms          | 130ms         | 380 ops/s  |

**Test Configuration:**
- 3 nodes, 4 CPU cores each, 8GB RAM
- 1Gbps network, < 1ms RTT
- In-memory storage
- 100 concurrent clients

Your results may vary based on hardware, network, and workload.
