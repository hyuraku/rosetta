# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is a distributed key-value store implementation using the Raft consensus algorithm, written in Go. The system provides strong consistency guarantees across multiple nodes through leader election, log replication, and safety properties defined by the Raft protocol.

**Current Status**: Beta - Core Raft features implemented with persistence, duplicate detection, and read optimizations. Approaching v1.0 production readiness.

**Go Version**: 1.23.4 (specified in `.go-version`)

## Architecture Overview

The codebase is organized into several key packages that work together:

### Core Components

- **raft/**: Implements the Raft consensus algorithm
  - `state.go`: Node state management (Follower/Candidate/Leader) and persistent state
  - `node.go`: Main Raft node orchestration and event loop
  - `rpc.go`: RequestVote and AppendEntries RPC implementations
  - `log.go`: Log entry management and replication logic

- **kvstore/**: Distributed key-value store built on top of Raft
  - `store.go`: Core KV operations (PUT/GET/DELETE) with Raft integration
  - `client.go`: HTTP client for interacting with the KV store

- **network/**: Network communication layer
  - `transport.go`: HTTP-based RPC transport for Raft messages
  - `discovery.go`: Cluster membership and node discovery

- **config/**: Configuration management for nodes and clusters

- **persistence/**: Data persistence layer
  - `file_storage.go`: Atomic file operations for Raft state and KV snapshots
  - `snapshotter.go`: Snapshot management with duplicate detection

- **examples/**: Practical examples and utilities
  - `simple-cluster/`: Scripts for running a 3-node cluster locally
  - `benchmark/`: Performance benchmarking tool

- **docs/**: Comprehensive documentation
  - `api.md`: HTTP API reference
  - `persistence.md`: Persistence implementation details
  - `performance.md`: Performance tuning guide
  - `deployment.md`: Production deployment guide
  - `troubleshooting.md`: Common issues and solutions

### Key Architectural Patterns

**Consensus Layer**: The Raft implementation follows the standard protocol with separate concerns for state management, log replication, and RPC communication. The `RaftNode` orchestrates these components through an event loop.

**State Machine**: The KV store acts as a replicated state machine. All operations go through Raft consensus before being applied to the local store. The `applyCh` channel bridges the Raft layer and KV store.

**Transport Abstraction**: The `RPCTransport` interface allows for different transport implementations. The codebase includes both HTTP transport for production and a mock transport for testing.

**Leader-Only Writes**: All write operations (PUT/DELETE) must go through the current leader node. Clients are redirected with HTTP 503 responses when contacting non-leader nodes.

**Persistence & Crash Recovery**: Raft state (term, votedFor, log) and KV store snapshots are persisted to disk using atomic file writes. Nodes automatically recover from crashes by loading persisted state.

**Duplicate Detection**: The snapshotter implements duplicate detection to prevent redundant snapshot writes, optimizing disk I/O and storage usage.

**Read Optimization**: Read-only queries bypass the Raft consensus layer when possible, reducing latency for read-heavy workloads while maintaining consistency guarantees.

## Development Commands

### Makefile Commands

The project uses a Makefile for common development tasks:

```bash
# Build the application
make build

# Run all tests (unit + integration)
make test

# Run tests with race detector
make test-race

# Run benchmarks
make bench

# Format code
make fmt

# Run linter (golangci-lint)
make lint

# Tidy dependencies
make tidy

# Install development dependencies
make deps

# Run single node for development
make run

# Clean build artifacts
make clean
```

### Building and Running
```bash
# Build the application
go build -o rosetta main.go

# Run a single node
./rosetta -id=node1 -listen=localhost:8080 -http=localhost:9080

# Run a 3-node cluster
./rosetta -id=node1 -listen=localhost:8080 -http=localhost:9080 -peers=node2:localhost:8081,node3:localhost:8082
./rosetta -id=node2 -listen=localhost:8081 -http=localhost:9081 -peers=node1:localhost:8080,node3:localhost:8082
./rosetta -id=node3 -listen=localhost:8082 -http=localhost:9082 -peers=node1:localhost:8080,node2:localhost:8081
```

### Testing
```bash
# Run all tests with verbose output
go test ./... -v

# Run unit tests only
go test ./tests/unit/... -v

# Run integration tests only
go test ./tests/integration/... -v

# Run specific test
go test ./tests/unit -run TestRaftStateInitialization -v
```

### Performance Commands
```bash
# Performance testing and optimization
go test -bench=. -benchmem ./...

# CPU profiling
go test -cpuprofile=cpu.prof -bench=.

# Memory profiling
go test -memprofile=mem.prof -bench=.

# Race condition detection
go test -race ./...
```

### API Usage
The HTTP API provides these endpoints:
- `PUT /kv` - Store key-value pair
- `GET /kv/{key}` - Retrieve value by key
- `DELETE /kv/{key}` - Delete key
- `GET /status` - Node status (term, leader state, log size)
- `GET /leader` - Current leader information

### Examples:
```bash
# save data
curl -X PUT http://localhost:9080/kv -d '{"key":"test","value":"hello"}'

# fetch data
curl http://localhost:9080/kv/test

# delete data
curl -X DELETE http://localhost:9080/kv/test
```

## Important Implementation Details

**Election Timeouts**: Each node has randomized election timeouts (150ms + node-specific offset) to prevent split votes. The `RaftState.ResetElectionTimer()` must be called on receiving valid AppendEntries.

**Log Consistency**: The `AppendEntries` RPC includes prevLogIndex and prevLogTerm for consistency checks. Failed consistency checks require backtracking via `NextIndex` decrements.

**Apply Channel**: The `applyCh` is the critical bridge between Raft consensus and the state machine. Commands are applied in order after being committed by a majority.

**Pending Operations**: The KV store tracks pending operations with unique IDs to match client requests with applied commands, handling timeouts and leadership changes.

**Mock Transport**: For testing, use `raft.NewMockTransport()` which provides in-memory RPC communication without network overhead.

**Persistence Implementation**:
- Raft state is persisted to `<data-dir>/raft_state.json` using atomic writes (write to temp file, then rename)
- KV store snapshots are saved to `<data-dir>/kv_snapshot.json`
- On startup, nodes check for persisted state and restore from disk if available
- The `FileStorage` interface abstracts persistence for testability
- See `docs/persistence.md` for detailed implementation information

**Duplicate Detection**:
- The `Snapshotter` component maintains a hash of the last snapshot to detect duplicates
- Before writing a snapshot, the system computes a hash and compares it with the previous one
- If the hash matches, the write is skipped, saving disk I/O and storage
- This optimization is critical for high-frequency snapshot scenarios

**Read Optimization**:
- Read-only queries (GET operations) can be served directly from the local KV store
- This bypasses the Raft consensus layer, significantly reducing latency
- The optimization is safe because GET operations don't modify state
- For strict linearizability, reads can still go through Raft if needed
- See PR #5 for the implementation details

## Code Quality

**Linting**: The project uses `golangci-lint` with comprehensive linter configuration (`.golangci.yml`):
- 30+ enabled linters including errcheck, gosec, staticcheck, stylecheck
- Custom settings for function length, cyclomatic complexity, and code duplication
- Line length limit: 140 characters
- Test files have relaxed rules for magic numbers and function length

**Formatting**: Standard Go formatting (`gofmt`) is enforced via `make fmt`

**Testing**: Race condition detection is available via `make test-race`

## Testing Strategy

- **Unit Tests**: Test individual components in isolation (state transitions, log operations, config validation)
- **Integration Tests**: Test multi-node clusters with leader election, log replication, and failure scenarios
- **Mock Transport**: Enables deterministic testing of Raft behavior without network complexity

The integration tests specifically verify leader election convergence, log replication across nodes, and proper handling of node failures and network partitions.

## Examples and Quick Start

**Simple Cluster Example** (`examples/simple-cluster/`):
```bash
cd examples/simple-cluster
./start.sh    # Start a 3-node cluster
./demo.sh     # Run basic operations demo
./stop.sh     # Stop the cluster
```

**Benchmarking** (`examples/benchmark/`):
```bash
cd examples/benchmark
go build benchmark.go
./benchmark -nodes=http://localhost:9080,http://localhost:9081,http://localhost:9082
```

## Project Status and Roadmap

### Completed Features ✅
- [x] Core Raft consensus (leader election, log replication)
- [x] Distributed key-value store (PUT/GET/DELETE)
- [x] Persistence and crash recovery
- [x] Duplicate detection for snapshots
- [x] Read-only query optimization
- [x] HTTP API
- [x] Comprehensive testing suite
- [x] Development tooling (Makefile, linter, formatter)

### In Progress 🚧
- [ ] Log compaction and snapshotting
- [ ] Monitoring and observability (metrics, health checks)

### Planned for v1.0 📋
- [ ] Complete log compaction implementation
- [ ] Add Prometheus metrics endpoint
- [ ] Enhanced monitoring and logging
- [ ] Production deployment documentation
- [ ] Performance tuning guide

See `TODO.md` for the complete feature roadmap and implementation details.

## Additional Resources

- **API Documentation**: See `docs/api.md` for HTTP API reference
- **Deployment Guide**: See `docs/deployment.md` for production deployment
- **Performance Tuning**: See `docs/performance.md` for optimization tips
- **Troubleshooting**: See `docs/troubleshooting.md` for common issues
- **Raft Implementation Status**: See `docs/raft-paper-implementation-status.md` for Raft paper compliance
