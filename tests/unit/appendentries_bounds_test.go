package unit

import (
	"testing"

	"rosetta/raft"
)

// R13-1: PrevLogIndex sits exactly on the follower's snapshot boundary, but the
// leader's PrevLogTerm disagrees with our LastIncludedTerm. Before the fix this
// branch fell straight through to merge with no term check at all — a leader
// whose committed prefix disagrees with our own snapshot would be accepted
// silently. This must be refused, and refusing it must not touch anything: not
// the log, not CommitIndex, not the snapshot boundary itself.
func TestAppendEntriesRejectsBoundaryTermMismatch(t *testing.T) {
	follower, _ := compactedFollower(t) // LastIncludedIndex=5, LastIncludedTerm=1

	lastLogBefore := follower.GetLastLogIndex()
	commitBefore := follower.GetCommitIndex()
	liiBefore, litBefore := follower.GetSnapshotMetadata()

	reply := &raft.AppendEntriesReply{}
	follower.AppendEntries(&raft.AppendEntriesArgs{
		Term:         1,
		LeaderID:     "leader",
		PrevLogIndex: 5, // == LastIncludedIndex
		PrevLogTerm:  2, // mismatch: our LastIncludedTerm is 1
		Entries:      []raft.LogEntry{{Term: 2, Index: 6, Command: "e", Type: "command"}},
		LeaderCommit: 5,
	}, reply)

	if reply.Success {
		t.Fatal("AppendEntries with a boundary term mismatch must be rejected")
	}
	if reply.ConflictTerm != -1 {
		t.Errorf("ConflictTerm = %d, want -1 (leader should fall back to its snapshot path, not walk terms)", reply.ConflictTerm)
	}
	if reply.ConflictIndex != liiBefore+1 {
		t.Errorf("ConflictIndex = %d, want %d (LastIncludedIndex+1)", reply.ConflictIndex, liiBefore+1)
	}

	if got := follower.GetLastLogIndex(); got != lastLogBefore {
		t.Errorf("log mutated by a rejected request: last index %d, want unchanged %d", got, lastLogBefore)
	}
	if got := follower.GetCommitIndex(); got != commitBefore {
		t.Errorf("CommitIndex mutated by a rejected request: got %d, want unchanged %d", got, commitBefore)
	}
	gotLii, gotLit := follower.GetSnapshotMetadata()
	if gotLii != liiBefore || gotLit != litBefore {
		t.Errorf("snapshot boundary mutated by a rejected request: got (%d,%d), want unchanged (%d,%d)",
			gotLii, gotLit, liiBefore, litBefore)
	}
}

// R13-1: PrevLogIndex is below the snapshot boundary, but the leader's Entries
// slice happens to carry the boundary entry itself (absolute index ==
// LastIncludedIndex) at a term that disagrees with LastIncludedTerm. Before the
// fix, PrevLogIndex < lii accepted unconditionally with no inspection of
// Entries at all.
func TestAppendEntriesRejectsBoundaryTermMismatchWithinEntries(t *testing.T) {
	follower, _ := compactedFollower(t) // LastIncludedIndex=5, LastIncludedTerm=1

	lastLogBefore := follower.GetLastLogIndex()
	commitBefore := follower.GetCommitIndex()
	liiBefore, litBefore := follower.GetSnapshotMetadata()

	// PrevLogIndex=3 < lii=5. Entries span absolute indices 4..7; index 5 (the
	// boundary) is stamped term 2, disagreeing with our LastIncludedTerm of 1.
	entries := []raft.LogEntry{
		{Term: 1, Index: 4, Command: "e", Type: "command"},
		{Term: 2, Index: 5, Command: "e", Type: "command"}, // boundary, mismatched term
		{Term: 2, Index: 6, Command: "e", Type: "command"},
		{Term: 2, Index: 7, Command: "e", Type: "command"},
	}

	reply := &raft.AppendEntriesReply{}
	follower.AppendEntries(&raft.AppendEntriesArgs{
		Term:         1,
		LeaderID:     "leader",
		PrevLogIndex: 3,
		PrevLogTerm:  1,
		Entries:      entries,
		LeaderCommit: 5,
	}, reply)

	if reply.Success {
		t.Fatal("AppendEntries whose Entries disagree with our snapshot boundary term must be rejected")
	}
	if reply.ConflictTerm != -1 {
		t.Errorf("ConflictTerm = %d, want -1", reply.ConflictTerm)
	}
	if reply.ConflictIndex != liiBefore+1 {
		t.Errorf("ConflictIndex = %d, want %d (LastIncludedIndex+1)", reply.ConflictIndex, liiBefore+1)
	}

	if got := follower.GetLastLogIndex(); got != lastLogBefore {
		t.Errorf("log mutated by a rejected request: last index %d, want unchanged %d", got, lastLogBefore)
	}
	if got := follower.GetCommitIndex(); got != commitBefore {
		t.Errorf("CommitIndex mutated by a rejected request: got %d, want unchanged %d", got, commitBefore)
	}
	gotLii, gotLit := follower.GetSnapshotMetadata()
	if gotLii != liiBefore || gotLit != litBefore {
		t.Errorf("snapshot boundary mutated by a rejected request: got (%d,%d), want unchanged (%d,%d)",
			gotLii, gotLit, liiBefore, litBefore)
	}
}
