# Persistence Feature

> Last verified: 2026-07-21 against commit `9383cfe`.

This document describes the persistence feature implemented in Rosetta, which provides crash recovery and durability for the distributed key-value store.

## Overview

The persistence layer ensures that:
- Raft state (term, votedFor, log, snapshot metadata) survives node crashes
- KV store snapshots are preserved across restarts
- All persisted writes are atomic (temp file + rename) and fsynced

> **Warning**: Snapshots received by a follower via InstallSnapshot are applied
> in memory only and are not persisted to disk, so a crash after log compaction
> can make part of the state unrecoverable on that node.
> See ../KNOWN_ISSUES.md (A8).

## Architecture

### Components

#### 1. Storage Interface
The `Storage` interface defines the contract for persistent storage:

```go
type Storage interface {
    SaveRaftState(state *raft.PersistentState) error
    LoadRaftState() (*raft.PersistentState, error)
    SaveSnapshot(snapshot *Snapshot) error
    LoadSnapshot() (*Snapshot, error)
    Close() error
}
```

#### 2. File Storage
`FileStorage` implements the `Storage` interface using the file system:

- **Location**: `persistence/file_storage.go`
- **Storage Format**: JSON
- **Atomicity**: Uses atomic file writes (write to temp file, fsync, rename, then fsync the directory)
- **File Permissions**: `0600` for files, `0700` for the data directory
- **Files**:
  - `raft_state.json`: Persistent Raft state
  - `snapshot.json`: KV store snapshot

#### 3. Raft Persister
Adapter that implements `raft.Persister` interface:

```go
type Persister interface {
    SaveRaftState(state *PersistentState) error
    LoadRaftState() (*PersistentState, error)
}
```

#### 4. KV Snapshotter
Adapter (`persistence/kv_snapshotter.go`) that implements the `kvstore.Snapshotter` and `kvstore.SnapshotterV2` interfaces:

```go
type Snapshotter interface {
    SaveSnapshot(data map[string]string, lastIncludedIndex, lastIncludedTerm int) error
    LoadSnapshot() (data map[string]string, lastIncludedIndex, lastIncludedTerm int, err error)
}

// SnapshotterV2 extends Snapshotter to support session data for duplicate detection
type SnapshotterV2 interface {
    Snapshotter
    SaveSnapshotV2(data *SnapshotData, lastIncludedIndex, lastIncludedTerm int) error
    LoadSnapshotV2() (data *SnapshotData, lastIncludedIndex, lastIncludedTerm int, err error)
}
```

The KV store prefers the V2 interface, which also persists client sessions
(for duplicate detection). Loading falls back to the V1 format (plain
`map[string]string`) for older snapshot files.

## Data Persistence

### What is Persisted

#### Raft State
- **CurrentTerm**: Current election term
- **VotedFor**: Candidate that received vote in current term
- **Log**: Log of commands (entries not yet covered by a snapshot)
- **LastIncludedIndex** / **LastIncludedTerm**: Snapshot metadata for log compaction

#### KV Store State
- **KVData**: Complete key-value pairs
- **Sessions**: Client sessions for duplicate detection (V2 format)
- **LastIncludedIndex**: Last log index included in snapshot
- **LastIncludedTerm**: Term of last included log entry

### When Data is Persisted

Raft state is persisted when:
1. Term is incremented
2. Vote is cast (VotedFor changes)
3. Log entry is appended
4. Log is truncated (on AppendEntries consistency check)
5. The log is compacted after a snapshot
6. Before responding to RequestVote/AppendEntries RPCs that changed
   persistent state (a failed persist causes the RPC to be rejected)

KV store snapshots are saved automatically by the apply loop: after
`max_raft_state` commands (default 1000) have been applied since the last
snapshot, the store writes a snapshot and asks Raft to compact its log.
There is no manual snapshot API and no snapshot on shutdown.

## Usage

### Configuration

Set the data directory in your configuration:

```json
{
  "node_id": "node1",
  "data_dir": "./data",
  "max_raft_state": 1000,
  ...
}
```

Each node will create a subdirectory under `data_dir` named after its `node_id`.
`max_raft_state` controls how many applied commands trigger an automatic
snapshot (default 1000). Persistence is always enabled; there is no flag to
disable it.

### Startup Behavior

1. **First Start**: Node initializes with empty state
2. **Restart**: Node loads persisted state automatically
   - Raft state is restored (term, votedFor, log)
   - KV store snapshot is restored (if exists)
   - Node resumes operation from last known state
3. **Corrupted Raft state**: If `raft_state.json` exists but cannot be read or
   parsed, the node refuses to start instead of silently resetting to term 0
   (which could allow a double vote)

### Example: Node Restart

```bash
# Start node
./rosetta -id=node1 -listen=localhost:8080 -http=localhost:9080

# Perform some operations
curl -X PUT http://localhost:9080/kv -d '{"key":"test","value":"data"}'

# Stop node (Ctrl+C)
# Data is persisted in ./data/node1/

# Restart node
./rosetta -id=node1 -listen=localhost:8080 -http=localhost:9080

# Data is restored automatically
curl http://localhost:9080/kv/test
# Returns: {"success":true,"value":"data"}
```

## File Structure

```
data/
└── node1/
    ├── raft_state.json    # Raft persistent state
    └── snapshot.json      # KV store snapshot
```

### Raft State File Format

