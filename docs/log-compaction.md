# Log Compaction and Snapshotting

> Last verified: 2026-09-06 against commit `980f43d`.

This document describes the log compaction and snapshotting features in Rosetta, which are intended to prevent unbounded log growth and enable efficient operation over long periods.

> **Status**: The index/wiring defects that made compaction unusable are fixed. Absolute-index handling is unified across the receive, vote, commit, and apply paths (A1-A5, `8ad5367`), the snapshotter is wired in the production binary and the V2 snapshot format parses (A6, `d0cbdc1`/`c516f54`), follower-side snapshots are persisted (A8, `c516f54`), and the InstallSnapshot receiver applies the paper's §7 retention rule instead of keeping a divergent suffix (A7, `019d33e`). The three **safety** gaps a 2026-09-06 re-audit (`docs/raft-audit-2026-09-06.md`, frozen) found on this same path are fixed too: the receive path now persists the KV payload before the Raft boundary and startup refuses an unrecoverable pair (R3, `0695b95`); the leader ships one immutable `(index, term, data)` envelope (R4, `f53617e`); and both receivers refuse a snapshot at or below what they have already applied (R5, `156510a`). The **liveness** gap on the same path (B3) is fixed as well (`f873d9b`): the handler still writes the payload under `rs.mu`, but it queues the snapshot for a dedicated applier goroutine instead of sending on `applyCh` with the lock held, so a slow state machine no longer stalls RPCs or the election timer. See ../KNOWN_ISSUES.md.

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

> **Note**: Step 5 used to break here — after truncation the receiver-side RPC handlers, vote comparisons, and commit logic interpreted absolute indices as slice positions. `8ad5367` unified all of them on absolute indices via the `slicePos`/`logTermAt` helpers in raft/log.go (A1-A5, now fixed).

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

This means log compaction fires automatically during normal operation once 1000 commands have been applied. The index-handling defects that this used to expose are fixed (see ../KNOWN_ISSUES.md, A1-A5).

### Disable Automatic Snapshots

Automatic snapshots **cannot be disabled through configuration**: `Config.Validate()` (config/config.go) rejects `max_raft_state <= 0` with `"max_raft_state must be positive"`, so a config file containing `"max_raft_state": 0` prevents the node from starting. There is also no command-line flag for it, so nodes started with flags always run with the default of 1000.

Snapshotting can only be disabled programmatically, by constructing the store with `kvstore.NewKVStore(0)` (the KV store skips snapshots when `maxRaftState <= 0`). This is mainly useful in tests that want a single, uncompacted log to assert against — group A's index/wiring defects and the re-audit's R3–R5 are all fixed, so it is no longer a way to avoid a known safety issue.

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

> **Note**: This RPC used to be dead code in the production binary — the leader-side send path (`sendSnapshotToPeer` in raft/rpc.go) needs a `raft.Snapshotter` and main.go never registered one. `d0cbdc1` wires `persistence.NewRaftSnapshotter` through `raftNode.SetSnapshotter` (main.go:255, 273), so the send fires in production (A6, fixed).

#### 4. KV Store Integration (kvstore/store.go)

- Automatic snapshot creation after N commands
- Snapshot installation from apply channel — `parseSnapshotBytes` (kvstore/store.go) accepts the V2 format `{"kv_data":...,"sessions":...}` and falls back to the legacy bare `map[string]string`, and `installSnapshotFromApplyMsg` persists the installed snapshot when a snapshotter is configured (A6, A8, fixed in `c516f54`)
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

> **Note**: This flow works end to end for the four defects it used to have: the production snapshotter is wired so the leader actually sends (A6), the receiver applies the §7 retention rule instead of keeping a divergent suffix (A7, `019d33e` — see "Follower discards conflicting log entries" above, implemented by `logAfterSnapshot` in raft/log.go), the KV store parses the V2 snapshot format (A6), and the installed snapshot is persisted on the follower (A8). The 2026-09-06 re-audit's safety findings on this flow are fixed as well: the leader ships metadata and payload as one generation (R4), both receivers refuse a snapshot at or below what they have already applied (R5), and the payload is made durable before the Raft boundary so a crash falls on the recoverable side (R3). The liveness gap on the same path is closed too: B3 (`f873d9b`) moved the `applyCh` hand-off to a dedicated applier goroutine, so the handler no longer holds `rs.mu` across a send the state machine may be slow to take. See ../KNOWN_ISSUES.md.

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

