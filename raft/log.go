package raft

import (
	"encoding/json"
)

type LogEntry struct {
	Term    int         `json:"term"`
	Index   int         `json:"index"`
	Command interface{} `json:"command"`
	Type    string      `json:"type"`
}

type LogManager struct {
	entries []LogEntry
}

func NewLogManager() *LogManager {
	return &LogManager{
		entries: make([]LogEntry, 0),
	}
}

func (lm *LogManager) AppendEntry(term int, command interface{}, entryType string) int {
	index := len(lm.entries) + 1
	entry := LogEntry{
		Term:    term,
		Index:   index,
		Command: command,
		Type:    entryType,
	}
	lm.entries = append(lm.entries, entry)
	return index
}

func (lm *LogManager) GetEntry(index int) *LogEntry {
	if index <= 0 || index > len(lm.entries) {
		return nil
	}
	return &lm.entries[index-1]
}

func (lm *LogManager) GetEntries(startIndex int) []LogEntry {
	if startIndex <= 0 || startIndex > len(lm.entries)+1 {
		return nil
	}
	if startIndex > len(lm.entries) {
		return []LogEntry{}
	}
	return lm.entries[startIndex-1:]
}

func (lm *LogManager) GetLastLogIndex() int {
	return len(lm.entries)
}

func (lm *LogManager) GetLastLogTerm() int {
	if len(lm.entries) == 0 {
		return 0
	}
	return lm.entries[len(lm.entries)-1].Term
}

func (lm *LogManager) GetLogTerm(index int) int {
	if index <= 0 || index > len(lm.entries) {
		return 0
	}
	return lm.entries[index-1].Term
}

func (lm *LogManager) TruncateAfter(index int) {
	if index < 0 {
		lm.entries = make([]LogEntry, 0)
	} else if index < len(lm.entries) {
		lm.entries = lm.entries[:index]
	}
}

func (lm *LogManager) AppendEntries(prevLogIndex, prevLogTerm int, entries []LogEntry) bool {
	if prevLogIndex > 0 {
		if prevLogIndex > len(lm.entries) {
			return false
		}
		if lm.entries[prevLogIndex-1].Term != prevLogTerm {
			return false
		}
	}

	if len(entries) > 0 {
		lm.TruncateAfter(prevLogIndex)
		lm.entries = append(lm.entries, entries...)

		for i := range lm.entries[prevLogIndex:] {
			lm.entries[prevLogIndex+i].Index = prevLogIndex + i + 1
		}
	}

	return true
}

func (lm *LogManager) GetAllEntries() []LogEntry {
	result := make([]LogEntry, len(lm.entries))
	copy(result, lm.entries)
	return result
}

func (lm *LogManager) Size() int {
	return len(lm.entries)
}

func (lm *LogManager) SerializeEntry(entry LogEntry) ([]byte, error) {
	return json.Marshal(entry)
}

func (lm *LogManager) DeserializeEntry(data []byte) (*LogEntry, error) {
	var entry LogEntry
	err := json.Unmarshal(data, &entry)
	if err != nil {
		return nil, err
	}
	return &entry, nil
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
// Boundary behaviour:
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

func (rs *RaftState) AppendLogEntry(command interface{}, entryType string) int {
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
		rs.logger.Printf("AppendLogEntry: persist failed: %v", err)
	}
	return index
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
// the given absolute index, keeping the snapshot boundary intact.
func (rs *RaftState) TruncateLogAfter(index int) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	keep := index - rs.persistent.LastIncludedIndex
	if keep < 0 {
		rs.persistent.Log = make([]LogEntry, 0)
	} else if keep < len(rs.persistent.Log) {
		rs.persistent.Log = rs.persistent.Log[:keep]
	}
	if err := rs.persist(); err != nil {
		rs.logger.Printf("TruncateLogAfter: persist failed: %v", err)
	}
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
