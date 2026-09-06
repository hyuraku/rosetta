package raft

import (
	"testing"
	"time"
)

// commandEntries builds terms-1 command entries for absolute indices from..to.
func commandEntries(from, to int) []LogEntry {
	entries := make([]LogEntry, 0, to-from+1)
	for i := from; i <= to; i++ {
		entries = append(entries, LogEntry{Term: 1, Index: i, Command: "cmd", Type: entryTypeCommand})
	}
	return entries
}

// waitForLastApplied blocks until the applier has claimed through want, which is
// how a test knows the applier is inside a delivery and holding no lock.
func waitForLastApplied(t *testing.T, rs *RaftState, want int) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rs.GetLastApplied() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("applier never claimed through index %d (LastApplied = %d)", want, rs.GetLastApplied())
}

// TestSlowStateMachineDoesNotBlockConsensus is the B3 regression test: a state
// machine that never drains applyCh must not stop the node from serving RPCs or
// from timing out into an election.
//
// Before the applier split this failed on the very first assertion. AppendEntries
// advanced the commit index and then sent to applyCh from inside its own rs.mu
// critical section, so with nobody reading the channel the handler never
// returned, and every other RPC — plus the election timer's own reset — queued up
// behind the lock it still held.
func TestSlowStateMachineDoesNotBlockConsensus(t *testing.T) {
	// Unbuffered and deliberately never read: the applier blocks on its first
	// delivery and stays there for the whole test.
	applyCh := make(chan ApplyMsg)
	rs := NewRaftState("n1", []string{"n1", "n2", "n3"}, applyCh)
	defer rs.Stop()

	appended := make(chan struct{})
	go func() {
		defer close(appended)
		rs.AppendEntries(&AppendEntriesArgs{
			Term:         1,
			LeaderID:     "n2",
			PrevLogIndex: 0,
			PrevLogTerm:  0,
			Entries:      commandEntries(1, 3),
			LeaderCommit: 3,
		}, &AppendEntriesReply{})
	}()
	select {
	case <-appended:
	case <-time.After(2 * time.Second):
		t.Fatal("AppendEntries never returned: the commit path is blocked behind the state machine")
	}

	// The applier is now stuck mid-delivery with no lock held.
	waitForLastApplied(t, rs, 3)

	// A second RPC must still be served while that delivery is stuck.
	served := make(chan RequestVoteReply, 1)
	go func() {
		reply := RequestVoteReply{}
		rs.RequestVote(&RequestVoteArgs{
			Term:         2,
			CandidateID:  "n3",
			LastLogIndex: 3,
			LastLogTerm:  1,
		}, &reply)
		served <- reply
	}()
	select {
	case reply := <-served:
		if !reply.VoteGranted {
			t.Fatalf("vote not granted: %+v", reply)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RequestVote never returned: rs.mu is held across the applyCh send")
	}

	// And the election timer must still be running, so this node can campaign.
	select {
	case <-rs.ElectionTimer():
	case <-time.After(2 * time.Second):
		t.Fatal("election timer never fired while the state machine was stalled")
	}
}

// TestApplierOrdersSnapshotAheadOfLaterCommands checks the ordering the state
// machine is promised across the two apply streams: a snapshot at index N is
// delivered before every command above N, even when it is installed while the
// applier is in the middle of an older batch. Commands at or below N may still
// trail it — those were claimed before the snapshot existed, and the state
// machine's monotonicity guard drops them (KNOWN_ISSUES.md R5).
func TestApplierOrdersSnapshotAheadOfLaterCommands(t *testing.T) {
	// Unbuffered, so nothing moves until this test reads it: that is what pins
	// the applier inside the first batch while the snapshot is installed.
	applyCh := make(chan ApplyMsg)
	rs := NewRaftState("n1", []string{"n1", "n2"}, applyCh)
	defer rs.Stop()

	reply := &AppendEntriesReply{}
	rs.AppendEntries(&AppendEntriesArgs{
		Term:         1,
		LeaderID:     "n2",
		PrevLogIndex: 0,
		PrevLogTerm:  0,
		Entries:      commandEntries(1, 3),
		LeaderCommit: 3,
	}, reply)
	if !reply.Success {
		t.Fatalf("setup AppendEntries failed: %+v", reply)
	}
	waitForLastApplied(t, rs, 3)

	// A snapshot lands while commands 1..3 are in flight.
	rs.InstallSnapshot(&InstallSnapshotArgs{
		Term:              1,
		LeaderID:          "n2",
		LastIncludedIndex: 5,
		LastIncludedTerm:  1,
		Data:              []byte("snapshot"),
	}, &InstallSnapshotReply{})
	if idx, term := rs.GetSnapshotMetadata(); idx != 5 || term != 1 {
		t.Fatalf("snapshot boundary = (%d, %d), want (5, 1)", idx, term)
	}

	// And a command above the snapshot follows it.
	reply = &AppendEntriesReply{}
	rs.AppendEntries(&AppendEntriesArgs{
		Term:         1,
		LeaderID:     "n2",
		PrevLogIndex: 5,
		PrevLogTerm:  1,
		Entries:      commandEntries(6, 6),
		LeaderCommit: 6,
	}, reply)
	if !reply.Success {
		t.Fatalf("post-snapshot AppendEntries failed: %+v", reply)
	}

	type delivery struct {
		snapshot bool
		index    int
	}
	want := []delivery{
		{index: 1}, {index: 2}, {index: 3},
		{snapshot: true, index: 5},
		{index: 6},
	}
	for i, w := range want {
		select {
		case msg := <-applyCh:
			got := delivery{snapshot: msg.SnapshotValid, index: msg.CommandIndex}
			if msg.SnapshotValid {
				got.index = msg.SnapshotIndex
			}
			if got != w {
				t.Fatalf("delivery %d = %+v, want %+v", i, got, w)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for delivery %d (%+v)", i, w)
		}
	}

	select {
	case msg := <-applyCh:
		t.Fatalf("unexpected extra delivery: %+v", msg)
	case <-time.After(50 * time.Millisecond):
	}
}
