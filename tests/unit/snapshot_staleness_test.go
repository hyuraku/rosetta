package unit

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"rosetta/kvstore"
	"rosetta/raft"
)

// TestInstallSnapshotIgnoredBelowLastApplied is the R5 regression test for the
// Raft side of the guard.
//
// A follower that has applied through index N must not accept a snapshot that
// ends at M < N. The receiver used to compare the incoming snapshot only against
// its own LastIncludedIndex, which stays at 0 until this node compacts, so a
// delayed or duplicated InstallSnapshot carrying an old snapshot was accepted:
// the log was cut back to the stale boundary, CommitIndex/LastApplied were left
// pointing into it, and the payload went down applyCh and overwrote the state
// machine with an earlier version of the data — committed writes silently
// disappearing.
func TestInstallSnapshotIgnoredBelowLastApplied(t *testing.T) {
	kvs := kvstore.NewKVStore(0) // memory only: no snapshotter wired
	defer kvs.Close()

	rs := raft.NewRaftState("node1", []string{"node1"}, kvs.GetApplyCh())

	// Commit and apply three writes, so both Raft and the KV store are at index 3.
	const applied = 3
	for i := 1; i <= applied; i++ {
		appendKVCommand(t, rs, fmt.Sprintf("k%d", i), fmt.Sprintf("v%d", i))
	}
	rs.UpdateCommitIndex(applied)
	if !waitForKVValue(kvs, "k3", "v3") {
		t.Fatalf("setup: KV store did not apply through index %d", applied)
	}
	if got := rs.GetLastApplied(); got != applied {
		t.Fatalf("setup: LastApplied = %d, want %d", got, applied)
	}

	// A stale snapshot ending at index 2 arrives from a leader in a later term.
	stale, err := json.Marshal(&kvstore.SnapshotData{KVData: map[string]string{"k1": "rolled-back"}})
	if err != nil {
		t.Fatalf("marshal stale snapshot: %v", err)
	}
	reply := &raft.InstallSnapshotReply{}
	rs.InstallSnapshot(&raft.InstallSnapshotArgs{
		Term:              1,
		LeaderID:          "leader",
		LastIncludedIndex: applied - 1,
		LastIncludedTerm:  1,
		Data:              stale,
	}, reply)

	// The term is still accepted (this is valid leader traffic), but nothing
	// about the log or the applied position may move.
	if reply.Term != 1 {
		t.Fatalf("reply.Term = %d, want 1", reply.Term)
	}
	if got := rs.GetLastApplied(); got != applied {
		t.Fatalf("stale snapshot moved LastApplied to %d, want %d", got, applied)
	}
	if got := rs.GetCommitIndex(); got != applied {
		t.Fatalf("stale snapshot moved CommitIndex to %d, want %d", got, applied)
	}
	if got := rs.GetLastLogIndex(); got != applied {
		t.Fatalf("stale snapshot truncated the log to index %d, want %d", got, applied)
	}
	if idx, term := rs.GetSnapshotMetadata(); idx != 0 || term != 0 {
		t.Fatalf("stale snapshot moved the snapshot boundary to (%d, %d), want (0, 0)", idx, term)
	}

	// The state machine must not have been rolled back either.
	if got := kvs.GetSnapshot()["k1"]; got != "v1" {
		t.Fatalf("stale snapshot rolled the KV store back: k1 = %q, want %q", got, "v1")
	}

	// And the node keeps working: the next committed command still applies.
	appendKVCommand(t, rs, "k4", "v4")
	rs.UpdateCommitIndex(applied + 1)
	if !waitForKVValue(kvs, "k4", "v4") {
		t.Fatalf("command after the ignored snapshot was never applied")
	}
}

// appendKVCommand appends a KV PUT to the log the way the leader would.
func appendKVCommand(t *testing.T, rs *raft.RaftState, key, value string) {
	t.Helper()
	cmd, err := json.Marshal(kvstore.Command{
		Op:    kvstore.OpPut,
		Key:   key,
		Value: value,
		ID:    "op-" + key,
	})
	if err != nil {
		t.Fatalf("marshal command for %s: %v", key, err)
	}
	if _, err := rs.AppendLogEntry(cmd, "command"); err != nil {
		t.Fatalf("append command for %s: %v", key, err)
	}
}

// waitForKVValue polls the store until key holds want, or the budget runs out.
func waitForKVValue(kvs *kvstore.KVStore, key, want string) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if kvs.GetSnapshot()[key] == want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
