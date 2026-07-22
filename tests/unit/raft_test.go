package unit

import (
	"testing"
	"time"

	"rosetta/raft"
)

func TestRaftStateInitialization(t *testing.T) {
	applyCh := make(chan raft.ApplyMsg, 10)
	peers := []string{"node1", "node2", "node3"}

	state := raft.NewRaftState("node1", peers, applyCh)

	if state == nil {
		t.Fatal("NewRaftState returned nil")
	}

	if state.GetNodeState() != raft.Follower {
		t.Errorf("Expected initial state to be Follower, got %v", state.GetNodeState())
	}

	if state.GetCurrentTerm() != 0 {
		t.Errorf("Expected initial term to be 0, got %d", state.GetCurrentTerm())
	}

	if state.GetVotedFor() != nil {
		t.Errorf("Expected initial votedFor to be nil, got %v", state.GetVotedFor())
	}
}

func TestRaftStateTransitions(t *testing.T) {
	applyCh := make(chan raft.ApplyMsg, 10)
	peers := []string{"node1", "node2", "node3"}

	state := raft.NewRaftState("node1", peers, applyCh)

	state.SetState(raft.Candidate)
	if state.GetNodeState() != raft.Candidate {
		t.Errorf("Expected state to be Candidate, got %v", state.GetNodeState())
	}

	state.SetState(raft.Leader)
	if state.GetNodeState() != raft.Leader {
		t.Errorf("Expected state to be Leader, got %v", state.GetNodeState())
	}

	leaderState := state.GetLeaderState()
	if leaderState == nil {
		t.Error("Expected leader state to be initialized when becoming leader")
	}

	state.SetState(raft.Follower)
	if state.GetNodeState() != raft.Follower {
		t.Errorf("Expected state to be Follower, got %v", state.GetNodeState())
	}

	leaderState = state.GetLeaderState()
	if leaderState != nil {
		t.Error("Expected leader state to be nil when not leader")
	}
}

func TestTermIncrement(t *testing.T) {
	applyCh := make(chan raft.ApplyMsg, 10)
	peers := []string{"node1", "node2", "node3"}

	state := raft.NewRaftState("node1", peers, applyCh)

	initialTerm := state.GetCurrentTerm()
	state.IncrementTerm()

	if state.GetCurrentTerm() != initialTerm+1 {
		t.Errorf("Expected term to be %d, got %d", initialTerm+1, state.GetCurrentTerm())
	}

	if state.GetVotedFor() != nil {
		t.Error("Expected votedFor to be reset to nil after term increment")
	}
}

func TestVoting(t *testing.T) {
	applyCh := make(chan raft.ApplyMsg, 10)
	peers := []string{"node1", "node2", "node3"}

	state := raft.NewRaftState("node1", peers, applyCh)

	nodeID := nodeID2
	state.SetVotedFor(&nodeID)

	votedFor := state.GetVotedFor()
	if votedFor == nil || *votedFor != nodeID2 {
		t.Errorf("Expected votedFor to be 'node2', got %v", votedFor)
	}
}

func TestLogOperations(t *testing.T) {
	applyCh := make(chan raft.ApplyMsg, 10)
	peers := []string{"node1", "node2", "node3"}

	state := raft.NewRaftState("node1", peers, applyCh)

	if state.GetLastLogIndex() != 0 {
		t.Errorf("Expected initial log index to be 0, got %d", state.GetLastLogIndex())
	}

	if state.GetLastLogTerm() != 0 {
		t.Errorf("Expected initial log term to be 0, got %d", state.GetLastLogTerm())
	}

	index := state.AppendLogEntry("test command", "command")
	if index != 1 {
		t.Errorf("Expected first log entry index to be 1, got %d", index)
	}

	if state.GetLastLogIndex() != 1 {
		t.Errorf("Expected log index to be 1 after append, got %d", state.GetLastLogIndex())
	}

	entry := state.GetLogEntry(1)
	if entry == nil {
		t.Fatal("Expected log entry to exist")
	}

	if entry.Command != "test command" {
		t.Errorf("Expected command to be 'test command', got %v", entry.Command)
	}

	if entry.Type != "command" {
		t.Errorf("Expected type to be 'command', got %s", entry.Type)
	}
}

