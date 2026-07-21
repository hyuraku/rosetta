# Log Compaction and Snapshotting

> Last verified: 2026-07-21 against commit `9383cfe`.

This document describes the log compaction and snapshotting features in Rosetta, which are intended to prevent unbounded log growth and enable efficient operation over long periods.

> **Warning**: Log compaction is only partially implemented. Snapshot creation and log truncation are wired up and fire automatically (default threshold: 1000 applied commands), but only the leader's *sending* path translates between absolute (snapshot-inclusive) and relative (post-truncation) log indices. The AppendEntries/RequestVote handlers, commit advancement, and the apply path still use raw slice positions, so once the log is actually truncated, replication, elections, and commits break. InstallSnapshot is also unwired in the production binary. See ../KNOWN_ISSUES.md (A1-A8). Treat this document as a description of the design intent, with per-section notes on what actually works today.

## Overview

Without log compaction, the Raft log would grow without bound, eventually consuming all available disk space and memory. Snapshotting solves this problem by periodically creating a compact representation of the state machine and discarding old log entries.

## How It Works

### Basic Concept

```
Before Snapshot:
Log: [Entry1, Entry2, Entry3, ... Entry1000]
State: {key1: value1, key2: value2, ...}

After Snapshot:
Snapshot: {key1: value1, key2: value2, ...} @ index=1000, term=5
Log: [Entry1001, Entry1002, ...]  (only recent entries)
```

### Automatic Snapshotting

Rosetta automatically creates snapshots when the log grows beyond a configured threshold:

1. **Trigger**: After N commands are applied (`maxRaftState` in config)
2. **Create**: KV store serializes its current state
3. **Save**: Snapshot is persisted to disk
4. **Compact**: Old log entries are discarded (via `RaftState.TruncateLogTo`)
5. **Continue**: Normal operation resumes with smaller log

> **Warning**: Steps 1-4 work as described, but step 5 does not: after truncation the receiver-side RPC handlers, vote comparisons, and commit logic still interpret indices as slice positions, so replication and elections misbehave. See ../KNOWN_ISSUES.md (A1-A5).

## Configuration

### Snapshot Thresholds

Configure when snapshots are taken:

```json
{
  "max_raft_state": 1000,      // Take snapshot after 1000 commands
  "snapshot_interval": 100      // (Reserved for future use)
}
```

Default values:
- `maxRaftState`: 1000 commands
- Automatic snapshots enabled by default

This means log compaction fires automatically during normal operation once 1000 commands have been applied — at which point the known index-handling issues take effect (see ../KNOWN_ISSUES.md, A1-A5).

### Disable Automatic Snapshots

Automatic snapshots **cannot be disabled through configuration**: `Config.Validate()` (config/config.go) rejects `max_raft_state <= 0` with `"max_raft_state must be positive"`, so a config file containing `"max_raft_state": 0` prevents the node from starting. There is also no command-line flag for it, so nodes started with flags always run with the default of 1000.

Snapshotting can only be disabled programmatically, by constructing the store with `kvstore.NewKVStore(0)` (the KV store skips snapshots when `maxRaftState <= 0`). Given the known compaction issues (see ../KNOWN_ISSUES.md, A1-A8), disabling compaction this way is currently the safer mode of operation.

## Architecture

### Components

#### 1. Snapshot Metadata (raft/state.go)

```go
type PersistentState struct {
    CurrentTerm int
    VotedFor    *string
    Log         []LogEntry

    // Snapshot metadata
    LastIncludedIndex int  // Last log index in snapshot
    LastIncludedTerm  int  // Term of last entry in snapshot
}
```

#### 2. Snapshot Operations (raft/snapshot.go)

- `TakeSnapshot()`: Create snapshot and compact log (exists, but not used on the automatic path)
- `TruncateLogTo()`: Compact the log up to an index whose snapshot was already persisted elsewhere; this is what the automatic path (`RaftNode.TriggerSnapshot`) actually calls
- `InstallSnapshotFromData()`: Install received snapshot
- `GetSnapshotMetadata()`: Get current snapshot info
- `ShouldTakeSnapshot()`: Check if snapshot is needed

#### 3. InstallSnapshot RPC (raft/rpc.go)

Transfers snapshots between nodes:

```go
type InstallSnapshotArgs struct {
    Term              int
    LeaderID          string
    LastIncludedIndex int
    LastIncludedTerm  int
    Data              []byte  // Serialized state machine
}
```

> **Warning**: In the production binary this RPC is never sent. The leader-side send path (`sendSnapshotToPeer` in raft/rpc.go) requires a `raft.Snapshotter` wired via `RaftNode.SetSnapshotter`, but main.go never calls it (and `persistence.KVSnapshotter` does not implement the `raft.Snapshotter` interface), so the leader silently skips the send and a follower behind the compaction boundary can never catch up. See ../KNOWN_ISSUES.md (A6).

#### 4. KV Store Integration (kvstore/store.go)

