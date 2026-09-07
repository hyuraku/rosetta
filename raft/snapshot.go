package raft

import (
	"fmt"
	"log"
)

// SnapshotData is one snapshot generation: the payload together with the log
// position it was taken at. The three fields belong to each other and must
// travel as a unit — the InstallSnapshot RPC promises the receiver that Data is
// the state machine as of (LastIncludedIndex, LastIncludedTerm), and a receiver
// that installs a payload under someone else's boundary corrupts its state
// machine silently (KNOWN_ISSUES.md R4).
//
// Treat a *SnapshotData as immutable once returned: producers hand out a value
// that no longer aliases their own storage, and consumers must not write to
// Data.
type SnapshotData struct {
	LastIncludedIndex int
	LastIncludedTerm  int
	Data              []byte
}

// Snapshotter interface for creating and installing snapshots
type Snapshotter interface {
	// CreateSnapshot creates a snapshot of the state machine up to the given index
	CreateSnapshot(lastIncludedIndex, lastIncludedTerm int) ([]byte, error)

	// InstallSnapshot durably stores a snapshot received from a leader. On the
	// InstallSnapshot receive path this is what makes the state machine payload
	// durable, and it must succeed before the Raft snapshot boundary is
	// persisted (see the ordering invariant on RaftState.InstallSnapshot).
	InstallSnapshot(data []byte, lastIncludedIndex, lastIncludedTerm int) error

	// ReadSnapshot returns the current persisted snapshot as a single immutable
	// (index, term, data) envelope. The leader calls this when sending an
	// InstallSnapshot RPC to a lagging follower and takes the RPC's
	// LastIncludedIndex/LastIncludedTerm from the returned value, never from a
	// separately sampled copy of the Raft boundary.
	// Returns (nil, nil) when no snapshot has been taken yet.
	ReadSnapshot() (*SnapshotData, error)
}

// TakeSnapshot creates a snapshot and truncates the log
func (rs *RaftState) TakeSnapshot(lastIncludedIndex int, snapshotter Snapshotter) error {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	// Validate index
	if lastIncludedIndex <= rs.persistent.LastIncludedIndex {
		return nil // Already have a newer snapshot
	}

	if lastIncludedIndex > len(rs.persistent.Log)+rs.persistent.LastIncludedIndex {
		log.Printf("Warning: snapshot index %d beyond log end %d",
			lastIncludedIndex, len(rs.persistent.Log)+rs.persistent.LastIncludedIndex)
		return nil
	}

	// Get the term of the last included entry
	var lastIncludedTerm int
	logIndex := lastIncludedIndex - rs.persistent.LastIncludedIndex
	if logIndex > 0 && logIndex <= len(rs.persistent.Log) {
		lastIncludedTerm = rs.persistent.Log[logIndex-1].Term
	} else {
		lastIncludedTerm = rs.persistent.LastIncludedTerm
	}

	// Create snapshot through the snapshotter
	snapshotData, err := snapshotter.CreateSnapshot(lastIncludedIndex, lastIncludedTerm)
	if err != nil {
		return err
	}

	// Record the configuration the boundary is taken under before the entries
	// carrying it are discarded (see TruncateLogTo).
	boundaryConfig := rs.configAtIndexLocked(lastIncludedIndex)
	rs.persistent.SnapshotConfig = boundaryConfig

	// Truncate log - keep only entries after snapshot
	entriesToKeep := lastIncludedIndex - rs.persistent.LastIncludedIndex
	if entriesToKeep < len(rs.persistent.Log) {
		rs.persistent.Log = rs.persistent.Log[entriesToKeep:]
		// Adjust log indices
		for i := range rs.persistent.Log {
			rs.persistent.Log[i].Index = lastIncludedIndex + i + 1
		}
	} else {
		rs.persistent.Log = make([]LogEntry, 0)
	}

	// Update snapshot metadata
	rs.persistent.LastIncludedIndex = lastIncludedIndex
	rs.persistent.LastIncludedTerm = lastIncludedTerm

	// Persist the updated state
	if err := rs.persist(); err != nil {
		return fmt.Errorf("failed to persist state after taking snapshot: %w", err)
	}

	rs.logger.Printf("Snapshot taken: lastIndex=%d, lastTerm=%d, logSize=%d, snapshotSize=%d",
		lastIncludedIndex, lastIncludedTerm, len(rs.persistent.Log), len(snapshotData))

	return nil
}

