# Troubleshooting Guide

This guide helps diagnose and resolve common issues with Rosetta.

## Table of Contents
- [Quick Diagnostics](#quick-diagnostics)
- [Common Issues](#common-issues)
- [Leader Election Problems](#leader-election-problems)
- [Replication Issues](#replication-issues)
- [Performance Problems](#performance-problems)
- [Network Issues](#network-issues)
- [Debug Tools](#debug-tools)

## Quick Diagnostics

### Check Cluster Health

```bash
# Check node status
for port in 9080 9081 9082; do
  echo "Node on port $port:"
  curl -s http://localhost:$port/status | jq
done

# Check leader
curl -s http://localhost:9080/leader | jq
```

### Verify Connectivity

```bash
# Check if Raft ports are listening
netstat -tuln | grep 8080
netstat -tuln | grep 8081
netstat -tuln | grep 8082

# Test connectivity between nodes
telnet <node-ip> 8080
```

### Check Logs

```bash
# View recent logs
sudo journalctl -u rosetta -n 100 --no-pager

# Follow logs in real-time
sudo journalctl -u rosetta -f

# Filter for errors
sudo journalctl -u rosetta | grep -i error
```

## Common Issues

### Issue 1: Node Won't Start

**Symptoms:**
- Service fails to start
- Immediate crash after startup

**Diagnosis:**
```bash
# Check service status
sudo systemctl status rosetta

# View startup logs
sudo journalctl -u rosetta -n 50

# Try manual start
./rosetta -id=node1 -listen=localhost:8080 -http=localhost:9080
```

**Common Causes & Solutions:**

1. **Port Already in Use**
   ```bash
   # Find process using port
   sudo lsof -i :8080
   sudo lsof -i :9080

   # Solution: Kill conflicting process or change port
   ```

2. **Invalid Peer Configuration**
   - Check peer addresses are correct
   - Ensure peer format is `nodeID:address:port`
   - Verify DNS resolution if using hostnames

3. **Permission Issues**
   ```bash
   # Check file permissions
   ls -la /usr/local/bin/rosetta
   ls -la /var/lib/rosetta

   # Fix permissions
   sudo chown rosetta:rosetta /var/lib/rosetta
   sudo chmod 755 /usr/local/bin/rosetta
   ```

### Issue 2: "Not Leader" Errors

**Symptoms:**
- PUT/DELETE requests return 503
- Error: "not leader"

**Diagnosis:**
```bash
# Find current leader
curl http://localhost:9080/leader

# Check if any node is leader
for port in 9080 9081 9082; do
  curl -s http://localhost:$port/status | jq '.is_leader'
done
```

**Solutions:**

1. **No Leader Elected**
   - Wait for election (usually < 1 second)
   - Check logs for election issues
   - Verify cluster has quorum

2. **Client Pointing to Wrong Node**
   ```bash
   # Redirect to leader
   LEADER=$(curl -s http://localhost:9080/leader | jq -r '.leader_addr')
   curl -X PUT http://$LEADER/kv -d '{"key":"test","value":"data"}'
   ```

3. **Leader Election Loops**
   - See [Leader Election Problems](#leader-election-problems)

### Issue 3: Data Inconsistency

**Symptoms:**
- Different values on different nodes
- Stale reads

**Diagnosis:**
```bash
# Compare values across nodes
KEY="test"
for port in 9080 9081 9082; do
  echo "Node $port:"
  curl -s http://localhost:$port/kv/$KEY
done

# Check log indices
for port in 9080 9081 9082; do
  echo "Node $port:"
  curl -s http://localhost:$port/status | jq '{commit_index, log_length}'
done
```

**Solutions:**

1. **Replication Lag**
   - Normal during high write load
   - Check network latency
   - Monitor commit_index progression

2. **Split Brain (Rare)**
   - Requires network partition
   - Check network connectivity
   - Verify firewall rules
   - Restart affected nodes

3. **In-Memory State**
   - Current implementation doesn't persist
   - Restart loses all data
   - Implement persistence for production

### Issue 4: High Latency

**Symptoms:**
- Slow write operations
- Timeouts on PUT/DELETE

**Diagnosis:**
```bash
# Measure latency
time curl -X PUT http://localhost:9080/kv -d '{"key":"test","value":"data"}'

# Check system load
top
iostat -x 1 5

# Check network latency
ping <peer-node>
```

**Solutions:**

See [Performance Problems](#performance-problems) section

### Issue 5: Cluster Unavailable

**Symptoms:**
- No operations succeed
- No leader elected
- All nodes return errors

**Diagnosis:**
```bash
# Count running nodes
for port in 9080 9081 9082; do
  curl -s http://localhost:$port/status && echo "Node $port: UP" || echo "Node $port: DOWN"
done

# Check quorum
# For 3-node cluster: need 2 nodes
# For 5-node cluster: need 3 nodes
```

**Solutions:**

1. **Lost Quorum**
   - Start enough nodes to form majority
   - For 3-node cluster: need 2 nodes running
   - For 5-node cluster: need 3 nodes running

2. **All Nodes Down**
   ```bash
   # Restart cluster from scratch
   # Note: This loses all data in current implementation
   sudo systemctl start rosetta@node1
   sudo systemctl start rosetta@node2
   sudo systemctl start rosetta@node3
   ```

## Leader Election Problems

### Continuous Elections (Flip-Flopping)

**Symptoms:**
- Term number increases rapidly
- No stable leader
- Logs show frequent elections

**Diagnosis:**
```bash
# Monitor term changes
watch -n 1 'curl -s http://localhost:9080/status | jq .term'

# Check election timeout
grep -i "election" /var/log/rosetta/rosetta.log
```

**Common Causes:**

1. **Network Instability**
   - High packet loss
   - Intermittent connectivity
   - Solution: Fix network issues

2. **Clock Skew**
   - Different system times on nodes
   ```bash
   # Check time on all nodes
   date

   # Sync time
   sudo ntpdate -u pool.ntp.org
   ```

3. **Overloaded Nodes**
   - High CPU/memory usage
   - Solution: Reduce load or scale up

4. **Election Timeout Too Low**
   - Network latency exceeds timeout
   - Requires code modification to increase timeouts

### Split Vote Scenario

**Symptoms:**
- Elections complete but no leader
- Multiple candidates in same term

**Diagnosis:**
```bash
# Check states during election
for port in 9080 9081 9082; do
  curl -s http://localhost:$port/status | jq '{node_id, state, term}'
done
```

**Solution:**
- Usually self-resolving within 1-2 election cycles
- Random timeouts prevent this
- If persistent: restart one node

## Replication Issues

### Follower Falling Behind

**Symptoms:**
- Increasing gap in commit_index
- Slow reads on follower

**Diagnosis:**
```bash
# Check replication lag
for port in 9080 9081 9082; do
  echo "Node $port:"
  curl -s http://localhost:$port/status | \
    jq '{commit_index, log_length, lag: (.log_length - .commit_index)}'
done
```

**Solutions:**

1. **Network Congestion**
   - Check bandwidth usage
   - Prioritize Raft traffic (QoS)

2. **Slow Follower**
   - Check system resources
   - May need more powerful hardware

3. **Log Compaction Needed**
   - Large log causes slowdown
   - Implement snapshotting (future feature)

### AppendEntries Failures

**Symptoms:**
- Logs show AppendEntries rejections
- Follower and leader logs diverge

**Diagnosis:**
```bash
# Check for consistency errors
sudo journalctl -u rosetta | grep -i "consistency\|appendentries.*false"
```

**Solutions:**

1. **Log Mismatch**
   - Normal during catch-up
   - Leader backtracks via NextIndex
   - Monitor until consistency restored

2. **Corrupted State**
   - Restart affected node
   - Node will sync from leader

## Performance Problems

### High Write Latency

**Target:** < 50ms for local cluster, < 200ms for geo-distributed

**Diagnosis:**
```bash
# Measure write latency
for i in {1..10}; do
  time curl -X PUT http://localhost:9080/kv \
    -d "{\"key\":\"bench$i\",\"value\":\"test\"}" \
    2>&1 | grep real
done
```

**Solutions:**

1. **Quorum Latency**
   - Inherent to Raft (must wait for majority)
   - Reduce network latency between nodes
   - Use faster network (10GbE)
   - Colocate nodes in same datacenter

2. **Disk I/O Bottleneck**
   - Use SSDs for log storage
   - Monitor disk I/O: `iostat -x 1`

3. **CPU Bottleneck**
   - Check CPU usage: `top`
   - Scale vertically (more cores)

4. **Too Many Nodes**
   - More nodes = slower writes
   - Use 3-5 nodes unless needed

### High Read Latency

**Target:** < 5ms

**Diagnosis:**
```bash
# Measure read latency
for i in {1..10}; do
  time curl http://localhost:9080/kv/test 2>&1 | grep real
done
```

**Solutions:**

1. **Read from Follower** (if stale reads acceptable)
   - Currently all reads go to leader
   - Implement follower reads (future feature)

2. **Add Caching Layer**
   - Use Redis/Memcached for hot keys
   - Cache in application layer

3. **Optimize Lookup**
   - Current in-memory map is O(1)
   - Check for lock contention

### Memory Usage Growth

**Diagnosis:**
```bash
# Monitor memory
free -h
ps aux | grep rosetta

# Check store size
curl http://localhost:9080/status | jq .log_length
```

**Solutions:**

1. **Log Growth**
   - Implement log compaction
   - Create snapshots (future feature)
   - Delete old entries

2. **Memory Leak**
   - Run with race detector: `go test -race ./...`
   - Profile memory: `go tool pprof`
   - Report issue on GitHub

## Network Issues

### Firewall Blocking Raft

**Symptoms:**
- Nodes can't communicate
- Timeout errors in logs

**Diagnosis:**
```bash
# Test connectivity
telnet <peer-ip> 8080

# Check firewall
sudo iptables -L -n
sudo firewall-cmd --list-all
```

**Solution:**
```bash
# Allow Raft ports
sudo firewall-cmd --permanent --add-port=8080-8082/tcp
sudo firewall-cmd --reload
```

### DNS Resolution Issues

**Symptoms:**
- "No such host" errors
- Intermittent connectivity

**Diagnosis:**
```bash
# Test DNS
nslookup node2.example.com
dig node2.example.com

# Check /etc/hosts
cat /etc/hosts
```

**Solution:**
```bash
# Add to /etc/hosts
echo "10.0.1.11 node2" | sudo tee -a /etc/hosts
```

## Debug Tools

### Enable Verbose Logging

Modify systemd service to add debug flags (requires code changes):
```ini
ExecStart=/usr/local/bin/rosetta \
  -id=node1 \
  -listen=localhost:8080 \
  -http=localhost:9080 \
  -peers=node2:localhost:8081 \
  -debug=true \
  -log-level=debug
```

### Raft State Dump

```bash
# Get detailed state
curl http://localhost:9080/status | jq

# Monitor state changes
watch -n 1 'curl -s http://localhost:9080/status | jq'
```

### Network Packet Capture

```bash
# Capture Raft traffic
sudo tcpdump -i any -n port 8080 -w raft.pcap

# Analyze with wireshark
wireshark raft.pcap
```

### Performance Profiling

```bash
# CPU profile (requires code changes to expose pprof)
go tool pprof http://localhost:6060/debug/pprof/profile

# Memory profile
go tool pprof http://localhost:6060/debug/pprof/heap

# Goroutine profile
go tool pprof http://localhost:6060/debug/pprof/goroutine
```

### Testing Failure Scenarios

```bash
# Simulate network partition
sudo iptables -A INPUT -s <peer-ip> -j DROP
sudo iptables -A OUTPUT -d <peer-ip> -j DROP

# Remove partition
sudo iptables -D INPUT -s <peer-ip> -j DROP
sudo iptables -D OUTPUT -d <peer-ip> -j DROP

# Simulate node failure
sudo systemctl stop rosetta

# Simulate slow network
sudo tc qdisc add dev eth0 root netem delay 100ms
sudo tc qdisc del dev eth0 root
```

## Getting Help

If you can't resolve the issue:

1. **Gather Information:**
   - Node status from all nodes
   - Recent logs (last 500 lines)
   - Network topology
   - Cluster configuration
   - Steps to reproduce

2. **Check Documentation:**
   - [README.md](../README.md)
   - [API Documentation](api.md)
   - [Deployment Guide](deployment.md)

3. **Open an Issue:**
   - Go to GitHub Issues
   - Provide all gathered information
   - Include Rosetta version: `./rosetta -version`

4. **Community Support:**
   - Check existing GitHub issues
   - Search for similar problems
   - Contribute solutions back

## Preventive Measures

1. **Monitoring:**
   - Set up health checks
   - Monitor key metrics
   - Alert on anomalies

2. **Testing:**
   - Run integration tests regularly
   - Simulate failures in staging
   - Load test before production

3. **Backups:**
   - Regular state backups
   - Document recovery procedures
   - Test restore process

4. **Updates:**
   - Keep software updated
   - Review changelogs
   - Test upgrades in staging