func TestRequestVote(t *testing.T) {
	applyCh := make(chan raft.ApplyMsg, 10)
	peers := []string{"node1", "node2", "node3"}

	state := raft.NewRaftState("node1", peers, applyCh)

	args := &raft.RequestVoteArgs{
		Term:         1,
		CandidateID:  "node2",
		LastLogIndex: 0,
		LastLogTerm:  0,
	}

	reply := &raft.RequestVoteReply{}
	state.RequestVote(args, reply)

	if !reply.VoteGranted {
		t.Error("Expected vote to be granted for valid request")
	}

	if reply.Term != 1 {
		t.Errorf("Expected reply term to be 1, got %d", reply.Term)
	}

	votedFor := state.GetVotedFor()
	if votedFor == nil || *votedFor != nodeID2 {
		t.Errorf("Expected to have voted for node2, got %v", votedFor)
	}
}

func TestRequestVoteStaleterm(t *testing.T) {
	applyCh := make(chan raft.ApplyMsg, 10)
	peers := []string{"node1", "node2", "node3"}

	state := raft.NewRaftState("node1", peers, applyCh)
	state.IncrementTerm()
	state.IncrementTerm()

	args := &raft.RequestVoteArgs{
		Term:         1,
		CandidateID:  "node2",
		LastLogIndex: 0,
		LastLogTerm:  0,
	}

	reply := &raft.RequestVoteReply{}
	state.RequestVote(args, reply)

	if reply.VoteGranted {
		t.Error("Expected vote to be denied for stale term")
	}

	if reply.Term != 2 {
		t.Errorf("Expected reply term to be 2, got %d", reply.Term)
	}
}

func TestAppendEntries(t *testing.T) {
	applyCh := make(chan raft.ApplyMsg, 10)
	peers := []string{"node1", "node2", "node3"}

	state := raft.NewRaftState("node1", peers, applyCh)

	entries := []raft.LogEntry{
		{Term: 1, Index: 1, Command: "cmd1", Type: "command"},
		{Term: 1, Index: 2, Command: "cmd2", Type: "command"},
	}

	args := &raft.AppendEntriesArgs{
		Term:         1,
		LeaderID:     "node2",
		PrevLogIndex: 0,
		PrevLogTerm:  0,
		Entries:      entries,
		LeaderCommit: 0,
	}

	reply := &raft.AppendEntriesReply{}
	state.AppendEntries(args, reply)

	if !reply.Success {
		t.Error("Expected AppendEntries to succeed")
	}

	if state.GetLastLogIndex() != 2 {
		t.Errorf("Expected log index to be 2, got %d", state.GetLastLogIndex())
	}

	entry := state.GetLogEntry(1)
	if entry == nil || entry.Command != "cmd1" {
		t.Error("Expected first entry to be 'cmd1'")
	}

	entry = state.GetLogEntry(2)
	if entry == nil || entry.Command != "cmd2" {
		t.Error("Expected second entry to be 'cmd2'")
	}
}

func TestElectionTimer(t *testing.T) {
	applyCh := make(chan raft.ApplyMsg, 10)
	peers := []string{"node1", "node2", "node3"}

	state := raft.NewRaftState("node1", peers, applyCh)

	timerCh := state.ElectionTimer()
	if timerCh == nil {
		t.Fatal("Expected election timer channel to be non-nil")
	}

	state.UpdateLastHeartbeat()
	lastHeartbeat := state.GetLastHeartbeat()

	if time.Since(lastHeartbeat) > time.Millisecond {
		t.Error("Expected last heartbeat to be recent")
	}

	state.ResetElectionTimer()
}

func TestMockTransport(t *testing.T) {
	transport := raft.NewMockTransport()

	applyCh1 := make(chan raft.ApplyMsg, 10)
	applyCh2 := make(chan raft.ApplyMsg, 10)

	peers := []string{"node1", "node2"}

	node1 := raft.NewRaftNode("node1", peers, transport, applyCh1)
	node2 := raft.NewRaftNode("node2", peers, transport, applyCh2)

	transport.RegisterNode("node1", node1)
	transport.RegisterNode("node2", node2)

	nodes := transport.GetNodes()
	if len(nodes) != 2 {
		t.Errorf("Expected 2 nodes, got %d", len(nodes))
	}

	if _, exists := nodes["node1"]; !exists {
		t.Error("Expected node1 to be registered")
	}

	if _, exists := nodes["node2"]; !exists {
		t.Error("Expected node2 to be registered")
	}

	transport.RemoveNode("node2")
	nodes = transport.GetNodes()
	if len(nodes) != 1 {
		t.Errorf("Expected 1 node after removal, got %d", len(nodes))
	}

	node1.Kill()
	node2.Kill()
}

