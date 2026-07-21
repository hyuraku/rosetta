# Persistence Feature

This document describes the persistence feature implemented in Rosetta, which provides crash recovery and durability for the distributed key-value store.

## Overview

The persistence layer ensures that:
- Raft state (term, votedFor, log) survives node crashes
- KV store data is preserved across restarts
- System can recover from failures without data loss
- All writes are atomic and durable

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
- **Atomicity**: Uses atomic file writes (write to temp file, then rename)
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
Adapter that implements `kvstore.Snapshotter` interface:

```go
type Snapshotter interface {
    SaveSnapshot(data map[string]string, lastIncludedIndex, lastIncludedTerm int) error
    LoadSnapshot() (data map[string]string, lastIncludedIndex, lastIncludedTerm int, err error)
}
```

## Data Persistence

### What is Persisted

#### Raft State
- **CurrentTerm**: Current election term
- **VotedFor**: Candidate that received vote in current term
- **Log**: Complete log of commands

#### KV Store State
- **Data**: Complete key-value pairs
- **LastIncludedIndex**: Last log index included in snapshot
- **LastIncludedTerm**: Term of last included log entry

### When Data is Persisted

Raft state is persisted when:
1. Term is incremented
2. Vote is cast (VotedFor changes)
3. Log entry is appended
4. Log is truncated (on AppendEntries consistency check)

KV store snapshots can be saved:
- Manually via API call
- Periodically (if auto-snapshotting is enabled)
- Before shutdown (recommended)

## Usage

### Configuration

Set the data directory in your configuration:

```json
{
  "node_id": "node1",
  "data_dir": "./data",
  ...
}
```

Each node will create a subdirectory under `data_dir` named after its `node_id`.

### Startup Behavior

1. **First Start**: Node initializes with empty state
2. **Restart**: Node loads persisted state automatically
   - Raft state is restored (term, votedFor, log)
   - KV store snapshot is restored (if exists)
   - Node resumes operation from last known state

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
      "Term": 1,
      "Index": 1,
      "Command": "{\"op\":\"PUT\",\"key\":\"test\",\"value\":\"data\"}",
      "Type": "command"
    }
  ]
}
```

### Snapshot File Format

```json
{
  "last_included_index": 100,
  "last_included_term": 5,
  "data": "base64_encoded_kv_data"
}
```

## Crash Recovery

### Recovery Process

1. **Node Crashes**: Power failure, process killed, etc.
2. **Node Restarts**:
   - Storage layer opens data directory
   - Loads `raft_state.json` if exists
   - Loads `snapshot.json` if exists
3. **State Restoration**:
   - RaftState restores term, votedFor, and log
   - KVStore restores key-value data
4. **Resume Operation**: Node continues from recovered state

### Recovery Guarantees

- **No data loss**: All committed operations are persisted
- **Consistency**: State is always consistent (atomic writes)
- **Ordering**: Log entries maintain correct order
- **Idempotency**: Safe to restart multiple times

### Testing Recovery

Run the crash recovery tests:

```bash
# Unit tests
go test ./tests/unit/persistence_test.go -v

# Integration tests
go test ./tests/integration/persistence_integration_test.go -v
```

## Performance Considerations

### Write Performance

- Each state change triggers a write to disk
- Writes are atomic (temp file + rename)
- Consider using SSDs for better performance
- File system sync ensures durability

### Optimization Tips

1. **Use SSD**: Significantly faster than HDD
2. **Separate Disk**: Use dedicated disk for data directory
3. **File System**: ext4 or XFS recommended
4. **Snapshot Frequency**: Balance between safety and performance

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

### Automated Backups

Consider implementing automated backups:
- Periodic snapshots (every N minutes/hours)
- Copy to remote storage (S3, NFS, etc.)
- Keep multiple versions for point-in-time recovery

## Troubleshooting

### Corrupted State File

If state file is corrupted:

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

Implement log compaction to reduce disk usage.

### Permission Issues

Ensure data directory is writable:

```bash
chmod 755 ./data
chmod 644 ./data/node1/*
```

## Future Enhancements

Planned improvements:
1. **Log Compaction**: Remove old log entries after snapshotting
2. **Compression**: Compress snapshot data
3. **Incremental Snapshots**: Save only changed data
4. **Write-Ahead Log (WAL)**: More efficient append-only log
5. **Background Persistence**: Async writes for better performance
6. **Snapshot Streaming**: Transfer snapshots between nodes

## API Integration

### Check Persistence Status

```bash
# Get node status (includes persistence info)
curl http://localhost:9080/status
```

### Force Snapshot (Future)

```bash
# Trigger manual snapshot
curl -X POST http://localhost:9080/admin/snapshot
```

## References

- [Raft Paper](https://raft.github.io/raft.pdf) - Section 7: Log compaction
- [etcd Documentation](https://etcd.io/docs/) - Persistence implementation
- [RocksDB](https://rocksdb.org/) - High-performance persistent storage

## Security Considerations

1. **File Permissions**: Restrict access to data directory
2. **Encryption at Rest**: Consider encrypting sensitive data
3. **Backup Security**: Secure backup files appropriately
4. **Audit Logs**: Log all persistence operations

## Monitoring

Metrics to monitor:
- Disk usage of data directory
- Write latency to disk
- Number of persistence operations
- Snapshot size over time
- Recovery time after crash

## Conclusion

The persistence feature provides crucial durability guarantees for Rosetta. By persisting both Raft state and KV store data, the system can recover from crashes without data loss while maintaining consistency across the cluster.
