package unit

import (
	"testing"
	"time"

	"rosetta/persistence"
	"rosetta/raft"
)

// compactedFollower returns a follower whose log has been compacted so that
// LastIncludedIndex == boundary. It seeds indices 1..total via AppendEntries,
// commits up to boundary (so they are applied), then truncates the prefix.
// All seeded entries are term 1. The returned follower has:
//
//	LastIncludedIndex = boundary, LastIncludedTerm = 1
//	live log = entries (boundary+1 .. total)
//	CommitIndex = LastApplied = boundary
func compactedFollower(t *testing.T, total, boundary int) (state *raft.RaftState, ch chan raft.ApplyMsg) {
	t.Helper()
	applyCh := make(chan raft.ApplyMsg, total+16)
	peers := []string{"follower", "leader"}
	follower := raft.NewRaftState("follower", peers, applyCh)

	entries := make([]raft.LogEntry, 0, total)
	for i := 1; i <= total; i++ {
		entries = append(entries, raft.LogEntry{Term: 1, Index: i, Command: "e", Type: "command"})
	}
	reply := &raft.AppendEntriesReply{}
	follower.AppendEntries(&raft.AppendEntriesArgs{
		Term:         1,
		LeaderID:     "leader",
		PrevLogIndex: 0,
		PrevLogTerm:  0,
		Entries:      entries,
		LeaderCommit: boundary,
	}, reply)
	if !reply.Success {
		t.Fatalf("setup AppendEntries failed")
	}
	// Drain the entries applied up to boundary.
	for i := 0; i < boundary; i++ {
		select {
		case <-applyCh:
		case <-time.After(time.Second):
			t.Fatalf("setup: timed out draining applied entry %d", i+1)
		}
	}
	if err := follower.TruncateLogTo(boundary); err != nil {
		t.Fatalf("setup TruncateLogTo(%d): %v", boundary, err)
	}
	return follower, applyCh
}

// TestAppendLogEntryAfterCompaction verifies AppendLogEntry stamps the correct
// absolute index (item #7) once a snapshot boundary exists.
func TestAppendLogEntryAfterCompaction(t *testing.T) {
	applyCh := make(chan raft.ApplyMsg, 32)
	rs := raft.NewRaftState("node1", []string{"node1"}, applyCh)

	for i := 0; i < 10; i++ {
		rs.AppendLogEntry("cmd", "command")
	}
	if err := rs.TruncateLogTo(5); err != nil {
		t.Fatalf("TruncateLogTo(5): %v", err)
	}

	// Next appended entry must be absolute index 11, not len(log)+1 == 6.
	got := rs.AppendLogEntry("cmd", "command")
	if got != 11 {
		t.Errorf("AppendLogEntry after compaction: got index %d, want 11", got)
	}
	if last := rs.GetLastLogIndex(); last != 11 {
		t.Errorf("GetLastLogIndex after compaction: got %d, want 11", last)
	}
	if e := rs.GetLogEntry(11); e == nil || e.Index != 11 {
		t.Errorf("GetLogEntry(11): got %+v, want entry with Index 11", e)
	}
}

