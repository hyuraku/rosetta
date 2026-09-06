# Persistence Feature

> Last verified: 2026-09-06 against commit `c6ee4b4`.

This document describes the persistence feature implemented in Rosetta, which provides crash recovery and durability for the distributed key-value store.

## Overview

The persistence layer ensures that:
- Raft state (term, votedFor, log, snapshot metadata) survives node crashes
- KV store snapshots are preserved across restarts
- All persisted writes are atomic (temp file + rename) and fsynced, **per file** — see
  the current-constraints warning below for what this does not cover

> **Current constraint**: Raft state (`raft_state.json`) and the KV snapshot
> (`snapshot.json`) are two separate files. Each is atomically written on its own,
> but there is still **no atomicity across the two files**. What makes that safe is
> the order they are written in (R3, fixed in `0695b95`): on the InstallSnapshot
> receive path the KV payload is made durable first, then the Raft boundary, then
> the in-memory state machine is updated, so a crash can only leave the snapshot
> *ahead* of the Raft state. Startup checks the pair and refuses the other
> direction. See "Snapshot generation consistency" below and ../KNOWN_ISSUES.md (R3).

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

A failed persist never passes silently, and it never leaves the in-memory log
ahead of the disk. On the leader's own append paths (`RaftState.Start`, the
`AppendLogEntry` wrapper, `TruncateLogAfter`, and the no-op appended on election)
the in-memory change is rolled back so memory and disk agree, and the error is
returned to the caller: `RaftNode.Start` reports it, and the KV store fails the
client operation instead of waiting for it to be applied.

The receiving side of `AppendEntries` behaves the same way (R2, fixed in
`c362ae4`): if the entries merged into the log cannot be persisted, the merge is
rolled back before the handler replies `Success=false`. That matters because the
handler skips the write for a request whose entries already match — an
optimization that is only sound while memory and disk agree. Leaving an
un-persisted merge in memory used to make the leader's *resend* of the same
request look like a duplicate, so the follower answered `Success=true` for
entries that were on no disk, the leader counted that ACK in `MatchIndex`, and a
crash of that follower could lose a committed entry.

`RaftState.Start` also makes the leadership check, the term stamp and the
persist one critical section (R1, fixed in `2c26b9a`), so a node demoted
mid-append never writes a command under the new leader's term.

The InstallSnapshot receive path follows the same discipline as of `0695b95`
(R3): a failed boundary persist rolls the in-memory log, snapshot boundary,
`CommitIndex` and `LastApplied` back to their pre-call values, so memory and disk
still agree when the handler returns. `TruncateLogTo` does the same for the
compaction path. The KV payload written before the boundary deliberately stays on
disk — see "Snapshot generation consistency" below.

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

Raft's volatile `CommitIndex`/`LastApplied` are restored from the snapshot boundary
on restart (`raft/state.go:191-192`, `loadPersistentState`), so recovery does not
re-apply or misindex entries below the snapshot (A5, fixed).

### Recovery Guarantees

- **Log durability**: Log entries, term, and vote are written to stable
  storage before RPC responses are sent
- **Consistency**: Each file is always internally consistent (atomic writes)
- **Ordering**: Log entries maintain correct order
- **Idempotency**: Safe to restart multiple times

A snapshot a follower receives via InstallSnapshot is persisted to disk too
(A8, fixed). Since `0695b95` the write happens in the Raft handler, *before* the
snapshot boundary is persisted, and the resulting `ApplyMsg` carries
`SnapshotPersisted: true` so `installSnapshotFromApplyMsg`
(`kvstore/store.go:348-395`) only updates memory instead of writing the same
generation a second time. When no `raft.Snapshotter` is wired — memory-only
configurations and most unit tests — the flag is false and the KV store still
owns the write, as before.

### Snapshot generation consistency

`raft_state.json` and `snapshot.json` are written independently, so a crash lands
between two writes and the two files can come back at different generations. The
write order decides which mismatch is reachable (R3, fixed in `0695b95`).

The InstallSnapshot receiver (`raft/rpc.go`, `RaftState.InstallSnapshot`) holds
one ordering invariant, stated in its doc comment:

1. the state machine payload becomes durable (`Snapshotter.InstallSnapshot`),
2. the Raft snapshot boundary becomes durable (`persist`),
3. the in-memory state machine is updated (the `applyCh` send).

Only the "snapshot ahead of Raft" mismatch is therefore reachable, and it is
recoverable: the node comes up on the older Raft boundary, replays the committed
prefix from its log, and the KV apply loop drops every entry at or below the
index its own snapshot already covers. The opposite mismatch — Raft compacted
past what the state machine holds — is unrecoverable, because the discarded
entries can never be delivered again.

Startup enforces this. `persistence.VerifySnapshotConsistency`
(`persistence/consistency.go`) compares the two boundaries and is called from
`verifyDurableState` in `main.go` before either half is constructed:

- Raft boundary **>** snapshot index → the process refuses to start, the same
  fail-closed handling C4 gives an unreadable state file. Restore the node's data
  directory from a peer.
- snapshot index **>** Raft boundary → a compaction did not finish before the
  last shutdown. Logged, then absorbed by the apply loop's monotonicity guard.

The check lives in `persistence` rather than in `raft` or `kvstore` because those
two are constructed independently in tests; only the caller that owns the shared
`Storage` sees both files.

Moving the `applyCh` send off the handler goroutine (B3, `f873d9b`) kept steps 1
and 2 exactly where they were: both still complete, in that order, under the
handler's own `rs.mu` acquisition, and only then is the snapshot queued for the
applier. Anything that touches this path again must preserve that.

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
- **Snapshot Transfer (InstallSnapshot)**: The production binary wires a
  `raft.Snapshotter` (`persistence.NewRaftSnapshotter`), so a leader with a
  compacted log does send InstallSnapshot to lagging followers (A6, fixed). The
  snapshot it ships is one immutable `(index, term, data)` envelope read from a
  single `snapshot.json` load (R4, fixed). The receive path no longer blocks on
  the state machine either (B3, fixed). That envelope is shipped as a series of
  chunks (R15, fixed), which changes nothing about what reaches disk: the
  receiver assembles the payload in memory and only the final chunk triggers the
  `snapshot.json` write, followed by the `raft_state.json` boundary write, in
  that order — see `docs/log-compaction.md`.

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
Follower-side snapshot durability (A8) and snapshot-transfer wiring (A6) are
fixed, and so are the three snapshot-path findings of the 2026-09-06 re-audit:
generation-consistent persistence with a fail-closed startup check (R3), a single
immutable envelope on the send path (R4), and monotonicity guards on both
receivers (R5). The two files are still written separately — the guarantee comes
from their order and from the startup check, not from cross-file atomicity — but
the receive path no longer holds `rs.mu` across the `applyCh` send, so a slow
state machine cannot stall the node (B3, fixed). See ../KNOWN_ISSUES.md before
relying on recovery in compaction scenarios.
