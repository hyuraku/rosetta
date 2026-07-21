# Rosetta - Future Features TODO

This document tracks planned features and enhancements for the Rosetta distributed key-value store.

## Status Legend
- 🔴 **High Priority** - Critical for production readiness
- 🟡 **Medium Priority** - Important for usability and functionality
- 🟢 **Low Priority** - Nice to have, enhances user experience
- ✅ **Completed** - Already implemented

---

## Completed Features ✅

- [x] **Persistence (Crash Recovery)** - Implemented in feature/raft-persistence
  - Raft state persistence (term, votedFor, log)
  - KV store snapshot storage
  - Automatic recovery on restart
  - Atomic file writes for data integrity
  - Comprehensive unit and integration tests

---

## High Priority Features 🔴

### 1. Log Compaction / Snapshotting
**Priority:** 🔴 High
**Estimated Effort:** Large (2-3 weeks)
**Status:** Not Started

**Description:**
Implement log compaction to prevent unbounded log growth. After a certain number of entries, create a snapshot of the state machine and discard old log entries.

**Requirements:**
- Automatic snapshot creation when log exceeds threshold
- Snapshot transfer to followers
- Truncation of old log entries after snapshot
- InstallSnapshot RPC implementation
- Configuration for snapshot interval and retention

**Implementation Steps:**
1. Add snapshot creation logic in KVStore
2. Implement InstallSnapshot RPC
3. Add log truncation after snapshot
4. Update recovery to load from snapshot + remaining log
5. Add configuration for compaction thresholds
6. Write tests for snapshot creation and recovery

**Related Files:**
- `raft/snapshot.go` (new)
- `raft/rpc.go` (add InstallSnapshot RPC)
- `kvstore/store.go` (auto-snapshot logic)
- `config/config.go` (add compaction config)

**References:**
- Raft Paper Section 7: Log compaction
- etcd implementation

---

### 2. Monitoring & Observability
**Priority:** 🔴 High
**Estimated Effort:** Medium (1-2 weeks)
**Status:** Not Started

**Description:**
Add comprehensive monitoring and observability features for production operations.

**Requirements:**
- Prometheus metrics endpoint (`/metrics`)
- Health check endpoint (`/health`)
- Structured logging (JSON format)
- OpenTelemetry distributed tracing support
- Key metrics to track:
  - Request latency (p50, p90, p99)
  - Throughput (ops/sec)
  - Leader election count
  - Log size and growth rate
  - Snapshot frequency
  - Node status (leader/follower/candidate)
  - Replication lag

**Implementation Steps:**
1. Add Prometheus client library
2. Implement metrics collection points throughout codebase
3. Create `/metrics` endpoint
4. Add structured logging with configurable levels
5. Implement `/health` endpoint with detailed checks
6. Add OpenTelemetry integration (optional)
7. Create Grafana dashboard examples

**Related Files:**
- `monitoring/metrics.go` (new)
- `monitoring/health.go` (new)
- `monitoring/logging.go` (new)
- `main.go` (add metrics HTTP handler)
- `docs/monitoring.md` (new)

---

### 2.5. Decouple Log Application into a Dedicated Applier Goroutine
**Priority:** 🔴 High
**Estimated Effort:** Medium (3-5 days)
**Status:** Not Started

