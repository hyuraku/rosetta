package raft

import "fmt"

type LogEntry struct {
	Term    int         `json:"term"`
	Index   int         `json:"index"`
	Command interface{} `json:"command"`
	Type    string      `json:"type"`
}

// Absolute-index helpers. After log compaction, absolute log index N lives at
// slice position N-LastIncludedIndex-1. These translate between the two spaces
// and are the single source of truth used throughout the package so the
// compaction boundary is handled consistently. All require rs.mu held.

// lastAbsLogIndex returns the absolute index of the last log entry. With no
// live entries this is the snapshot boundary (LastIncludedIndex, 0 before any
// compaction).
func (rs *RaftState) lastAbsLogIndex() int {
	return rs.persistent.LastIncludedIndex + len(rs.persistent.Log)
}

// lastAbsLogTerm returns the term of the last log entry, or LastIncludedTerm
// when every entry has been compacted away.
func (rs *RaftState) lastAbsLogTerm() int {
	if len(rs.persistent.Log) == 0 {
		return rs.persistent.LastIncludedTerm
	}
	return rs.persistent.Log[len(rs.persistent.Log)-1].Term
}

// slicePos maps an absolute log index to its position in persistent.Log. The
// result is only meaningful when LastIncludedIndex < absIndex <= lastAbsLogIndex();
// callers must bounds-check (the value is negative below the boundary).
func (rs *RaftState) slicePos(absIndex int) int {
	return absIndex - rs.persistent.LastIncludedIndex - 1
}

// logTermAt returns the term of the entry at absolute index absIndex.
// Boundary behavior:
//   - absIndex == LastIncludedIndex          -> LastIncludedTerm (snapshot boundary)
//   - absIndex < LastIncludedIndex           -> 0 (compacted away, term unknown)
//   - LastIncludedIndex < absIndex <= last   -> the live entry's term
//   - absIndex > lastAbsLogIndex             -> 0 (beyond the log)
func (rs *RaftState) logTermAt(absIndex int) int {
	if absIndex == rs.persistent.LastIncludedIndex {
		return rs.persistent.LastIncludedTerm
	}
	pos := rs.slicePos(absIndex)
	if pos < 0 || pos >= len(rs.persistent.Log) {
		return 0
	}
	return rs.persistent.Log[pos].Term
}

// logAfterSnapshot decides which live log entries survive an incoming
// InstallSnapshot whose last included entry is (lastIncludedIndex,
// lastIncludedTerm), and returns the new log. Requires rs.mu held. The caller
// has already rejected stale snapshots, so lastIncludedIndex is strictly above
// the current LastIncludedIndex and every entry at or below it is subsumed by
// the snapshot regardless of the outcome.
//
// Paper §7 (Figure 13, receiver steps 6 and 7):
//
//  6. If an existing log entry has the same index and term as the snapshot's
//     last included entry, retain the log entries following it.
//  7. Otherwise, discard the entire log.
//
// logTermAt reports 0 for an index beyond the live log, so a follower too far
// behind to hold the snapshot's last entry takes the discard path through the
// same comparison. Retaining the suffix unconditionally (the pre-fix behavior,
// KNOWN_ISSUES.md A7) lets a follower splice a stale leader's entries onto the
// new leader's snapshot, leaving a log that matches no single history and
// breaking Log Matching.
func (rs *RaftState) logAfterSnapshot(lastIncludedIndex, lastIncludedTerm int) []LogEntry {
	if rs.logTermAt(lastIncludedIndex) != lastIncludedTerm {
		return nil
	}
	return rs.persistent.Log[rs.slicePos(lastIncludedIndex)+1:]
}

// AppendLogEntry appends a new entry for the current term and returns its
// absolute index once the entry has reached stable storage.
//
// A persist failure is reported, never swallowed: the leader counts its own log
// as one of the replicas when advancing the commit index, so an entry that only
// ever existed in memory could be committed, acknowledged to the client, and
// then lost when the leader restarts (Figure 2, "Persistent state ... updated on
// stable storage before responding to RPCs"). On failure the in-memory append is
// rolled back so memory and disk stay in agreement, and the returned index is 0
// — callers must treat the command as never started.
func (rs *RaftState) AppendLogEntry(command interface{}, entryType string) (int, error) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	index := rs.lastAbsLogIndex() + 1
	entry := LogEntry{
		Term:    rs.persistent.CurrentTerm,
		Index:   index,
		Command: command,
		Type:    entryType,
	}
	rs.persistent.Log = append(rs.persistent.Log, entry)
	if err := rs.persist(); err != nil {
		rs.persistent.Log = rs.persistent.Log[:len(rs.persistent.Log)-1]
		rs.logger.Printf("AppendLogEntry: persist failed, rolled back entry %d: %v", index, err)
		return 0, fmt.Errorf("persist log entry %d: %w", index, err)
	}
	return index, nil
}