// TestAppendEntriesConflictLogTooShort tests fast rollback when follower's log is too short
func TestAppendEntriesConflictLogTooShort(t *testing.T) {
	applyCh := make(chan raft.ApplyMsg, 10)
	peers := []string{"follower", "leader"}

	follower := raft.NewRaftState("follower", peers, applyCh)

	// Leader tries to append at index 5, but follower has empty log
	args := &raft.AppendEntriesArgs{
		Term:         2,
		LeaderID:     "leader",
		PrevLogIndex: 5,
		PrevLogTerm:  1,
		Entries:      []raft.LogEntry{{Term: 2, Index: 6, Command: "cmd", Type: "command"}},
		LeaderCommit: 0,
	}

	reply := &raft.AppendEntriesReply{}
	follower.AppendEntries(args, reply)

	if reply.Success {
		t.Error("Expected AppendEntries to fail when log is too short")
	}

	if reply.ConflictTerm != -1 {
		t.Errorf("Expected ConflictTerm to be -1 (log too short), got %d", reply.ConflictTerm)
	}

	if reply.ConflictIndex != 1 {
		t.Errorf("Expected ConflictIndex to be 1 (empty log), got %d", reply.ConflictIndex)
	}
}

// TestAppendEntriesConflictTermMismatch tests fast rollback with term mismatch
func TestAppendEntriesConflictTermMismatch(t *testing.T) {
	applyCh := make(chan raft.ApplyMsg, 10)
	peers := []string{"follower", "leader"}

	follower := raft.NewRaftState("follower", peers, applyCh)

	// Populate follower's log with entries from term 1
	follower.AppendEntries(&raft.AppendEntriesArgs{
		Term:         1,
		LeaderID:     "leader",
		PrevLogIndex: 0,
		PrevLogTerm:  0,
		Entries: []raft.LogEntry{
			{Term: 1, Index: 1, Command: "cmd1", Type: "command"},
			{Term: 1, Index: 2, Command: "cmd2", Type: "command"},
			{Term: 2, Index: 3, Command: "cmd3", Type: "command"},
			{Term: 2, Index: 4, Command: "cmd4", Type: "command"},
		},
		LeaderCommit: 0,
	}, &raft.AppendEntriesReply{})

	// Leader tries to append at index 3, but expects term 3 (follower has term 2)
	args := &raft.AppendEntriesArgs{
		Term:         3,
		LeaderID:     "leader",
		PrevLogIndex: 3,
		PrevLogTerm:  3, // Mismatch: follower has term 2 at index 3
		Entries:      []raft.LogEntry{{Term: 3, Index: 4, Command: "cmd", Type: "command"}},
		LeaderCommit: 0,
	}

	reply := &raft.AppendEntriesReply{}
	follower.AppendEntries(args, reply)

	if reply.Success {
		t.Error("Expected AppendEntries to fail due to term mismatch")
	}

	if reply.ConflictTerm != 2 {
		t.Errorf("Expected ConflictTerm to be 2, got %d", reply.ConflictTerm)
	}

	// ConflictIndex should point to first entry of term 2 (index 3)
	if reply.ConflictIndex != 3 {
		t.Errorf("Expected ConflictIndex to be 3 (first index of term 2), got %d", reply.ConflictIndex)
	}
}