// TestApplyEntriesAfterCompaction verifies applyEntries reads the correct
// slice position and reports absolute CommandIndex without panicking
// (items #3, #4).
func TestApplyEntriesAfterCompaction(t *testing.T) {
	follower, applyCh := compactedFollower(t, 10, 5)

	// Advance commit to 8; entries 6,7,8 must be applied with absolute indices.
	follower.UpdateCommitIndex(8)

	for want := 6; want <= 8; want++ {
		select {
		case msg := <-applyCh:
			if msg.CommandIndex != want {
				t.Errorf("apply after compaction: got CommandIndex %d, want %d", msg.CommandIndex, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for applied entry %d", want)
		}
	}
	if ci := follower.GetCommitIndex(); ci != 8 {
		t.Errorf("commit index after compaction: got %d, want 8", ci)
	}
}

// TestAppendEntriesPrevLogIndexAtBoundary covers PrevLogIndex == LastIncludedIndex
// (item #2): the term is compared against LastIncludedTerm.
func TestAppendEntriesPrevLogIndexAtBoundary(t *testing.T) {
	follower, _ := compactedFollower(t, 10, 5)

	reply := &raft.AppendEntriesReply{}
	follower.AppendEntries(&raft.AppendEntriesArgs{
		Term:         1,
		LeaderID:     "leader",
		PrevLogIndex: 5, // == LastIncludedIndex
		PrevLogTerm:  1, // == LastIncludedTerm
		Entries:      []raft.LogEntry{{Term: 1, Index: 6, Command: "e", Type: "command"}},
		LeaderCommit: 5,
	}, reply)
	if !reply.Success {
		t.Errorf("AppendEntries with PrevLogIndex==LastIncludedIndex should succeed, got failure")
	}
	// Entry 6 already matches, so the live log (6..10) must be left intact, not
	// re-appended at a wrong slice position.
	if last := follower.GetLastLogIndex(); last != 10 {
		t.Errorf("boundary AppendEntries corrupted log: last index %d, want 10", last)
	}
}

// TestAppendEntriesPrevLogIndexAboveBoundary covers PrevLogIndex > LastIncludedIndex
// for both the matching and conflicting cases (item #2).
func TestAppendEntriesPrevLogIndexAboveBoundary(t *testing.T) {
	follower, _ := compactedFollower(t, 10, 5)

	// Matching term at index 8 -> success.
	okReply := &raft.AppendEntriesReply{}
	follower.AppendEntries(&raft.AppendEntriesArgs{
		Term:         1,
		LeaderID:     "leader",
		PrevLogIndex: 8,
		PrevLogTerm:  1,
		Entries:      []raft.LogEntry{{Term: 1, Index: 9, Command: "e", Type: "command"}},
		LeaderCommit: 5,
	}, okReply)
	if !okReply.Success {
		t.Errorf("AppendEntries with matching term at index 8 should succeed")
	}

	// Conflicting term at index 8 -> failure with conflict info.
	badReply := &raft.AppendEntriesReply{}
	follower.AppendEntries(&raft.AppendEntriesArgs{
		Term:         1,
		LeaderID:     "leader",
		PrevLogIndex: 8,
		PrevLogTerm:  2, // real term is 1
		Entries:      []raft.LogEntry{{Term: 2, Index: 9, Command: "x", Type: "command"}},
		LeaderCommit: 5,
	}, badReply)
	if badReply.Success {
		t.Errorf("AppendEntries with conflicting term at index 8 should fail")
	}
	if badReply.ConflictTerm != 1 {
		t.Errorf("expected ConflictTerm 1, got %d", badReply.ConflictTerm)
	}
	// ConflictIndex must never point below the snapshot boundary.
	if badReply.ConflictIndex <= 5 {
		t.Errorf("ConflictIndex %d must stay above snapshot boundary 5", badReply.ConflictIndex)
	}
}

// TestAppendEntriesPrevLogIndexBelowBoundary covers PrevLogIndex < LastIncludedIndex
// (item #2): entries subsumed by the snapshot are ignored; the suffix past the
// boundary is merged and the call succeeds.
func TestAppendEntriesPrevLogIndexBelowBoundary(t *testing.T) {
	follower, _ := compactedFollower(t, 10, 5)

	entries := make([]raft.LogEntry, 0)
	for i := 4; i <= 11; i++ { // spans compacted prefix and one new entry
		entries = append(entries, raft.LogEntry{Term: 1, Index: i, Command: "e", Type: "command"})
	}
	reply := &raft.AppendEntriesReply{}
	follower.AppendEntries(&raft.AppendEntriesArgs{
		Term:         1,
		LeaderID:     "leader",
		PrevLogIndex: 3, // < LastIncludedIndex
		PrevLogTerm:  1,
		Entries:      entries,
		LeaderCommit: 5,
	}, reply)
	if !reply.Success {
		t.Errorf("AppendEntries with PrevLogIndex below snapshot boundary should succeed")
	}
	if last := follower.GetLastLogIndex(); last != 11 {
		t.Errorf("expected last index 11 after merging suffix, got %d", last)
	}
	if e := follower.GetLogEntry(11); e == nil || e.Index != 11 {
		t.Errorf("expected entry 11 present after merge, got %+v", e)
	}
}

// TestRequestVoteUsesAbsoluteIndex verifies the election restriction (§5.4.1)
// is evaluated against the absolute last log index/term after compaction
// (item #1).
func TestRequestVoteUsesAbsoluteIndex(t *testing.T) {
	// Candidate that is behind (LastLogIndex 8 < our absolute 10) must be denied.
	behind, _ := compactedFollower(t, 10, 5)
	reply := &raft.RequestVoteReply{}
	behind.RequestVote(&raft.RequestVoteArgs{
		Term:         2,
		CandidateID:  "cand",
		LastLogIndex: 8,
		LastLogTerm:  1,
	}, reply)
	if reply.VoteGranted {
		t.Errorf("vote must be denied to a candidate behind our absolute log (10), but was granted")
	}

	// Candidate that is up to date (LastLogIndex 10) must be granted.
	upToDate, _ := compactedFollower(t, 10, 5)
	reply2 := &raft.RequestVoteReply{}
	upToDate.RequestVote(&raft.RequestVoteArgs{
		Term:         2,
		CandidateID:  "cand",
		LastLogIndex: 10,
		LastLogTerm:  1,
	}, reply2)
	if !reply2.VoteGranted {
		t.Errorf("vote must be granted to an up-to-date candidate (LastLogIndex 10)")
	}
}

// TestRestartInitializesVolatileFromSnapshot verifies that a node restarted
// from a compacted persistent state initializes CommitIndex/LastApplied to
// LastIncludedIndex, so applyEntries never re-reads compacted positions
// (item #9).
func TestRestartInitializesVolatileFromSnapshot(t *testing.T) {
	tempDir := t.TempDir()
	storage, err := persistence.NewFileStorage(tempDir)
	if err != nil {
		t.Fatalf("NewFileStorage: %v", err)
	}
	defer storage.Close()
	persister := persistence.NewRaftPersister(storage)

	// Persisted state as if the node had snapshotted through index 5 and holds
	// live entries 6..10.
	log := make([]raft.LogEntry, 0)
	for i := 6; i <= 10; i++ {
		log = append(log, raft.LogEntry{Term: 1, Index: i, Command: "e", Type: "command"})
	}
	if err := persister.SaveRaftState(&raft.PersistentState{
		CurrentTerm:       1,
		VotedFor:          nil,
		Log:               log,
		LastIncludedIndex: 5,
		LastIncludedTerm:  1,
	}); err != nil {
		t.Fatalf("SaveRaftState: %v", err)
	}

	applyCh := make(chan raft.ApplyMsg, 32)
	rs, err := raft.NewRaftStateWithPersister("node1", []string{"node1"}, applyCh, persister)
	if err != nil {
		t.Fatalf("NewRaftStateWithPersister: %v", err)
	}

	if ci := rs.GetCommitIndex(); ci != 5 {
		t.Errorf("restart CommitIndex: got %d, want 5 (LastIncludedIndex)", ci)
	}
	if la := rs.GetLastApplied(); la != 5 {
		t.Errorf("restart LastApplied: got %d, want 5 (LastIncludedIndex)", la)
	}

	// Committing forward must apply 6..10 exactly once, with absolute indices.
	rs.UpdateCommitIndex(10)
	for want := 6; want <= 10; want++ {
		select {
		case msg := <-applyCh:
			if msg.CommandIndex != want {
				t.Errorf("post-restart apply: got CommandIndex %d, want %d", msg.CommandIndex, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("post-restart apply: timed out waiting for entry %d", want)
		}
	}
}