- Automatic snapshot creation after N commands
- Snapshot installation from apply channel (implemented, but broken: it unmarshals the data as a plain `map[string]string`, which fails for the V2 format `{"kv_data":...,"sessions":...}` that production snapshots are saved in, and it never persists the installed snapshot to disk — see ../KNOWN_ISSUES.md (A6, A8))
- State serialization/deserialization

## Snapshot Lifecycle

### 1. Creation Process

```
[KV Store applies command]
       ↓
[commandsSinceSnapshot++]
       ↓
[Check: commandsSinceSnapshot >= maxRaftState?]
       ↓ Yes
[Serialize KV data to JSON]
       ↓
[Save snapshot to disk via persistence layer]
       ↓
[Notify Raft to compact log]
       ↓
[Raft truncates old log entries]
       ↓
[commandsSinceSnapshot = 0]
```

### 2. Installation Process

When a node is far behind or joins the cluster, the design intent is:

```
[Leader detects follower is too far behind]
       ↓
[Leader sends InstallSnapshot RPC]
       ↓
[Follower receives snapshot]
       ↓
[Follower discards conflicting log entries]
       ↓
[Follower updates snapshot metadata]
       ↓
[Follower sends snapshot to apply channel]
       ↓
[KV Store installs snapshot data]
       ↓
[Follower catches up with recent entries]
```

> **Warning**: This flow does not currently work end to end. (a) The leader never sends the RPC in production because no `raft.Snapshotter` is wired (A6). (b) The receiver keeps log entries after `LastIncludedIndex` without checking their terms, so a diverged suffix from an old leader can survive (A7). (c) Even when the RPC is delivered (e.g. in tests), the KV store fails to unmarshal V2-format snapshot data, and the Raft layer has already truncated its log and advanced CommitIndex/LastApplied by then (A6). (d) The installed snapshot is never written to disk on the follower, so a crash right after installation loses the state permanently (A8). See ../KNOWN_ISSUES.md (A6, A7, A8).

### 3. Recovery Process

On node restart:

```
[Node starts]
       ↓
[Load snapshot from disk]
       ↓
[Restore KV data from snapshot]
       ↓
[Load remaining log entries]
       ↓
[Apply log entries after snapshot]
       ↓
[Node fully recovered]
```

> **Warning**: The last two steps are not implemented correctly. The KV store does restore its data from `snapshot.json`, and Raft reloads `raft_state.json` (including the snapshot metadata), but the volatile state is reinitialized to `CommitIndex=0, LastApplied=0` instead of `LastIncludedIndex` (raft/state.go), and `applyEntries` indexes the log as `Log[LastApplied-1]` — a slice position, not an absolute index. After a restart with a compacted log this leads to re-application of entries already in the snapshot, mis-indexed applies, or an out-of-range panic. See ../KNOWN_ISSUES.md (A5).

## File Structure

Snapshot data is stored alongside Raft state:

```
data/
└── node1/
    ├── raft_state.json    # Includes snapshot metadata
    └── snapshot.json      # KV store snapshot
```

### raft_state.json with Snapshot

```json
{
  "CurrentTerm": 10,
  "VotedFor": null,
  "Log": [
    {
      "term": 10,
      "index": 1001,
      "command": "...",
      "type": "command"
    }
  ],
  "LastIncludedIndex": 1000,
  "LastIncludedTerm": 9
}
```

### snapshot.json

```json
{
  "last_included_index": 1000,
  "last_included_term": 9,
  "data": "eyJrdl9kYXRhIjp7ImtleTEiOiJ2YWx1ZTEiLCJrZXkyIjoidmFsdWUyIn19"
}
```

The `data` field is base64-encoded JSON. Because `persistence.KVSnapshotter` implements the V2 interface, production snapshots use the V2 payload `{"kv_data": {...}, "sessions": {...}}` (the example above decodes to `{"kv_data":{"key1":"value1","key2":"value2"}}`), not a plain key-value map. Note also that `last_included_term` is typically `0` in practice: the KV store's apply loop never updates `lastAppliedTerm`, so the term written into the snapshot is only non-zero if it was itself restored from an earlier snapshot.

## Performance Impact

### Benefits

1. **Reduced Disk Usage**: Log doesn't grow indefinitely
2. **Faster Recovery**: Nodes replay fewer entries on startup
3. **Lower Memory**: Smaller log in memory
4. **Faster Catch-up**: New nodes receive snapshot instead of entire log

### Trade-offs

1. **Snapshot Overhead**: Creating snapshot takes CPU and I/O
2. **Disk Writes**: Additional writes for snapshot files
3. **Network Bandwidth**: Large snapshots consume bandwidth

### Optimization Tips

1. **Tune Threshold**: Balance between snapshot frequency and log size
   - Too frequent: High overhead
   - Too infrequent: Large logs

2. **Monitoring**: Track snapshot metrics
   - Snapshot creation time
   - Snapshot size
   - Log size before/after

3. **Hardware**: Use SSD for better snapshot I/O

## Example Usage

### Check Snapshot Status

