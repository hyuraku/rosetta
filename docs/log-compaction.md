# Log Compaction and Snapshotting

This document describes the log compaction and snapshotting features in Rosetta, which prevent unbounded log growth and enable efficient operation over long periods.

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
4. **Compact**: Old log entries are discarded
5. **Continue**: Normal operation resumes with smaller log

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

### Disable Automatic Snapshots

Set `maxRaftState` to 0 to disable:

```json
{
  "max_raft_state": 0
}
```

**Warning**: Only disable for testing! Production systems should always use snapshots.

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

- `TakeSnapshot()`: Create snapshot and compact log
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

#### 4. KV Store Integration (kvstore/store.go)

- Automatic snapshot creation after N commands
- Snapshot installation from apply channel
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

When a node is far behind or joins the cluster:

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
      "Term": 10,
      "Index": 1001,
      "Command": "...",
      "Type": "command"
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
  "data": "eyJrZXkxIjoidmFsdWUxIiwia2V5MiI6InZhbHVlMiJ9"
}
```

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

```
[KVSTORE] Saved snapshot: entries=500, lastIndex=1000, lastTerm=9
[RAFT-STATE-node1] Snapshot taken: lastIndex=1000, lastTerm=9, logSize=0, snapshotSize=5120
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

### Concurrent Snapshots

- Only one snapshot operation at a time
- New commands continue to be applied during snapshot
- Snapshot captures state at specific index

### Snapshot Transfer

InstallSnapshot RPC is used when:
- Follower's nextIndex < leader's lastIncludedIndex
- New node joins cluster
- Node recovers from long partition

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
go test ./tests/unit/snapshot_test.go -v
```

Tests cover:
- Snapshot metadata tracking
- Snapshot triggering logic
- InstallSnapshot RPC
- Serialization/deserialization

### Integration Tests

Test snapshot in real cluster scenarios:
- Automatic snapshot creation
- Log compaction after snapshot
- Recovery from snapshot
- Snapshot transfer between nodes

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

Log compaction through snapshotting is essential for long-term operation of Rosetta. It ensures the system remains efficient even after millions of operations while maintaining full consistency guarantees.
