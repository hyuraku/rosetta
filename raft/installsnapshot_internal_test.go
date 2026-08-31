package raft

import "testing"

// These are white-box tests of the InstallSnapshot receiver's log-retention
// rule (paper §7 / Figure 13, receiver steps 6 and 7):
//
//	6. If an existing log entry has the same index and term as the snapshot's
//	   last included entry, retain the log entries following it and reply.
//	7. Discard the entire log.
//
// The retention decision is what keeps Log Matching intact across a snapshot
// install. A follower that blindly keeps every entry above LastIncludedIndex
// can end up holding a suffix that belongs to a different (stale) leader's
// history while claiming the snapshot's history below it — the two halves of
// its log then no longer agree with any single leader, which is exactly the
// Log Matching violation tracked as A7 in KNOWN_ISSUES.md.

// newSnapshotTestState builds a follower with a known log and a drained apply
// channel, ready to receive an InstallSnapshot.
func newSnapshotTestState(t *testing.T, term int, log []LogEntry) *RaftState {
	t.Helper()

	applyCh := make(chan ApplyMsg, 16)
	go func() {
		for range applyCh { //nolint:revive // drain applied entries
		}
	}()

	rs := NewRaftState("n1", []string{"n1", "n2", "n3"}, applyCh)
	rs.mu.Lock()
	rs.persistent.CurrentTerm = term
	rs.persistent.Log = log
	rs.mu.Unlock()
	return rs
}

// logIndices renders the absolute indices held in the live log, for assertions.
func logIndices(rs *RaftState) []int {
	out := make([]int, 0, len(rs.persistent.Log))
	for _, e := range rs.persistent.Log {
		out = append(out, e.Index)
	}
	return out
}

// TestInstallSnapshotDiscardsDivergentSuffix covers step 7. The follower's
// entry at the snapshot's LastIncludedIndex carries a *different* term than the
// snapshot does, so everything this follower holds came from a history the
// leader has since overwritten. Keeping the entries above the boundary would
// splice a stale suffix onto the leader's snapshot.
func TestInstallSnapshotDiscardsDivergentSuffix(t *testing.T) {
	// Follower's history: indices 1-5, all from the stale term 2.
	rs := newSnapshotTestState(t, 2, []LogEntry{
		{Term: 2, Index: 1, Command: "a", Type: "command"},
		{Term: 2, Index: 2, Command: "b", Type: "command"},
		{Term: 2, Index: 3, Command: "c", Type: "command"},
		{Term: 2, Index: 4, Command: "d", Type: "command"},
		{Term: 2, Index: 5, Command: "e", Type: "command"},
	})

	// The new leader's snapshot covers up to index 3, but in term 5 — index 3
	// in the real history is a different entry than the one we hold.
	args := &InstallSnapshotArgs{
		Term:              5,
		LeaderID:          "n2",
		LastIncludedIndex: 3,
		LastIncludedTerm:  5,
		Data:              []byte(`{"kv_data":{}}`),
	}
	reply := &InstallSnapshotReply{}
	rs.InstallSnapshot(args, reply)

	rs.mu.Lock()
	defer rs.mu.Unlock()

	if got := len(rs.persistent.Log); got != 0 {
		t.Fatalf("divergent suffix retained: log holds indices %v, want the whole log discarded",
			logIndices(rs))
	}
	if got := rs.lastAbsLogIndex(); got != 3 {
		t.Fatalf("lastAbsLogIndex = %d, want 3 (the snapshot boundary)", got)
	}
	if got := rs.lastAbsLogTerm(); got != 5 {
		t.Fatalf("lastAbsLogTerm = %d, want 5 (LastIncludedTerm)", got)
	}
}

// TestInstallSnapshotRetainsMatchingSuffix covers step 6. The follower's entry
// at LastIncludedIndex agrees with the snapshot on both index and term, so the
// entries above it are part of the same history and must be kept — discarding
// them would be safe but forces a needless re-replication of entries the
// follower already has.
func TestInstallSnapshotRetainsMatchingSuffix(t *testing.T) {
	// Index 3 is term 5, matching the snapshot the leader is about to send.
	rs := newSnapshotTestState(t, 5, []LogEntry{
		{Term: 5, Index: 1, Command: "a", Type: "command"},
		{Term: 5, Index: 2, Command: "b", Type: "command"},
		{Term: 5, Index: 3, Command: "c", Type: "command"},
		{Term: 5, Index: 4, Command: "d", Type: "command"},
		{Term: 5, Index: 5, Command: "e", Type: "command"},
	})

	args := &InstallSnapshotArgs{
		Term:              5,
		LeaderID:          "n2",
		LastIncludedIndex: 3,
		LastIncludedTerm:  5,
		Data:              []byte(`{"kv_data":{}}`),
	}
	reply := &InstallSnapshotReply{}
	rs.InstallSnapshot(args, reply)

	rs.mu.Lock()
	defer rs.mu.Unlock()

	got := logIndices(rs)
	if len(got) != 2 || got[0] != 4 || got[1] != 5 {
		t.Fatalf("matching suffix not retained: log holds indices %v, want [4 5]", got)
	}
	if got := rs.lastAbsLogIndex(); got != 5 {
		t.Fatalf("lastAbsLogIndex = %d, want 5", got)
	}
	// The retained entries must still be addressable across the new boundary.
	if term := rs.logTermAt(4); term != 5 {
		t.Fatalf("logTermAt(4) = %d, want 5 — retained suffix is misaligned", term)
	}
}

// TestInstallSnapshotDiscardsLogEndingBeforeBoundary is the "no overlap" case:
// the follower is so far behind that it holds nothing at LastIncludedIndex.
// There is no entry to match against, so step 7 applies and the log is emptied.
func TestInstallSnapshotDiscardsLogEndingBeforeBoundary(t *testing.T) {
	rs := newSnapshotTestState(t, 2, []LogEntry{
		{Term: 2, Index: 1, Command: "a", Type: "command"},
		{Term: 2, Index: 2, Command: "b", Type: "command"},
	})

	args := &InstallSnapshotArgs{
		Term:              5,
		LeaderID:          "n2",
		LastIncludedIndex: 7,
		LastIncludedTerm:  5,
		Data:              []byte(`{"kv_data":{}}`),
	}
	reply := &InstallSnapshotReply{}
	rs.InstallSnapshot(args, reply)

	rs.mu.Lock()
	defer rs.mu.Unlock()

	if got := len(rs.persistent.Log); got != 0 {
		t.Fatalf("log holds indices %v, want empty", logIndices(rs))
	}
	if got := rs.lastAbsLogIndex(); got != 7 {
		t.Fatalf("lastAbsLogIndex = %d, want 7 (the snapshot boundary)", got)
	}
}
