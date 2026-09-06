package persistence

import "fmt"

// SnapshotConsistency reports how a node's two durable files relate to each
// other at startup: the Raft state file (raft_state.json), which carries the
// snapshot boundary the log was compacted to, and the snapshot file
// (snapshot.json), which carries the state machine payload.
type SnapshotConsistency struct {
	// RaftLastIncludedIndex is the boundary recorded in the Raft state file.
	RaftLastIncludedIndex int

	// SnapshotLastIncludedIndex is the index the persisted snapshot covers.
	// Zero when no snapshot has been written yet.
	SnapshotLastIncludedIndex int

	// SnapshotPresent reports whether a snapshot file exists at all.
	SnapshotPresent bool

	// CompactionPending reports that the snapshot is ahead of the Raft
	// boundary, i.e. a compaction did not finish before the last shutdown. This
	// is the recoverable direction: the node replays the log from the older
	// boundary and the state machine ignores everything at or below the index
	// its own snapshot already covers.
	CompactionPending bool
}

// VerifySnapshotConsistency compares the two files a node recovers from and
// refuses the unrecoverable combination (KNOWN_ISSUES.md R3).
//
// The Raft state and the KV snapshot are separate files with no cross-file
// atomicity, so a crash can leave them at different generations. Only one
// direction is survivable:
//
//   - snapshot ahead of Raft — recoverable. Raft still holds the log below the
//     snapshot's index and replays it; the state machine drops the replayed
//     prefix because it has already applied through that index. This is the
//     direction the InstallSnapshot receive path deliberately falls into by
//     persisting the payload before the boundary.
//
//   - Raft ahead of snapshot — unrecoverable, so startup is refused. Raft
//     believes everything up to its boundary is folded into the state machine
//     and has discarded those log entries, but the state machine on disk stops
//     short of it. Nothing can ever deliver the missing entries again, and the
//     node would silently serve incomplete data. Failing closed (like C4's
//     refusal to start on an unreadable state file) leaves the operator with a
//     node that can be repaired from a peer instead of a quietly wrong one.
//
// It is deliberately placed here rather than in raft or kvstore: those two
// construct independently in tests, and only the caller that owns the shared
// Storage (main.go) sees both files.
func VerifySnapshotConsistency(storage Storage) (SnapshotConsistency, error) {
	var result SnapshotConsistency

	state, err := storage.LoadRaftState()
	if err != nil {
		return result, fmt.Errorf("cannot read raft state: %w", err)
	}
	if state != nil {
		result.RaftLastIncludedIndex = state.LastIncludedIndex
	}

	snapshot, err := storage.LoadSnapshot()
	if err != nil {
		return result, fmt.Errorf("cannot read snapshot: %w", err)
	}
	if snapshot != nil {
		result.SnapshotPresent = true
		result.SnapshotLastIncludedIndex = snapshot.LastIncludedIndex
	}

	if result.RaftLastIncludedIndex > result.SnapshotLastIncludedIndex {
		return result, fmt.Errorf(
			"raft state was compacted to index %d but the persisted snapshot only covers index %d: "+
				"the log entries between them are gone and the state machine can never be caught up "+
				"(KNOWN_ISSUES.md R3); restore this node's data directory from a peer",
			result.RaftLastIncludedIndex, result.SnapshotLastIncludedIndex)
	}

	result.CompactionPending = result.SnapshotLastIncludedIndex > result.RaftLastIncludedIndex
	return result, nil
}
