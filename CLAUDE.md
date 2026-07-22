# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is a distributed key-value store implementation using the Raft consensus algorithm, written in Go, built as a learning project.

**Current Status**: Educational implementation — not production-ready. The core happy path (leader election, log replication, HTTP KV API, crash recovery) works, but a safety review (`docs/safety-review-2026-07-07.md`) confirmed real violations of Raft's safety properties, most notably in log compaction. `KNOWN_ISSUES.md` at the repository root is the live, authoritative status of these issues. Never describe this project as production-ready in code or docs.

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

- **docs/**: Documentation (see `docs/README.md` for the index)
  - `api.md`: HTTP API reference
  - `persistence.md`: Persistence implementation details
  - `log-compaction.md`: Log compaction design and current state
  - `raft-paper-implementation-status.md`: Raft paper compliance status
  - `safety-review-2026-07-07.md`: Frozen safety review report (details behind `KNOWN_ISSUES.md`)
  - `textbook.md`: In-depth walkthrough of the codebase

### Key Architectural Patterns

**Consensus Layer**: The Raft implementation follows the standard protocol with separate concerns for state management, log replication, and RPC communication. The `RaftNode` orchestrates these components through an event loop.

**State Machine**: The KV store acts as a replicated state machine. All operations go through Raft consensus before being applied to the local store. The `applyCh` channel bridges the Raft layer and KV store.

**Transport Abstraction**: The `RPCTransport` interface allows for different transport implementations. The codebase includes both HTTP transport for production and a mock transport for testing.

**Leader-Only Writes**: All write operations (PUT/DELETE) must go through the current leader node. Clients are redirected with HTTP 503 responses when contacting non-leader nodes.

**Persistence & Crash Recovery**: Raft state (term, votedFor, log) and KV store snapshots are persisted to disk using atomic file writes. Nodes automatically recover from crashes by loading persisted state.

**Duplicate Detection**: The snapshotter implements duplicate detection to prevent redundant snapshot writes, optimizing disk I/O and storage usage.

**Read Optimization**: Read-only queries use the ReadIndex protocol — the leader confirms it still holds leadership via a fresh heartbeat quorum and serves from local state once the state machine has applied through the captured commit index, giving linearizable reads without appending to the log.

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
# Race condition detection
go test -race ./...

# HTTP-level benchmarking (no Go Benchmark* functions exist in this repo;
# `make bench` / `go test -bench` find nothing — use the benchmark tool instead)
cd examples/benchmark && go build benchmark.go && ./benchmark -nodes=http://localhost:9080
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
- Raft state is persisted to `<data-dir>/<node-id>/raft_state.json` using atomic writes (write to temp file, then rename)
- KV store snapshots are saved to `<data-dir>/<node-id>/snapshot.json`
- On startup, nodes check for persisted state and restore from disk if available
- The `FileStorage` interface abstracts persistence for testability
- See `docs/persistence.md` for detailed implementation information

**Duplicate Detection**:
- The `Snapshotter` component maintains a hash of the last snapshot to detect duplicates
- Before writing a snapshot, the system computes a hash and compares it with the previous one
- If the hash matches, the write is skipped, saving disk I/O and storage
- This optimization is critical for high-frequency snapshot scenarios

**Read Optimization**:
- GET operations on the leader are served through the ReadIndex protocol (Raft dissertation §6.4): the leader captures its commit index, confirms it still holds leadership with a fresh heartbeat quorum, waits until the state machine has applied through that index, then serves from local state — no log append
- A newly elected leader appends a current-term no-op entry so the read index reflects all previously committed entries; reads briefly return `ErrNoCurrentTermCommit` until that no-op commits
- This replaced the earlier lease-based reads and closes the confirmed linearizability gaps (`KNOWN_ISSUES.md` D1–D3, now fixed): a partitioned ex-leader fails the read instead of returning a stale value

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

### Working (verified happy path)
- Core Raft consensus (leader election, log replication) in a stable single-leader cluster
- Distributed key-value store (PUT/GET/DELETE) over HTTP
- Persistence and crash recovery with persist-before-reply discipline
- Duplicate detection for snapshot writes (disk I/O optimization)
- Development tooling (Makefile, linter, formatter), unit + integration tests

### Implemented but broken or unwired ⚠️
See `KNOWN_ISSUES.md` for the authoritative, up-to-date list. After the 2026-07 fixes the remaining issues are:
- Log compaction / InstallSnapshot: absolute-index handling (A1–A5), production snapshotter wiring (A6), and follower snapshot persistence (A8) are fixed, but the InstallSnapshot receiver still retains a divergent suffix without a term check (A7, Log Matching violation) — so `MaxRaftState=0` (compaction disabled) remains the only safe configuration
- Blocking `applyCh` send under `rs.mu` in `InstallSnapshot` (B3), two data races (E1, E2), and ignored `persist()` errors on the leader's own append path (C3, partial)

See `TODO.md` for the feature roadmap.

## Documentation Discipline

The docs in this repository previously drifted badly out of sync with the code (three contradictory "generations" coexisted until the 2026-07 overhaul). To prevent a recurrence:

- **Same-PR rule**: Any PR that changes behavior in `raft/`, `kvstore/`, `network/`, `persistence/`, or `main.go` MUST update the affected docs and `KNOWN_ISSUES.md` in the same PR (including marking fixed issues as fixed with the commit hash)
- **Verification headers**: Each file under `docs/` carries a "Last verified: <date> against commit <hash>" line below its title. When you verify or update a doc against the code, update this line
- **No aspirational docs**: Document what the code does now. Planned or partially wired features must be labeled as such, with a `KNOWN_ISSUES.md` reference where one exists
- `docs/safety-review-2026-07-07.md` is a frozen point-in-time report — never edit it; record status changes in `KNOWN_ISSUES.md` instead

## Additional Resources

- **Known Issues**: See `KNOWN_ISSUES.md` for the live status of confirmed safety issues
- **API Documentation**: See `docs/api.md` for HTTP API reference
- **Persistence**: See `docs/persistence.md` for persistence and crash recovery details
- **Log Compaction**: See `docs/log-compaction.md` for the design and its current state
- **Raft Implementation Status**: See `docs/raft-paper-implementation-status.md` for Raft paper compliance
- **Codebase Walkthrough**: See `docs/textbook.md` for an in-depth guided tour