```bash
# Get node status (includes log size)
curl http://localhost:9080/status

# Response:
{
  "node_id": "node1",
  "term": 10,
  "is_leader": true,
  "log_size": 150  # Entries after snapshot
}
```

### Monitor Logs

On the automatic path the actual log lines look like this (the `Snapshot taken:` line belongs to the unused `TakeSnapshot` code path and does not appear):

```
[KVSTORE] Saved snapshot V2: kvEntries=500, sessions=0, lastIndex=1000, lastTerm=0
[RAFT-node1] Snapshot trigger received for index 1000
[RAFT-STATE-node1] Log truncated up to index 1000 (term 9), remaining log size 0
```

## Implementation Details

### Log Index Adjustment

After snapshot, log indices are adjusted:

```
Before Snapshot:
Log indices: [1, 2, 3, ..., 1000, 1001, 1002]

After Snapshot (lastIncludedIndex=1000):
Log indices: [1001, 1002, ...]
               ↑ Still starts from actual index, not 0!
```

> **Warning**: Only the entries' `Index` *fields* keep their absolute values, and only the leader-side replication code (`replicateToPeer` in raft/rpc.go) translates absolute indices into post-truncation slice positions. The AppendEntries/RequestVote handlers, `updateCommitIndex`, and `applyEntries` still treat `len(Log)` and slice offsets as the log index, so every other code path disagrees with this diagram after compaction. See ../KNOWN_ISSUES.md (A1, A2, A4, A5).

### Concurrent Snapshots

- Only one snapshot operation at a time
- New commands continue to be applied during snapshot
- Snapshot captures state at specific index

### Snapshot Transfer

InstallSnapshot RPC is intended to be used when:
- Follower's nextIndex <= leader's lastIncludedIndex
- New node joins cluster
- Node recovers from long partition

> **Warning**: With the default main.go wiring the leader detects these cases but never actually sends the RPC, because no `raft.Snapshotter` is registered (`sendSnapshotToPeer` returns immediately when the snapshotter is nil). A leader that has compacted its log therefore cannot even heartbeat such followers. See ../KNOWN_ISSUES.md (A3, A6).

## Troubleshooting

### Snapshot Not Being Created

Check:
1. `maxRaftState` configuration
2. Commands are being applied
3. Disk space available
4. Log files for errors

### Large Snapshot Files

- Normal if KV store has many keys
- Consider compression (future enhancement)
- Monitor snapshot size growth

### Slow Recovery

If recovery is slow:
1. Check snapshot size
2. Verify disk I/O performance
3. Consider SSD upgrade
4. Review log between snapshots

## Future Enhancements

Planned improvements:

1. **Compression**: Compress snapshot data
2. **Incremental Snapshots**: Only save changed data
3. **Streaming**: Stream large snapshots in chunks
4. **Background Creation**: Async snapshot without blocking
5. **Configurable Triggers**: Time-based or size-based
6. **Snapshot Verification**: Checksum validation

## References

- [Raft Paper Section 7: Log compaction](https://raft.github.io/raft.pdf)
- [Raft Dissertation Chapter 5: Log compaction](https://github.com/ongardie/dissertation)
- [etcd Snapshotting](https://etcd.io/docs/v3.5/op-guide/maintenance/#snapshot-backup)

## Testing

### Unit Tests

```bash
go test ./tests/unit -run Snapshot -v
```

Tests (tests/unit/snapshot_test.go) cover:
- Snapshot metadata tracking
- Snapshot triggering logic (`ShouldTakeSnapshot`)
- InstallSnapshot RPC
- Serialization/deserialization

### Integration Tests

One integration test exists, `TestInstallSnapshotCatchUp` in tests/integration/snapshot_compaction_test.go. It covers leader-side log compaction (`TriggerSnapshot`/`TruncateLogTo`) and snapshot transfer to a late-joining follower — but only with a mock `raft.Snapshotter` wired manually via `SetSnapshotter`, which the production binary does not do (see ../KNOWN_ISSUES.md, A6). Automatic snapshot creation through the KV store and recovery from a snapshot after restart are not covered by integration tests.

## Monitoring Metrics

Key metrics to track:

- `snapshot_count`: Total snapshots created
- `snapshot_size_bytes`: Size of latest snapshot
- `snapshot_duration_ms`: Time to create snapshot
- `log_entries_before_snapshot`: Log size before compaction
- `log_entries_after_snapshot`: Log size after compaction
- `snapshot_transfer_count`: InstallSnapshot RPCs sent

(Metrics implementation planned for monitoring feature)

## Conclusion

Log compaction through snapshotting is essential for long-term operation of a Raft system, and the building blocks (snapshot persistence, log truncation, InstallSnapshot RPC) exist in Rosetta. However, the implementation is currently incomplete: index handling after truncation, InstallSnapshot wiring, follower-side installation, and post-restart recovery all have known defects that break Raft's safety guarantees once compaction fires (see ../KNOWN_ISSUES.md, A1-A8). Rosetta is a learning-oriented implementation; until these are fixed, compaction should be considered unsafe to rely on.