```json
{
  "CurrentTerm": 5,
  "VotedFor": "node2",
  "Log": [
    {
      "term": 1,
      "index": 1,
      "command": "{\"op\":\"PUT\",\"key\":\"test\",\"value\":\"data\"}",
      "type": "command"
    }
  ],
  "LastIncludedIndex": 0,
  "LastIncludedTerm": 0
}
```

### Snapshot File Format

```json
{
  "last_included_index": 100,
  "last_included_term": 5,
  "data": "base64-encoded JSON"
}
```

`data` is the base64 encoding of the serialized state machine. In the V2
format this is `{"kv_data": {...}, "sessions": {...}}`; older V1 snapshots
contain a plain `{"key": "value", ...}` map.

## Crash Recovery

### Recovery Process

1. **Node Crashes**: Power failure, process killed, etc.
2. **Node Restarts**:
   - Storage layer opens data directory
   - Loads `raft_state.json` if exists
   - Loads `snapshot.json` if exists
3. **State Restoration**:
   - RaftState restores term, votedFor, log, and snapshot metadata
   - KVStore restores key-value data (and sessions, for V2 snapshots)
4. **Resume Operation**: Node continues from recovered state

> **Warning**: Raft's volatile `LastApplied` is not restored from the snapshot
> metadata on restart. After the log has been compacted, recovery can re-apply
> or misindex entries. See ../KNOWN_ISSUES.md (A5).

### Recovery Guarantees

- **Log durability**: Log entries, term, and vote are written to stable
  storage before RPC responses are sent
- **Consistency**: Each file is always internally consistent (atomic writes)
- **Ordering**: Log entries maintain correct order
- **Idempotency**: Safe to restart multiple times

> **Warning**: These guarantees do not cover snapshots a follower receives via
> InstallSnapshot — those are not written to disk, so state covered only by
> such a snapshot is lost on crash. See ../KNOWN_ISSUES.md (A8).

### Testing Recovery

Run the crash recovery tests:

```bash
# Unit tests
go test ./tests/unit -run 'TestFileStorage|TestKVSnapshotter|TestRaftPersister' -v

# Integration tests
go test ./tests/integration -run Persistence -v
```

## Performance Considerations

### Write Performance

- Each state change triggers a write to disk
- Every write rewrites the entire `raft_state.json` (including the full
  remaining log) as indented JSON, so write cost grows with log size until
  the next compaction
- Writes are atomic (temp file + rename) and fsynced for durability
- Lowering `max_raft_state` shortens the log kept on disk but snapshots more
  often; raising it does the opposite

This design favors simplicity over throughput — Rosetta is a learning
implementation, not a tuned storage engine.

## Backup and Restore

### Creating Backups

```go
storage, _ := persistence.NewFileStorage("./data/node1")
err := storage.CopyTo("./backup/node1-2024-01-01")
```

### Restoring from Backup

1. Stop the node
2. Copy backup files to data directory
3. Start the node

```bash
# Stop node
kill <pid>

# Restore backup
cp -r ./backup/node1-2024-01-01/* ./data/node1/

# Start node
./rosetta -id=node1 ...
```

## Troubleshooting

### Corrupted State File

If `raft_state.json` is corrupted, the node refuses to start (see Startup
Behavior above). To recover, remove the corrupted files:

```bash
# Remove corrupted files (WARNING: data loss)
rm ./data/node1/raft_state.json
rm ./data/node1/snapshot.json

# Node will start with empty state
./rosetta -id=node1 ...
```

### Disk Full

Monitor disk usage:

```bash
df -h ./data
du -sh ./data/node1
```

Lowering `max_raft_state` makes compaction run more often, which reduces the
size of `raft_state.json`.

### Permission Issues

Files are created with owner-only permissions. Ensure the data directory is
writable by the process owner:

```bash
chmod 700 ./data ./data/node1
chmod 600 ./data/node1/*
```

## Related Features

- **Log Compaction**: Implemented. After a snapshot is saved, the Raft log is
  truncated up to the snapshot boundary and the compacted state is persisted.
- **Snapshot Transfer (InstallSnapshot)**: The RPC exists in the Raft layer,
  but the production binary never wires a `raft.Snapshotter`, so leaders never
  send snapshots to lagging followers. See ../KNOWN_ISSUES.md (A6).

## Future Enhancements

Planned improvements:
1. **Compression**: Compress snapshot data
2. **Incremental Snapshots**: Save only changed data
3. **Write-Ahead Log (WAL)**: More efficient append-only log
4. **Background Persistence**: Async writes for better performance

## API Integration

### Check Node Status

```bash
# Get node status (node_id, term, is_leader, log_size).
# There is no persistence-specific information in this response.
curl http://localhost:9080/status
```

There is no API endpoint to trigger a manual snapshot; snapshots are taken
automatically by the apply loop.

## References

- [Raft Paper](https://raft.github.io/raft.pdf) - Section 7: Log compaction
- [etcd Documentation](https://etcd.io/docs/) - Persistence implementation
- [RocksDB](https://rocksdb.org/) - High-performance persistent storage

## Security Considerations

1. **File Permissions**: Files are created with `0600` and directories with
   `0700` (owner-only access) by default
2. **Encryption at Rest**: Not implemented; data is stored as plain JSON
3. **Backup Security**: Files copied with `CopyTo` keep the same restrictive
   permissions, but securing backup locations is up to the operator

## Conclusion

The persistence feature provides durability for Rosetta's Raft state and KV
snapshots, allowing nodes to recover from crashes along the normal log path.
Known gaps remain around follower-side snapshot durability and snapshot
transfer wiring — see ../KNOWN_ISSUES.md (A8, A6) before relying on recovery
in compaction scenarios.
