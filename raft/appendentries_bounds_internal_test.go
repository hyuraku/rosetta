package raft

import (
	"testing"
	"time"
)

// R13-3 (PR #26 followup): replicateToPeer used to read
// rs.persistent.Log[prevLogIndex-lastIncludedIndex-1] without checking that
// prevLogIndex was still within the live log. A NextIndex left pointing past a
// log that was later shortened (TruncateLogAfter) made prevLogIndex exceed
// lastAbsLogIndex(), and that slice index ran past len(Log): a panic.
//
// This reproduces the scenario directly: seed a log, advance NextIndex[peer]
// past where the log will end, then shrink the log out from under it with
// TruncateLogAfter, and run a replication round. Before the fix this panics.
func TestReplicateToPeerClampsNextIndexAfterTruncation(t *testing.T) {
	// This round reaches quorum on entries 1..2 (see below) and the applier
	// delivers them asynchronously, so the channel is drained inline by index
	// rather than closed — closing it here would race the applier's send.
	applyCh := make(chan ApplyMsg, 32)

	const peerID = "n2"
	rs := NewRaftState("n1", []string{"n1", peerID}, applyCh)
	rs.mu.Lock()
	rs.persistent.CurrentTerm = 1
	rs.mu.Unlock()
	rs.SetState(Leader)

	for i := 0; i < 5; i++ {
		if _, err := rs.AppendLogEntry("seed", entryTypeCommand); err != nil {
			t.Fatalf("seeding log: %v", err)
		}
	}
	if last := rs.GetLastLogIndex(); last != 5 {
		t.Fatalf("setup: last log index = %d, want 5", last)
	}

	// Advance the peer's NextIndex as if earlier rounds had already caught it
	// up to (and slightly past) the log's current end.
	rs.mu.Lock()
	rs.leader.NextIndex[peerID] = 6
	rs.mu.Unlock()

	// Now shrink the log out from under that NextIndex. TruncateLogAfter(2)
	// drops entries 3..5, leaving lastAbsLogIndex() == 2 — three below the
	// NextIndex the leader still holds for this peer.
	if err := rs.TruncateLogAfter(2); err != nil {
		t.Fatalf("TruncateLogAfter: %v", err)
	}

	// Before the fix: prevLogIndex = NextIndex-1 = 5, and
	// rs.persistent.Log[5-0-1] indexes position 4 into a 2-entry log — panic.
	//
	// After the clamp, the round replicates entries 1..2 (all this 2-node
	// cluster's peer needs), the fake transport reports Success, and that
	// reaches quorum: CommitIndex advances to 2 and the applier delivers both
	// entries.
	rs.replicateToPeer(marshalTransport{}, peerID, 1, 0)

	// The clamp must also have corrected the map entry itself (not just a
	// local copy used for this one round), so a later round does not have to
	// rediscover the same staleness — see clampNextIndexIfStale.
	rs.mu.RLock()
	got := rs.leader.NextIndex[peerID]
	rs.mu.RUnlock()
	if want := rs.GetLastLogIndex() + 1; got != want {
		t.Errorf("NextIndex[%s] after the clamped round = %d, want %d (lastAbsLogIndex+1)", peerID, got, want)
	}

	for want := 1; want <= 2; want++ {
		select {
		case msg := <-applyCh:
			if msg.CommandIndex != want {
				t.Fatalf("applied out of order: got CommandIndex %d, want %d", msg.CommandIndex, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for applied entry %d", want)
		}
	}
}
