# Deployment Guide

This guide covers production deployment strategies for Rosetta, a distributed key-value store based on the Raft consensus algorithm.

## Table of Contents
- [Prerequisites](#prerequisites)
- [Cluster Sizing](#cluster-sizing)
- [Deployment Architectures](#deployment-architectures)
- [Configuration](#configuration)
- [Production Setup](#production-setup)
- [Monitoring](#monitoring)
- [Backup and Recovery](#backup-and-recovery)
- [Upgrades](#upgrades)

## Prerequisites

### System Requirements

**Minimum per node:**
- CPU: 2 cores
- RAM: 2GB
- Disk: 10GB SSD (for logs and state)
- Network: 100Mbps

**Recommended for production:**
- CPU: 4+ cores
- RAM: 8GB+
- Disk: 50GB+ SSD
- Network: 1Gbps

### Software Requirements
- Go 1.19 or later (for building)
- Linux/macOS operating system
- Systemd (for service management on Linux)

## Cluster Sizing

### Fault Tolerance

Raft requires a majority (quorum) of nodes to be available. Choose cluster size based on fault tolerance needs:

| Cluster Size | Tolerated Failures | Notes |
|--------------|-------------------|-------|
| 1 | 0 | Development only |
| 3 | 1 | Minimum for production |
| 5 | 2 | Recommended for production |
| 7 | 3 | High availability environments |

**Key Principles:**
- Always use odd numbers (3, 5, 7) to maximize fault tolerance
- More nodes = better fault tolerance but slower writes (more replicas)
- 5 nodes is the sweet spot for most use cases

### Network Topology

**Single Datacenter:**
- Deploy all nodes in the same datacenter
- Low latency between nodes (< 5ms)
- Suitable for most use cases

**Multi-Datacenter (Advanced):**
- Requires careful planning
- Place majority of nodes in primary DC
- Expect higher latency for writes
- Consider using regional clusters with cross-cluster replication

## Deployment Architectures

### Architecture 1: Simple 3-Node Cluster

```
┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│   Node 1    │────▶│   Node 2    │────▶│   Node 3    │
│  Leader     │     │  Follower   │     │  Follower   │
│ :8080/:9080 │     │ :8081/:9081 │     │ :8082/:9082 │
└─────────────┘     └─────────────┘     └─────────────┘
       │                   │                   │
       └───────────────────┴───────────────────┘
                    Client Traffic
```

**Use Case:** Development, small production deployments

### Architecture 2: Load Balanced with Proxy

```
                  ┌──────────────────┐
                  │  Load Balancer   │
                  │  (nginx/HAProxy) │
                  └──────────────────┘
                           │
        ┌──────────────────┼──────────────────┐
        │                  │                  │
  ┌─────────┐        ┌─────────┐        ┌─────────┐
  │ Node 1  │        │ Node 2  │        │ Node 3  │
  │ Leader  │◀──────▶│Follower │◀──────▶│Follower │
  └─────────┘        └─────────┘        └─────────┘
```

**Features:**
- Load balancer handles TLS termination
- Automatic leader detection and routing
- Health checks and failover

### Architecture 3: 5-Node Multi-AZ Deployment

```
┌─────────── Availability Zone 1 ───────────┐
│  ┌─────────┐              ┌─────────┐     │
│  │ Node 1  │◀────────────▶│ Node 2  │     │
│  └─────────┘              └─────────┘     │
└────────────────────────────────────────────┘

┌─────────── Availability Zone 2 ───────────┐
│  ┌─────────┐              ┌─────────┐     │
│  │ Node 3  │◀────────────▶│ Node 4  │     │
│  └─────────┘              └─────────┘     │
└────────────────────────────────────────────┘

┌─────────── Availability Zone 3 ───────────┐
│              ┌─────────┐                   │
│              │ Node 5  │                   │
│              └─────────┘                   │
└────────────────────────────────────────────┘
```

**Use Case:** High availability production deployments

## Configuration

### Command Line Flags

```bash
./rosetta \
  -id=<node-id> \           # Unique node identifier
  -listen=<raft-addr> \     # Raft RPC address (e.g., localhost:8080)
  -http=<http-addr> \       # HTTP API address (e.g., localhost:9080)
  -peers=<peer-list>        # Comma-separated peer list
```

### Example: 3-Node Cluster

**Node 1:**
```bash
./rosetta \
  -id=node1 \
  -listen=10.0.1.10:8080 \
  -http=10.0.1.10:9080 \
  -peers=node2:10.0.1.11:8080,node3:10.0.1.12:8080
```

**Node 2:**
```bash
./rosetta \
  -id=node2 \
  -listen=10.0.1.11:8080 \
  -http=10.0.1.11:9080 \
  -peers=node1:10.0.1.10:8080,node3:10.0.1.12:8080
```

**Node 3:**
```bash
./rosetta \
  -id=node3 \
  -listen=10.0.1.12:8080 \
  -http=10.0.1.12:9080 \
  -peers=node1:10.0.1.10:8080,node2:10.0.1.11:8080
```

## Production Setup

### 1. Build for Production

```bash
# Build with optimizations
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -ldflags="-s -w" \
  -o rosetta \
  main.go

# Verify binary
./rosetta -h
```

### 2. Create System User

```bash
sudo useradd -r -s /bin/false rosetta
sudo mkdir -p /var/lib/rosetta /var/log/rosetta
sudo chown rosetta:rosetta /var/lib/rosetta /var/log/rosetta
```

### 3. Install Binary

```bash
sudo cp rosetta /usr/local/bin/
sudo chmod +x /usr/local/bin/rosetta
```

### 4. Create Systemd Service

Create `/etc/systemd/system/rosetta.service`:

```ini
[Unit]
Description=Rosetta Distributed KV Store
After=network.target

[Service]
Type=simple
User=rosetta
Group=rosetta
ExecStart=/usr/local/bin/rosetta \
  -id=node1 \
  -listen=10.0.1.10:8080 \
  -http=10.0.1.10:9080 \
  -peers=node2:10.0.1.11:8080,node3:10.0.1.12:8080

Restart=on-failure
RestartSec=5
StandardOutput=journal
StandardError=journal

# Security hardening
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/rosetta /var/log/rosetta

[Install]
WantedBy=multi-user.target
```

### 5. Start and Enable Service

```bash
sudo systemctl daemon-reload
sudo systemctl enable rosetta
sudo systemctl start rosetta
sudo systemctl status rosetta
```

### 6. Configure Firewall

```bash
# Allow Raft RPC port (8080)
sudo firewall-cmd --permanent --add-port=8080/tcp

# Allow HTTP API port (9080)
sudo firewall-cmd --permanent --add-port=9080/tcp

# Reload firewall
sudo firewall-cmd --reload
```

## Monitoring

### Health Check Endpoints

**Node Status:**
```bash
curl http://localhost:9080/status
```

**Leader Information:**
```bash
curl http://localhost:9080/leader
```

### Key Metrics to Monitor

1. **Raft Metrics:**
   - Current term
   - Leader election frequency
   - Log replication lag
   - Commit index progression

2. **System Metrics:**
   - CPU usage
   - Memory usage
   - Disk I/O
   - Network throughput

3. **Application Metrics:**
   - Request latency (p50, p95, p99)
   - Request rate
   - Error rate
   - Store size

### Log Monitoring

```bash
# View logs
sudo journalctl -u rosetta -f

# Filter by time
sudo journalctl -u rosetta --since "1 hour ago"

# Export logs
sudo journalctl -u rosetta > /var/log/rosetta/export.log
```

## Backup and Recovery

### State Backup

The Raft state includes:
- Persistent state (term, votedFor)
- Log entries
- Committed data

**Current Implementation:**
- State is in-memory (not persistent across restarts)
- For production, implement persistent storage

**Planned Features:**
- Snapshot creation and restoration
- Log compaction
- Point-in-time recovery

### Disaster Recovery

**Scenario 1: Single Node Failure**
- Cluster continues operating with remaining nodes
- Replace failed node and rejoin cluster

**Scenario 2: Majority Failure**
- Cluster unavailable (no quorum)
- Restore from backup or reinitialize cluster

**Scenario 3: Complete Cluster Loss**
- Requires backup restoration
- May lose recent uncommitted data

## Upgrades

### Rolling Upgrade Process

For a 3-node cluster:

1. **Upgrade Node 3 (Follower):**
   ```bash
   sudo systemctl stop rosetta
   sudo cp new-rosetta /usr/local/bin/rosetta
   sudo systemctl start rosetta
   # Verify node rejoins cluster
   curl http://node3:9080/status
   ```

2. **Upgrade Node 2 (Follower):**
   ```bash
   sudo systemctl stop rosetta
   sudo cp new-rosetta /usr/local/bin/rosetta
   sudo systemctl start rosetta
   curl http://node2:9080/status
   ```

3. **Transfer Leadership and Upgrade Node 1:**
   ```bash
   # Leadership will transfer automatically on shutdown
   sudo systemctl stop rosetta
   sudo cp new-rosetta /usr/local/bin/rosetta
   sudo systemctl start rosetta
   curl http://node1:9080/status
   ```

### Upgrade Checklist

- [ ] Test upgrade in staging environment
- [ ] Backup current state
- [ ] Review changelog for breaking changes
- [ ] Verify cluster health before upgrade
- [ ] Upgrade one node at a time
- [ ] Monitor logs during upgrade
- [ ] Verify cluster health after each node
- [ ] Test application functionality
- [ ] Document any issues encountered

## Security Best Practices

1. **Network Security:**
   - Use private networks for Raft communication
   - Implement firewall rules
   - Consider VPN for multi-datacenter deployments

2. **TLS/SSL:**
   - Terminate TLS at load balancer
   - Use certificates from trusted CA
   - Implement mTLS for Raft communication (future feature)

3. **Access Control:**
   - Implement authentication at reverse proxy
   - Use API keys or OAuth
   - Implement role-based access control (RBAC)

4. **Audit Logging:**
   - Log all API requests
   - Track administrative actions
   - Implement log aggregation and analysis

## Performance Tuning

See [performance.md](performance.md) for detailed tuning guidelines.

### Quick Tips

1. **Use SSDs** for log storage
2. **Optimize network** settings for low latency
3. **Right-size cluster** (5 nodes is usually optimal)
4. **Monitor and tune** election timeouts based on network latency
5. **Implement caching** at application layer for read-heavy workloads

## Troubleshooting

See [troubleshooting.md](troubleshooting.md) for common issues and solutions.
