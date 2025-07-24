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

func (lm *LogManager) AppendEntries(prevLogIndex int, prevLogTerm int, entries []LogEntry) bool {
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

func (rs *RaftState) AppendLogEntry(command interface{}, entryType string) int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	
	index := len(rs.persistent.Log) + 1
	entry := LogEntry{
		Term:    rs.persistent.CurrentTerm,
		Index:   index,
		Command: command,
		Type:    entryType,
	}
	rs.persistent.Log = append(rs.persistent.Log, entry)
	return index
}

func (rs *RaftState) GetLogEntry(index int) *LogEntry {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	
	if index <= 0 || index > len(rs.persistent.Log) {
		return nil
	}
	return &rs.persistent.Log[index-1]
}

func (rs *RaftState) GetLogEntries(startIndex int) []LogEntry {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	
	if startIndex <= 0 || startIndex > len(rs.persistent.Log)+1 {
		return nil
	}
	if startIndex > len(rs.persistent.Log) {
		return []LogEntry{}
	}
	return rs.persistent.Log[startIndex-1:]
}

func (rs *RaftState) GetLastLogIndex() int {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return len(rs.persistent.Log)
}

func (rs *RaftState) GetLastLogTerm() int {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	
	if len(rs.persistent.Log) == 0 {
		return 0
	}
	return rs.persistent.Log[len(rs.persistent.Log)-1].Term
}

func (rs *RaftState) TruncateLogAfter(index int) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	
	if index < 0 {
		rs.persistent.Log = make([]LogEntry, 0)
	} else if index < len(rs.persistent.Log) {
		rs.persistent.Log = rs.persistent.Log[:index]
	}
}

func (rs *RaftState) appendLogEntries(prevLogIndex int, prevLogTerm int, entries []LogEntry) bool {
	if prevLogIndex > 0 {
		if prevLogIndex > len(rs.persistent.Log) {
			return false
		}
		if rs.persistent.Log[prevLogIndex-1].Term != prevLogTerm {
			return false
		}
	}

	if len(entries) > 0 {
		if prevLogIndex < len(rs.persistent.Log) {
			rs.persistent.Log = rs.persistent.Log[:prevLogIndex]
		}
		rs.persistent.Log = append(rs.persistent.Log, entries...)
		
		for i := range rs.persistent.Log[prevLogIndex:] {
			rs.persistent.Log[prevLogIndex+i].Index = prevLogIndex + i + 1
		}
	}

	return true
}

func (rs *RaftState) UpdateCommitIndex(leaderCommit int) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	
	newCommitIndex := min(leaderCommit, len(rs.persistent.Log))
	if newCommitIndex > rs.volatile.CommitIndex {
		rs.volatile.CommitIndex = newCommitIndex
		rs.applyEntries()
	}
}

func (rs *RaftState) applyEntries() {
	for rs.volatile.LastApplied < rs.volatile.CommitIndex {
		rs.volatile.LastApplied++
		entry := rs.persistent.Log[rs.volatile.LastApplied-1]
		
		applyMsg := ApplyMsg{
			CommandValid: true,
			Command:      entry.Command,
			CommandIndex: entry.Index,
		}
		
		select {
		case rs.applyCh <- applyMsg:
		default:
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}