**Background:**
`applyEntries` (`raft/log.go`) sends committed entries to `applyCh` while holding
`rs.mu`. A prior bug silently dropped entries with a non-blocking `select`/`default`
send while `LastApplied` had already advanced, permanently skipping committed entries
(Raft state-machine safety violation). This was fixed by switching to a blocking send,
which guarantees safety but keeps `rs.mu` held until the state machine drains the
channel. Under heavy write bursts (once `applyCh`'s buffer of 100 fills), the entire
node stalls — vote responses, heartbeat handling, and the election timer are all
blocked — trading liveness for safety.

**Goal:**
Separate application from consensus. A dedicated applier goroutine should wait for
`CommitIndex` to advance (via a condition variable or a notify channel) and send to
`applyCh` **without holding `rs.mu`**, so consensus progress is never blocked by a slow
state machine. This is the standard MIT 6.824-style pattern.

**Requirements:**
- Applier goroutine reads `CommitIndex`/`LastApplied` and copies entries under a brief
  lock, then releases `rs.mu` before sending to `applyCh`.
- `UpdateCommitIndex` / `AppendEntries` / commit-advance paths signal the applier
  (cond var / notify channel) instead of calling `applyEntries` inline under the lock.
- Preserve ordering and exactly-once apply semantics; `LastApplied` must only advance
  after a successful send.
- Reconcile with the `InstallSnapshot` apply path (`raft/rpc.go`), which also currently
  sends to `applyCh` under `rs.mu`.
- Clean shutdown so the applier goroutine exits without racing `close(applyCh)`.

**Affected Files:**
- `raft/log.go` (`applyEntries`, `UpdateCommitIndex`)
- `raft/state.go` (applier goroutine, notify primitive, lifecycle)
- `raft/rpc.go` (`AppendEntries` commit path, `InstallSnapshot` apply path)
- `raft/node.go` (start/stop the applier goroutine)

**Note:** Recommended as a standalone PR, separate from the safety fix that made the
send blocking.

---

## Medium Priority Features 🟡

### 3. Dynamic Cluster Membership
**Priority:** 🟡 Medium
**Estimated Effort:** Large (3-4 weeks)
**Status:** Not Started

**Description:**
Allow adding and removing nodes from a running cluster without downtime.

**Requirements:**
- Add node to running cluster
- Remove node safely from cluster
- Configuration change consensus (joint consensus approach)
- API endpoints for membership management
- Automatic peer discovery updates
- Graceful node shutdown

**Implementation Steps:**
1. Implement joint consensus for configuration changes
2. Add membership change log entries
3. Create `/admin/add-node` and `/admin/remove-node` endpoints
4. Update peer tracking on all nodes
5. Implement configuration propagation
6. Add safety checks (quorum validation)
7. Write tests for various membership change scenarios

**Related Files:**
- `raft/membership.go` (new)
- `raft/config_change.go` (new)
- `network/discovery.go` (update)
- `main.go` (add admin endpoints)
- `docs/membership.md` (new)

**References:**
- Raft Paper Section 6: Cluster membership changes
- Raft dissertation Chapter 4

---

### 4. Authentication & Authorization
**Priority:** 🟡 Medium
**Estimated Effort:** Medium (1-2 weeks)
**Status:** Not Started

**Description:**
Add security features to protect the cluster and data.

**Requirements:**
- API key authentication
- TLS/SSL support for HTTP API
- TLS for Raft RPC communication
- Basic ACL (Access Control Lists) for keys
- Client certificate authentication (mTLS)
- Role-based access control (optional)

**Implementation Steps:**
1. Add TLS configuration options
2. Implement API key middleware
3. Add TLS support to HTTP server
4. Add TLS support to Raft transport
5. Implement basic ACL system
6. Add authentication to all endpoints
7. Create admin tools for credential management
8. Write security documentation

**Related Files:**
- `auth/middleware.go` (new)
- `auth/acl.go` (new)
- `network/tls.go` (new)
- `config/config.go` (add auth config)
- `docs/security.md` (new)

---

### 5. Backup & Restore
**Priority:** 🟡 Medium
**Estimated Effort:** Small (1 week)
**Status:** Partially Implemented

**Current State:**
Basic backup functionality exists via `FileStorage.CopyTo()` method.

**Requirements:**
- Full cluster backup
- Incremental backup support
- Point-in-time recovery
- Export to S3/cloud storage
- Automated backup scheduling
- Backup verification
- Restore from backup API

**Implementation Steps:**
1. Create backup service
2. Add `/admin/backup` endpoint
3. Implement incremental backup logic
4. Add cloud storage integration (S3, GCS)
5. Create restore utility
6. Add backup scheduling
7. Implement backup verification
8. Write backup/restore documentation

**Related Files:**
- `backup/service.go` (new)
- `backup/cloud.go` (new)
- `main.go` (add backup endpoints)
- `persistence/file_storage.go` (extend backup methods)
- `docs/backup.md` (new)

---

### 6. Read Optimization
**Priority:** 🟡 Medium
**Estimated Effort:** Medium (1-2 weeks)
**Status:** Not Started

**Description:**
Optimize read operations to reduce latency and increase throughput.

**Requirements:**
- ReadIndex implementation for linearizable reads
- Lease-based reads (bypass consensus for fresh reads)
- Follower reads with bounded staleness
- Configuration for read consistency level
- Read-only query optimization

**Implementation Steps:**
1. Implement ReadIndex mechanism
2. Add lease-based reads for leader
3. Implement follower read with staleness bounds
4. Add consistency level to API (strong/bounded/eventual)
5. Optimize read path in KVStore
6. Add read caching (optional)
7. Benchmark read performance improvements
8. Document read consistency guarantees

**Related Files:**
- `raft/read.go` (new)
- `kvstore/store.go` (optimize reads)
- `config/config.go` (read consistency config)
- `docs/consistency.md` (new)

**References:**
- Raft dissertation Section 6.4: Processing read-only queries

---

## Low Priority Features 🟢

### 7. Transaction Support
**Priority:** 🟢 Low
**Estimated Effort:** Large (3-4 weeks)
**Status:** Not Started

**Description:**
Add multi-key transaction support with ACID guarantees.

**Requirements:**
- Multi-key atomic operations
- Compare-And-Swap (CAS) operations
- Transaction log and rollback
- Isolation levels (serializable by default)
- Optimistic concurrency control
- Transaction API (BEGIN/COMMIT/ROLLBACK)

**Implementation Steps:**
1. Design transaction protocol
2. Implement transaction coordinator
3. Add transaction log entries
4. Implement CAS operations
5. Add conflict detection and resolution
6. Create transaction API endpoints
7. Add transaction timeout handling
8. Write transaction tests
9. Document transaction semantics

**Related Files:**
- `transaction/coordinator.go` (new)
- `transaction/log.go` (new)
- `kvstore/store.go` (add transaction support)
- `docs/transactions.md` (new)

---

### 8. Advanced Query Features
**Priority:** 🟢 Low
**Estimated Effort:** Medium (2-3 weeks)
**Status:** Not Started

**Description:**
Add advanced querying capabilities beyond simple key-value operations.

**Requirements:**
- Range queries (get keys in range)
- Prefix search (keys starting with prefix)
- Batch operations (multi-get, multi-put, multi-delete)
- TTL (Time To Live) for keys
- Key expiration and automatic cleanup
- Secondary indexes (optional)
- Filtering and sorting (optional)

**Implementation Steps:**
1. Add range scan to storage layer
2. Implement prefix search algorithm
3. Add batch operation APIs
4. Implement TTL tracking system
5. Add background expiration cleaner
6. Create query optimization layer
7. Add pagination support
8. Benchmark query performance
9. Document query API

**Related Files:**
- `kvstore/query.go` (new)
- `kvstore/ttl.go` (new)
- `kvstore/store.go` (extend operations)
- `docs/queries.md` (new)

---

### 9. Configuration Management
**Priority:** 🟢 Low
**Estimated Effort:** Small (1 week)
**Status:** Partially Implemented

**Description:**
Improve configuration management and runtime configurability.

**Requirements:**
- Dynamic configuration reload (SIGHUP)
- Environment variable support
- Configuration validation improvements
- Hot reload for non-critical settings
- Configuration versioning
- Configuration API endpoint
- Config file templates and examples

**Implementation Steps:**
1. Add environment variable parsing
2. Implement config reload handler
3. Add `/admin/config` endpoint
4. Separate hot-reloadable vs restart-required configs
5. Add config validation warnings
6. Create config examples for common scenarios
7. Document all configuration options
8. Add config migration tool

**Related Files:**
- `config/config.go` (enhance validation)
- `config/reload.go` (new)
- `config/env.go` (new)
- `main.go` (add SIGHUP handler)
- `examples/configs/` (new directory)
- `docs/configuration.md` (enhance)

---

### 10. Client Libraries
**Priority:** 🟢 Low
**Estimated Effort:** Medium (2 weeks per language)
**Status:** Not Started

**Description:**
Create official client libraries for easy integration.

**Requirements:**
- Go client library with advanced features
- Python client library
- JavaScript/TypeScript client library
- Automatic leader detection and failover
- Connection pooling
- Retry logic with exponential backoff
- Consistent hashing for client-side sharding (future)

**Implementation Steps:**
1. Design client API
2. Implement Go client with all features
3. Add automatic leader discovery
4. Implement retry and timeout logic
5. Create Python client (if needed)
6. Create JavaScript client (if needed)
7. Add comprehensive examples
8. Write client documentation
9. Publish to package registries

**Related Files:**
- `client/go/` (new)
- `client/python/` (new)
- `client/javascript/` (new)
- `docs/client-libraries.md` (new)

---

## Performance Enhancements

### 11. Performance Optimizations
**Priority:** 🟡 Medium
**Estimated Effort:** Ongoing
**Status:** Continuous Improvement

**Areas for Optimization:**
- Batch writes for better throughput
- Pipelining for Raft RPCs
- Zero-copy serialization
- Memory pooling for allocations
- Async persistence (write-behind cache)
- Compression for large values
- Network buffer optimization

**Benchmarking:**
- Throughput tests (ops/sec)
- Latency tests (p50, p90, p99, p999)
- Concurrent client tests
- Large cluster tests (10+ nodes)
- Long-running stability tests

**Related Files:**
- `tests/performance/` (enhance)
- `docs/performance.md` (update)

---

## Testing & Quality

### 12. Testing Enhancements
**Priority:** 🟡 Medium
**Estimated Effort:** Ongoing
**Status:** Continuous Improvement

**Test Coverage Goals:**
- Unit test coverage: 80%+
- Integration test coverage: comprehensive scenarios
- Chaos testing (random failures)
- Fuzz testing for edge cases
- Long-running soak tests
- Network partition simulation
- Byzantine fault testing (optional)

**Test Infrastructure:**
- CI/CD pipeline (GitHub Actions)
- Automated test runs on PR
- Performance regression detection
- Test result reporting

**Related Files:**
- `tests/chaos/` (new)
- `tests/fuzz/` (new)
- `.github/workflows/` (new)

---

## Documentation

### 13. Documentation Improvements
**Priority:** 🟡 Medium
**Estimated Effort:** Ongoing
**Status:** Continuous Improvement

**Documentation Needs:**
- Architecture diagrams
- Sequence diagrams for key operations
- Deployment best practices
- Capacity planning guide
- Migration guide (upgrades)
- Disaster recovery procedures
- FAQ section
- Video tutorials (optional)

**Related Files:**
- `docs/architecture.md` (enhance)
- `docs/best-practices.md` (new)
- `docs/capacity-planning.md` (new)
- `docs/disaster-recovery.md` (new)
- `docs/faq.md` (new)

---

## Project Milestones

### v1.0 (Production Ready)
- [x] Basic Raft implementation
- [x] Key-value operations
- [x] Persistence
- [ ] Log compaction
- [ ] Monitoring
- [ ] Documentation complete
- [ ] Test coverage 80%+

### v1.1 (Enhanced Operations)
- [ ] Dynamic membership
- [ ] Authentication & Authorization
- [ ] Backup & Restore
- [ ] Advanced monitoring

### v1.2 (Performance & Features)
- [ ] Read optimizations
- [ ] Transaction support
- [ ] Advanced queries
- [ ] Client libraries

### v2.0 (Enterprise Features)
- [ ] Multi-region support
- [ ] Geographic replication
- [ ] Advanced security
- [ ] Enterprise dashboard

---

## Contributing

If you'd like to contribute to any of these features:

1. Check the feature status in this document
2. Review related documentation and design discussions
3. Open an issue to discuss your approach
4. Submit a PR with tests and documentation
5. See [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines

---

## Notes

- **Priority** levels may change based on user feedback and production needs
- **Estimated Effort** is approximate and may vary
- Some features may be combined or split as development progresses
- Performance optimizations are ongoing throughout all releases

---

Last Updated: 2025-11-11
Maintained by: Rosetta Development Team