// TestAppendEntriesConflictFirstIndexOfTerm tests finding first index of conflicting term
func TestAppendEntriesConflictFirstIndexOfTerm(t *testing.T) {
	applyCh := make(chan raft.ApplyMsg, 10)
	peers := []string{"follower", "leader"}

	follower := raft.NewRaftState("follower", peers, applyCh)

	// Create a log: [term1, term1, term2, term2, term2, term3]
	follower.AppendEntries(&raft.AppendEntriesArgs{
		Term:         1,
		LeaderID:     "leader",
		PrevLogIndex: 0,
		PrevLogTerm:  0,
		Entries: []raft.LogEntry{
			{Term: 1, Index: 1, Command: "cmd1", Type: "command"},
			{Term: 1, Index: 2, Command: "cmd2", Type: "command"},
			{Term: 2, Index: 3, Command: "cmd3", Type: "command"},
			{Term: 2, Index: 4, Command: "cmd4", Type: "command"},
			{Term: 2, Index: 5, Command: "cmd5", Type: "command"},
			{Term: 3, Index: 6, Command: "cmd6", Type: "command"},
		},
		LeaderCommit: 0,
	}, &raft.AppendEntriesReply{})

	// Leader expects term 4 at index 5, but follower has term 2
	args := &raft.AppendEntriesArgs{
		Term:         4,
		LeaderID:     "leader",
		PrevLogIndex: 5,
		PrevLogTerm:  4, // Mismatch
		Entries:      []raft.LogEntry{{Term: 4, Index: 6, Command: "cmd", Type: "command"}},
		LeaderCommit: 0,
	}

	reply := &raft.AppendEntriesReply{}
	follower.AppendEntries(args, reply)

	if reply.Success {
		t.Error("Expected AppendEntries to fail")
	}

	if reply.ConflictTerm != 2 {
		t.Errorf("Expected ConflictTerm to be 2, got %d", reply.ConflictTerm)
	}

	// ConflictIndex should be 3 (first entry with term 2)
	if reply.ConflictIndex != 3 {
		t.Errorf("Expected ConflictIndex to be 3, got %d", reply.ConflictIndex)
	}
}

// TestAppendEntriesReplyStructure tests that reply includes conflict information
func TestAppendEntriesReplyStructure(t *testing.T) {
	// This test verifies that AppendEntriesReply has the necessary fields for fast rollback
	reply := &raft.AppendEntriesReply{
		Term:          5,
		Success:       false,
		ConflictTerm:  3,
		ConflictIndex: 10,
	}

	if reply.ConflictTerm != 3 {
		t.Errorf("Expected ConflictTerm to be 3, got %d", reply.ConflictTerm)
	}

	if reply.ConflictIndex != 10 {
		t.Errorf("Expected ConflictIndex to be 10, got %d", reply.ConflictIndex)
	}
}

// countingPersister records how many times the log is saved so a test can
// assert that a pure-duplicate AppendEntries does not touch stable storage.
type countingPersister struct {
	saves int
}

func (p *countingPersister) SaveRaftState(*raft.PersistentState) error {
	p.saves++
	return nil
}

func (p *countingPersister) LoadRaftState() (*raft.PersistentState, error) {
	return &raft.PersistentState{Log: make([]raft.LogEntry, 0)}, nil
}

// appendEntriesOK applies an AppendEntries and fails the test if it is rejected.
func appendEntriesOK(t *testing.T, rs *raft.RaftState, prevIndex, prevTerm int, entries []raft.LogEntry) {
	t.Helper()
	reply := &raft.AppendEntriesReply{}
	rs.AppendEntries(&raft.AppendEntriesArgs{
		Term:         1,
		LeaderID:     "leader",
		PrevLogIndex: prevIndex,
		PrevLogTerm:  prevTerm,
		Entries:      entries,
		LeaderCommit: 0,
	}, reply)
	if !reply.Success {
		t.Fatalf("expected AppendEntries(prevIndex=%d) to succeed", prevIndex)
	}
}

// TestAppendEntriesStaleRequestDoesNotTruncate reproduces the reorder/retransmit
// hazard: after a longer batch is applied, a delayed AppendEntries carrying only
// a prefix of already-present entries must NOT truncate the (possibly committed)
// suffix. Raft §5.3 (receiver rule 3) only deletes entries that conflict.
func TestAppendEntriesStaleRequestDoesNotTruncate(t *testing.T) {
	applyCh := make(chan raft.ApplyMsg, 10)
	peers := []string{"follower", "leader"}
	follower := raft.NewRaftState("follower", peers, applyCh)

	// Seed e1,e2 then apply the newer batch e3,e4,e5 → log is [e1..e5].
	appendEntriesOK(t, follower, 0, 0, []raft.LogEntry{
		{Term: 1, Index: 1, Command: "e1", Type: "command"},
		{Term: 1, Index: 2, Command: "e2", Type: "command"},
	})
	appendEntriesOK(t, follower, 2, 1, []raft.LogEntry{
		{Term: 1, Index: 3, Command: "e3", Type: "command"},
		{Term: 1, Index: 4, Command: "e4", Type: "command"},
		{Term: 1, Index: 5, Command: "e5", Type: "command"},
	})

	if follower.GetLastLogIndex() != 5 {
		t.Fatalf("setup: expected log length 5, got %d", follower.GetLastLogIndex())
	}

	// A delayed, older AppendEntries carrying only e3 arrives. e3 already matches,
	// so the log must be left intact rather than truncated to [e1,e2,e3].
	appendEntriesOK(t, follower, 2, 1, []raft.LogEntry{
		{Term: 1, Index: 3, Command: "e3", Type: "command"},
	})

	if follower.GetLastLogIndex() != 5 {
		t.Errorf("stale AppendEntries truncated committed suffix: expected log length 5, got %d", follower.GetLastLogIndex())
	}
	if e := follower.GetLogEntry(5); e == nil || e.Command != "e5" {
		t.Errorf("expected entry 5 to still be 'e5', got %+v", e)
	}
}

