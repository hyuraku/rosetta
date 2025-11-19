package raft

import (
	"log"
)

// SnapshotData represents a snapshot of the state machine
type SnapshotData struct {
	LastIncludedIndex int
	LastIncludedTerm  int
	Data              []byte
}

// Snapshotter interface for creating and installing snapshots
type Snapshotter interface {
	// CreateSnapshot creates a snapshot of the state machine up to the given index
	CreateSnapshot(lastIncludedIndex, lastIncludedTerm int) ([]byte, error)

	// InstallSnapshot installs a snapshot into the state machine
	InstallSnapshot(data []byte, lastIncludedIndex, lastIncludedTerm int) error
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
	rs.persist()

	rs.logger.Printf("Snapshot taken: lastIndex=%d, lastTerm=%d, logSize=%d, snapshotSize=%d",
		lastIncludedIndex, lastIncludedTerm, len(rs.persistent.Log), len(snapshotData))

	return nil
}

// InstallSnapshotFromData installs a snapshot from raw data
func (rs *RaftState) InstallSnapshotFromData(lastIncludedIndex, lastIncludedTerm int, data []byte, snapshotter Snapshotter) error {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	// Don't install older snapshots
	if lastIncludedIndex <= rs.persistent.LastIncludedIndex {
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

	// Update volatile state
	if rs.volatile.CommitIndex < lastIncludedIndex {
		rs.volatile.CommitIndex = lastIncludedIndex
	}
	if rs.volatile.LastApplied < lastIncludedIndex {
		rs.volatile.LastApplied = lastIncludedIndex
	}

	// Persist the updated state
	rs.persist()

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
	if len(rs.persistent.Log) == 0 {
		return rs.persistent.LastIncludedIndex
	}
	return rs.persistent.LastIncludedIndex + len(rs.persistent.Log)
}

// GetLastLogTermWithSnapshot returns the last log term including snapshot
func (rs *RaftState) GetLastLogTermWithSnapshot() int {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	if len(rs.persistent.Log) == 0 {
		return rs.persistent.LastIncludedTerm
	}
	return rs.persistent.Log[len(rs.persistent.Log)-1].Term
}
