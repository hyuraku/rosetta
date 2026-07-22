package raft

import "testing"

// TestNoOpCommitsPriorTermEntry is a white-box test of the Figure 8 / §5.4.2
// commit rule and the role of the leader's no-op. It seeds a term-1 entry that
// is replicated to a majority but not committed, promotes the node to leader in
// term 2 (which appends a term-2 no-op), and verifies that:
//
//   - a majority storing only the prior-term entry does NOT make it committed
//     (a leader may not commit a previous term's entry by replica count alone);
//   - once a majority acks the current-term no-op, the commit index advances over
//     BOTH the no-op and the prior-term entry (indirect commit).
func TestNoOpCommitsPriorTermEntry(t *testing.T) {
	applyCh := make(chan ApplyMsg, 16)
	go func() {
		for range applyCh { //nolint:revive // drain applied entries
		}
	}()

	peers := []string{"n1", "n2", "n3"}
	rs := NewRaftState("n1", peers, applyCh)

	// History: a single term-1 entry, already replicated but uncommitted.
	rs.mu.Lock()
	rs.persistent.CurrentTerm = 1
	rs.persistent.Log = []LogEntry{{Term: 1, Index: 1, Command: "old", Type: "command"}}

	// Advance to term 2 and win the election; becomeLeader appends the term-2
	// no-op at index 2.
	rs.persistent.CurrentTerm = 2
	rs.state = Candidate
	rs.becomeLeader()
	if got := rs.lastAbsLogIndex(); got != 2 {
		rs.mu.Unlock()
		t.Fatalf("after becomeLeader last index = %d, want 2 (old entry + no-op)", got)
	}
	if e := rs.persistent.Log[1]; e.Term != 2 || e.Command != NoOpCommand {
		rs.mu.Unlock()
		t.Fatalf("index 2 = %+v, want term-2 no-op", e)
	}

	// A majority stores the prior-term entry (index 1) but not the no-op yet.
	rs.leader.MatchIndex["n2"] = 1
	rs.leader.MatchIndex["n3"] = 1
	rs.updateCommitIndex()
	commitPriorOnly := rs.volatile.CommitIndex
	rs.mu.Unlock()

	if commitPriorOnly != 0 {
		t.Fatalf("prior-term entry committed by replica count alone: commitIndex=%d, want 0", commitPriorOnly)
	}

	// The majority now acks the current-term no-op (index 2). Committing it must
	// carry the prior-term entry to committed as well.
	rs.mu.Lock()
	rs.leader.MatchIndex["n2"] = 2
	rs.leader.MatchIndex["n3"] = 2
	rs.updateCommitIndex()
	commit := rs.volatile.CommitIndex
	rs.mu.Unlock()

	if commit != 2 {
		t.Fatalf("committing the no-op did not indirectly commit the prior-term entry: commitIndex=%d, want 2", commit)
	}
}