// TestAppendEntriesDuplicateDoesNotPersist verifies that re-receiving an
// AppendEntries whose entries all match the existing log neither changes the log
// nor writes to stable storage (avoids redundant disk I/O on retransmits).
func TestAppendEntriesDuplicateDoesNotPersist(t *testing.T) {
	applyCh := make(chan raft.ApplyMsg, 10)
	peers := []string{"follower", "leader"}
	persister := &countingPersister{}
	follower, err := raft.NewRaftStateWithPersister("follower", peers, applyCh, persister)
	if err != nil {
		t.Fatalf("unexpected setup error: %v", err)
	}

	entries := []raft.LogEntry{
		{Term: 1, Index: 1, Command: "e1", Type: "command"},
		{Term: 1, Index: 2, Command: "e2", Type: "command"},
	}
	appendEntriesOK(t, follower, 0, 0, entries)

	savesAfterFirst := persister.saves
	if savesAfterFirst == 0 {
		t.Fatal("expected the initial AppendEntries to persist the log at least once")
	}

	// Identical re-delivery: nothing changes, so nothing should be persisted.
	appendEntriesOK(t, follower, 0, 0, entries)

	if follower.GetLastLogIndex() != 2 {
		t.Errorf("duplicate AppendEntries changed log length: expected 2, got %d", follower.GetLastLogIndex())
	}
	if persister.saves != savesAfterFirst {
		t.Errorf("duplicate AppendEntries persisted despite no change: saves went %d -> %d", savesAfterFirst, persister.saves)
	}
}

// TestAppendEntriesConflictTruncatesAndReplaces guards the genuine-conflict path
// (same index, different term): existing entries from the conflict point on must
// be deleted and replaced by the leader's entries.
func TestAppendEntriesConflictTruncatesAndReplaces(t *testing.T) {
	applyCh := make(chan raft.ApplyMsg, 10)
	peers := []string{"follower", "leader"}
	follower := raft.NewRaftState("follower", peers, applyCh)

	appendEntriesOK(t, follower, 0, 0, []raft.LogEntry{
		{Term: 1, Index: 1, Command: "e1", Type: "command"},
		{Term: 1, Index: 2, Command: "e2", Type: "command"},
		{Term: 1, Index: 3, Command: "e3", Type: "command"},
	})

	// Leader overwrites from index 2 with higher-term entries. Index 1 matches, so
	// only indexes 2..3 are replaced.
	appendEntriesOK(t, follower, 1, 1, []raft.LogEntry{
		{Term: 2, Index: 2, Command: "e2b", Type: "command"},
		{Term: 2, Index: 3, Command: "e3b", Type: "command"},
	})

	if follower.GetLastLogIndex() != 3 {
		t.Fatalf("expected log length 3 after conflict replace, got %d", follower.GetLastLogIndex())
	}
	if e := follower.GetLogEntry(1); e == nil || e.Term != 1 || e.Command != "e1" {
		t.Errorf("expected entry 1 unchanged (term 1, 'e1'), got %+v", e)
	}
	if e := follower.GetLogEntry(2); e == nil || e.Term != 2 || e.Command != "e2b" {
		t.Errorf("expected entry 2 replaced (term 2, 'e2b'), got %+v", e)
	}
	if e := follower.GetLogEntry(3); e == nil || e.Term != 2 || e.Command != "e3b" {
		t.Errorf("expected entry 3 replaced (term 2, 'e3b'), got %+v", e)
	}
}

// The lease-based read optimization (CanServeReadOnlyQuery / UpdateLeaderConfirmation)
// was removed in favor of the ReadIndex protocol. Its unsafe behavior — a leader
// serving reads while only a minority still acknowledged it — and the replacement
// are covered by the ReadIndex tests in readindex_test.go and the kvstore
// linearizable-read tests.