// InstallSnapshotFromData installs a snapshot from raw data
func (rs *RaftState) InstallSnapshotFromData(lastIncludedIndex, lastIncludedTerm int, data []byte, snapshotter Snapshotter) error {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	// Don't install a snapshot that would move this node backwards — neither
	// behind the current snapshot boundary nor behind what the state machine has
	// already applied (KNOWN_ISSUES.md R5, same rule as the RPC handler).
	if lastIncludedIndex <= rs.volatile.LastApplied ||
		lastIncludedIndex <= rs.persistent.LastIncludedIndex {
		return nil
	}

	// Install snapshot into state machine
	if err := snapshotter.InstallSnapshot(data, lastIncludedIndex, lastIncludedTerm); err != nil {
		return err
	}

	// Discard any log entries covered by the snapshot
	if lastIncludedIndex >= rs.persistent.LastIncludedIndex+len(rs.persistent.Log) {
		// Snapshot covers entire log
		rs.persistent.Log = make([]LogEntry, 0)
	} else {
		// Keep entries after snapshot
		entriesToDiscard := lastIncludedIndex - rs.persistent.LastIncludedIndex
		if entriesToDiscard > 0 && entriesToDiscard < len(rs.persistent.Log) {
			rs.persistent.Log = rs.persistent.Log[entriesToDiscard:]
		}
	}

	// Update snapshot metadata
	rs.persistent.LastIncludedIndex = lastIncludedIndex
	rs.persistent.LastIncludedTerm = lastIncludedTerm

	// This path installs a snapshot the caller already holds, with no cluster
	// configuration attached — unlike the InstallSnapshot RPC, which carries the
	// sender's boundary configuration. SnapshotConfig therefore stays as it was,
	// and only the configuration in effect is re-derived from whatever log
	// survived. Callers that need the configuration transferred must use the RPC
	// path (KNOWN_ISSUES.md R14).
	rs.recomputeConfigLocked()

	// Update volatile state
	if rs.volatile.CommitIndex < lastIncludedIndex {
		rs.volatile.CommitIndex = lastIncludedIndex
	}
	if rs.volatile.LastApplied < lastIncludedIndex {
		rs.volatile.LastApplied = lastIncludedIndex
	}

	// Persist the updated state
	if err := rs.persist(); err != nil {
		return fmt.Errorf("failed to persist state after installing snapshot: %w", err)
	}

	rs.logger.Printf("Snapshot installed: lastIndex=%d, lastTerm=%d, logSize=%d",
		lastIncludedIndex, lastIncludedTerm, len(rs.persistent.Log))

	return nil
}

// GetSnapshotMetadata returns the current snapshot metadata
func (rs *RaftState) GetSnapshotMetadata() (lastIncludedIndex, lastIncludedTerm int) {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.persistent.LastIncludedIndex, rs.persistent.LastIncludedTerm
}

// TruncateLogTo discards log entries up to and including absoluteIndex,
// updating snapshot metadata so future replication uses the new boundary.
//
// Unlike TakeSnapshot, this method does NOT call Snapshotter.CreateSnapshot:
// it assumes the state machine (e.g. kvstore) has already persisted its
// snapshot bytes by some other route. This avoids double-marshaling the
// state machine on every compaction trigger.
//
// The boundary entry's term is read from the live log before truncation.
// If absoluteIndex falls outside the current log range the call becomes a no-op
// (idempotent: safe to invoke from concurrent triggers).
func (rs *RaftState) TruncateLogTo(absoluteIndex int) error {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	if absoluteIndex <= rs.persistent.LastIncludedIndex {
		return nil // already truncated past this point
	}

	logEnd := rs.persistent.LastIncludedIndex + len(rs.persistent.Log)
	if absoluteIndex > logEnd {
		return fmt.Errorf("truncate index %d beyond log end %d", absoluteIndex, logEnd)
	}

	// Term of the entry that becomes the new LastIncludedTerm.
	sliceIdx := absoluteIndex - rs.persistent.LastIncludedIndex - 1
	boundaryTerm := rs.persistent.Log[sliceIdx].Term

	// Discard entries up to and including the boundary. The pre-truncation
	// values are kept so a failed persist can be undone: the truncation only
	// moves the slice header forward, leaving the discarded entries in the
	// backing array, and rs.mu is held throughout (same rollback discipline as
	// TruncateLogAfter and the AppendEntries merge, KNOWN_ISSUES.md R2/R3).
	prevLog := rs.persistent.Log
	prevLastIncludedIndex := rs.persistent.LastIncludedIndex
	prevLastIncludedTerm := rs.persistent.LastIncludedTerm
	prevSnapshotConfig := rs.persistent.SnapshotConfig

	// The configuration in effect at the new boundary has to be recorded before
	// the entries that carry it are discarded: it becomes what the log reverts to
	// once no configuration entry is left in it, and what a lagging follower is
	// told when this boundary is shipped as a snapshot (KNOWN_ISSUES.md R14).
	// Config itself is unchanged — the entries above the boundary, and therefore
	// the latest configuration entry, survive.
	boundaryConfig := rs.configAtIndexLocked(absoluteIndex)
	rs.persistent.SnapshotConfig = boundaryConfig

	discarded := absoluteIndex - rs.persistent.LastIncludedIndex
	rs.persistent.Log = rs.persistent.Log[discarded:]

	rs.persistent.LastIncludedIndex = absoluteIndex
	rs.persistent.LastIncludedTerm = boundaryTerm
	if err := rs.persist(); err != nil {
		rs.persistent.Log = prevLog
		rs.persistent.LastIncludedIndex = prevLastIncludedIndex
		rs.persistent.LastIncludedTerm = prevLastIncludedTerm
		rs.persistent.SnapshotConfig = prevSnapshotConfig
		rs.logger.Printf("TruncateLogTo: persist failed, restored the log at boundary %d: %v",
			prevLastIncludedIndex, err)
		return fmt.Errorf("failed to persist state after truncating log: %w", err)
	}

	rs.logger.Printf("Log truncated up to index %d (term %d), remaining log size %d",
		absoluteIndex, boundaryTerm, len(rs.persistent.Log))
	return nil
}

// ShouldTakeSnapshot checks if a snapshot should be taken based on log size
func (rs *RaftState) ShouldTakeSnapshot(maxLogSize int) bool {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return len(rs.persistent.Log) >= maxLogSize
}

// GetLastLogIndexWithSnapshot returns the last log index including snapshot
func (rs *RaftState) GetLastLogIndexWithSnapshot() int {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.lastAbsLogIndex()
}

// GetLastLogTermWithSnapshot returns the last log term including snapshot
func (rs *RaftState) GetLastLogTermWithSnapshot() int {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.lastAbsLogTerm()
}