> **Note**: These last two steps used to be wrong: volatile state was reinitialized to `CommitIndex=0, LastApplied=0` instead of the snapshot boundary, and `applyEntries` indexed the log by slice position. `8ad5367` makes `loadPersistentState` restore both from `LastIncludedIndex` (raft/state.go) and routes the apply path through `slicePos` with a range guard (A5, fixed). That apply path now lives in `takeApplyWork` (raft/applier.go), which kept the same guard when the applier was split out (B3, `f873d9b`).

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

> **Note**: Every path now agrees with this diagram. Absolute indices are the single representation throughout the package, and the conversion to a post-truncation slice position happens only through the `slicePos`/`logTermAt`/`lastAbsLogIndex` helpers in raft/log.go — used by the AppendEntries/RequestVote handlers, `updateCommitIndex`, and the applier's `takeApplyWork` alike (A1, A2, A4, A5, fixed in `8ad5367`).

### Concurrent Snapshots

- Only one snapshot operation at a time
- New commands continue to be applied during snapshot
- Snapshot captures state at specific index

### Snapshot Transfer

InstallSnapshot RPC is intended to be used when:
- Follower's nextIndex <= leader's lastIncludedIndex
- New node joins cluster
- Node recovers from long partition

> **Note**: `sendSnapshotToPeer` still returns immediately when no snapshotter is registered, but main.go registers one (`raftNode.SetSnapshotter`), so a leader with a compacted log does send to followers behind the boundary (A3, A6, fixed).
>
> Everything the RPC asserts about the snapshot comes from one envelope (R4, fixed in `f53617e`): `Snapshotter.ReadSnapshot` returns a `*raft.SnapshotData` holding `LastIncludedIndex`, `LastIncludedTerm` and `Data` from a single atomic `snapshot.json` read, and `sendSnapshotToPeer` builds `InstallSnapshotArgs` — and the follower's `MatchIndex`/`NextIndex` on success — from that value alone. The boundary `replicateToPeer` samples under `rs.mu.RLock` is used only to decide *whether* to send a snapshot; `nextIndex` is carried across so a snapshot ending before the entry the follower already has is skipped rather than shipped (it would drag that follower's match index backwards). Previously the boundary came from the sampled state and the bytes from a later, unlocked read, so a compaction in between produced an RPC claiming one generation's boundary for another's payload.

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
3. **Streaming**: Stream large snapshots in chunks. `InstallSnapshotArgs` currently
   carries the whole snapshot in one `Data []byte` field with no offset/done
   fields for chunking, resumption, or bounding memory use on large snapshots —
   see ../KNOWN_ISSUES.md (R15)
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

`TestInstallSnapshotCatchUp` (tests/integration/snapshot_compaction_test.go) covers leader-side log compaction (`TriggerSnapshot`/`TruncateLogTo`) and snapshot transfer to a late-joining follower, using a mock `raft.Snapshotter`. `tests/integration/snapshot_kvstore_wiring_test.go` covers the production wiring path that the mock used to bypass, and the receiver's §7 retention rule is covered by `TestInstallSnapshotDiscardsDivergentSuffix` and its siblings in raft/installsnapshot_internal_test.go. Automatic snapshot creation through the KV store and recovery from a snapshot after restart are still not covered by integration tests.

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

Log compaction through snapshotting is essential for long-term operation of a Raft system, and Rosetta implements it: snapshot persistence, log truncation, the InstallSnapshot RPC, unified absolute indexing, production wiring, follower-side persistence, and the §7 retention rule on the receiver (A1-A8, all fixed). The 2026-09-06 re-audit's three safety findings on this path are fixed as well (R3, R4, R5), and so is the liveness defect: the InstallSnapshot handler queues the snapshot for the applier goroutine rather than sending on `applyCh` under `rs.mu` (B3, `f873d9b`) — see ../KNOWN_ISSUES.md. What is still missing here is chunked transfer (R15). Rosetta is a learning-oriented implementation and is not production-ready.
