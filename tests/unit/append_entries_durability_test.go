package unit

import (
	"testing"

	"rosetta/raft"
)

const (
	// cmdDurable is the entry whose durability the R2 tests are about.
	cmdDurable = "durable-command"
	// failForever makes recordingPersister fail every save until it is reset.
	failForever = -1
)

// appendOneEntryArgs builds the AppendEntries request used throughout the R2
// tests: leader node2 at term 1 shipping a single entry at absolute index 1.
// A fresh copy is built per call so the resends really are independent requests.
func appendOneEntryArgs() *raft.AppendEntriesArgs {
	return &raft.AppendEntriesArgs{
		Term:         1,
		LeaderID:     nodeID2,
		PrevLogIndex: 0,
		PrevLogTerm:  0,
		Entries: []raft.LogEntry{
			{Term: 1, Index: 1, Command: cmdDurable, Type: "command"},
		},
		LeaderCommit: 0,
	}
}

// R2: a follower must not acknowledge entries it failed to write to stable
// storage — not even when the leader resends the very same request.
//
// Before the fix the failed merge stayed in memory, so mergeLogEntries reported
// "already present" on the resend, the persist was skipped and the follower
// answered Success=true. The leader counts that ACK in MatchIndex and commits,
// which can lose a committed entry when the follower crashes (Figure 2,
// "Persistent state ... updated on stable storage before responding to RPCs";
// §5.4 Leader Completeness).
func TestAppendEntries_RefusesToAckResendAfterPersistFailure(t *testing.T) {
	applyCh := make(chan raft.ApplyMsg, 16)
	drain(applyCh)
	persister := &recordingPersister{}

	rs, err := raft.NewRaftStateWithPersister(nodeID1, []string{nodeID1, nodeID2, "node3"}, applyCh, persister)
	if err != nil {
		t.Fatalf("unexpected setup error: %v", err)
	}

	// A heartbeat first, so the term change is durable and the failures injected
	// below can only hit the log write.
	heartbeat := appendOneEntryArgs()
	heartbeat.Entries = nil
	reply := &raft.AppendEntriesReply{}
	rs.AppendEntries(heartbeat, reply)
	if !reply.Success {
		t.Fatalf("setup heartbeat was rejected: %+v", reply)
	}

	// First delivery: the merge happens but the write fails.
	persister.setFailures(failForever)
	reply = &raft.AppendEntriesReply{}
	rs.AppendEntries(appendOneEntryArgs(), reply)
	if reply.Success {
		t.Fatal("follower acknowledged entries it could not persist")
	}
	if last := rs.GetLastLogIndex(); last != 0 {
		t.Errorf("last log index after the failed write = %d, want 0 (merge rolled back)", last)
	}

	// The leader resends the identical request while storage is still broken.
	reply = &raft.AppendEntriesReply{}
	rs.AppendEntries(appendOneEntryArgs(), reply)
	if reply.Success {
		t.Fatal("follower acknowledged a resend of entries that are still not on stable storage")
	}
	if len(persister.savedLog()) != 0 {
		t.Errorf("persisted log = %+v, want empty", persister.savedLog())
	}

	// Storage recovers: the same request must now be accepted and durable.
	persister.setFailures(0)
	reply = &raft.AppendEntriesReply{}
	rs.AppendEntries(appendOneEntryArgs(), reply)
	if !reply.Success {
		t.Fatal("follower rejected the entries after storage recovered")
	}
	if last := rs.GetLastLogIndex(); last != 1 {
		t.Errorf("last log index after recovery = %d, want 1", last)
	}

	saved := persister.savedLog()
	if len(saved) != 1 {
		t.Fatalf("persisted log holds %d entries, want 1", len(saved))
	}
	if saved[0].Command != cmdDurable || saved[0].Term != 1 || saved[0].Index != 1 {
		t.Errorf("persisted entry = %+v, want index 1, term 1, command %q", saved[0], cmdDurable)
	}
}
