package unit

import (
	"testing"
	"time"

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

// R13-2: commitIndex must advance only to the last index *this* request
// actually vouches for (Figure 2, receiver rule 5: min(leaderCommit, index of
// last new entry)), never past it just because the follower's own log happens
// to extend further. This reproduces a follower holding a leftover suffix from
// a stale term (as if left by a leader that never committed it) and a new
// leader sending a short request (here, an empty heartbeat) whose LeaderCommit
// reaches into that leftover suffix.
//
// Before the fix this test fails: CommitIndex advances to 8 (min(LeaderCommit,
// lastAbsLogIndex())) even though this request only vouches for up to index 5.
func TestAppendEntriesCommitIndexBoundedByLastNewEntry(t *testing.T) {
	applyCh := make(chan raft.ApplyMsg, 16)
	follower := raft.NewRaftState("follower", []string{"follower", "leader"}, applyCh)

	// Seed indices 1..5 at term 1 (as if from an earlier, legitimate leader).
	seedReply := &raft.AppendEntriesReply{}
	follower.AppendEntries(&raft.AppendEntriesArgs{
		Term:         1,
		LeaderID:     "leader",
		PrevLogIndex: 0,
		PrevLogTerm:  0,
		Entries: []raft.LogEntry{
			{Term: 1, Index: 1, Command: "e1", Type: "command"},
			{Term: 1, Index: 2, Command: "e2", Type: "command"},
			{Term: 1, Index: 3, Command: "e3", Type: "command"},
			{Term: 1, Index: 4, Command: "e4", Type: "command"},
			{Term: 1, Index: 5, Command: "e5", Type: "command"},
		},
		LeaderCommit: 0,
	}, seedReply)
	if !seedReply.Success {
		t.Fatalf("setup: seeding indices 1..5 failed: %+v", seedReply)
	}

	// A second, later leader (term 2) appends a suffix at 6..8 that it never
	// gets to commit before this test moves on — an ordinary, never-agreed-on
	// tail left sitting on the follower's log.
	staleReply := &raft.AppendEntriesReply{}
	follower.AppendEntries(&raft.AppendEntriesArgs{
		Term:         2,
		LeaderID:     "leader",
		PrevLogIndex: 5,
		PrevLogTerm:  1,
		Entries: []raft.LogEntry{
			{Term: 2, Index: 6, Command: "stale6", Type: "command"},
			{Term: 2, Index: 7, Command: "stale7", Type: "command"},
			{Term: 2, Index: 8, Command: "stale8", Type: "command"},
		},
		LeaderCommit: 0,
	}, staleReply)
	if !staleReply.Success {
		t.Fatalf("setup: appending the stale term-2 suffix failed: %+v", staleReply)
	}
	if got := follower.GetLastLogIndex(); got != 8 {
		t.Fatalf("setup: last log index = %d, want 8", got)
	}

	// A third leader (term 3) probes this follower with an empty heartbeat at
	// PrevLogIndex=5 (it has not yet learned about, let alone verified, entries
	// 6..8) but reports LeaderCommit=8 from its own, unrelated replication
	// state. This request's "last new entry" is only index 5.
	heartbeatReply := &raft.AppendEntriesReply{}
	follower.AppendEntries(&raft.AppendEntriesArgs{
		Term:         3,
		LeaderID:     "leader",
		PrevLogIndex: 5,
		PrevLogTerm:  1,
		Entries:      nil,
		LeaderCommit: 8,
	}, heartbeatReply)

	if !heartbeatReply.Success {
		t.Fatalf("heartbeat rejected unexpectedly: %+v", heartbeatReply)
	}
	if got := follower.GetCommitIndex(); got != 5 {
		t.Errorf("CommitIndex = %d, want 5 (min(LeaderCommit=8, lastNewIndex=5, lastAbsLogIndex=8)); "+
			"a short request must not let LeaderCommit reach into a suffix it never vouched for", got)
	}

	// The applier must only have delivered entries 1..5, never the stale 6..8.
	for want := 1; want <= 5; want++ {
		select {
		case msg := <-applyCh:
			if msg.CommandIndex != want {
				t.Fatalf("applied out of order: got CommandIndex %d, want %d", msg.CommandIndex, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for applied entry %d", want)
		}
	}
	select {
	case msg := <-applyCh:
		t.Fatalf("applier delivered an entry beyond CommitIndex 5: %+v", msg)
	case <-time.After(100 * time.Millisecond):
		// Expected: nothing more to apply.
	}
}