// GetLogEntry returns the entry at the given absolute log index, or nil when
// that index is outside the live log (compacted away or not yet present).
func (rs *RaftState) GetLogEntry(index int) *LogEntry {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	pos := rs.slicePos(index)
	if pos < 0 || pos >= len(rs.persistent.Log) {
		return nil
	}
	return &rs.persistent.Log[pos]
}

// GetLogEntries returns the live log entries starting at the given absolute
// index. Requesting exactly one past the last index yields an empty slice;
// anything below the snapshot boundary or beyond the end yields nil.
func (rs *RaftState) GetLogEntries(startIndex int) []LogEntry {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	pos := rs.slicePos(startIndex)
	if pos < 0 || pos > len(rs.persistent.Log) {
		return nil
	}
	if pos == len(rs.persistent.Log) {
		return []LogEntry{}
	}
	return rs.persistent.Log[pos:]
}

func (rs *RaftState) GetLastLogIndex() int {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.lastAbsLogIndex()
}

func (rs *RaftState) GetLastLogTerm() int {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.lastAbsLogTerm()
}

// TruncateLogAfter discards every entry whose absolute index is greater than
// the given absolute index, keeping the snapshot boundary intact. The shortened
// log must reach stable storage before the truncation is reported as done: a
// truncation that survives only in memory would reappear as the discarded suffix
// after a restart. On a persist failure the previous log is restored and the
// error is returned; re-slicing is enough to restore it because truncation only
// moves the slice header and leaves the discarded entries in the backing array,
// and rs.mu is held throughout so nothing can overwrite them in between.
func (rs *RaftState) TruncateLogAfter(index int) error {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	previous := rs.persistent.Log
	keep := index - rs.persistent.LastIncludedIndex
	if keep < 0 {
		rs.persistent.Log = make([]LogEntry, 0)
	} else if keep < len(rs.persistent.Log) {
		rs.persistent.Log = rs.persistent.Log[:keep]
	}
	if err := rs.persist(); err != nil {
		rs.persistent.Log = previous
		rs.logger.Printf("TruncateLogAfter: persist failed, restored log after index %d: %v", index, err)
		return fmt.Errorf("persist log truncated after index %d: %w", index, err)
	}
	return nil
}

func (rs *RaftState) UpdateCommitIndex(leaderCommit int) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	newCommitIndex := min(leaderCommit, rs.lastAbsLogIndex())
	if newCommitIndex > rs.volatile.CommitIndex {
		rs.volatile.CommitIndex = newCommitIndex
		rs.applyEntries()
	}
}

func (rs *RaftState) applyEntries() {
	for rs.volatile.LastApplied < rs.volatile.CommitIndex {
		next := rs.volatile.LastApplied + 1
		pos := rs.slicePos(next)
		if pos < 0 || pos >= len(rs.persistent.Log) {
			// The next entry to apply has been compacted away or is not yet
			// present. This can only happen if LastApplied lags behind the
			// snapshot boundary; refuse to index out of range and stop.
			rs.logger.Printf("applyEntries: index %d outside live log (lastIncluded=%d, logLen=%d), stopping",
				next, rs.persistent.LastIncludedIndex, len(rs.persistent.Log))
			return
		}
		rs.volatile.LastApplied = next
		entry := rs.persistent.Log[pos]

		applyMsg := ApplyMsg{
			CommandValid: true,
			Command:      entry.Command,
			CommandIndex: next,
			CommandTerm:  entry.Term,
		}

		// Block until the state machine consumes the entry. Dropping the
		// message here (as a non-blocking send with a default case would)
		// while LastApplied has already advanced would permanently skip a
		// committed entry, violating the Raft state-machine safety property.
		// The consumer (kvstore applyLoop) never acquires rs.mu synchronously,
		// so this send always drains and cannot deadlock. This mirrors the
		// blocking send already used on the InstallSnapshot apply path.
		rs.applyCh <- applyMsg
	}
}
