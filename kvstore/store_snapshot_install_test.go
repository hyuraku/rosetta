package kvstore

import (
	"encoding/json"
	"testing"
	"time"

	"rosetta/raft"
)

// TestInstallSnapshotFromApplyMsgV2RestoresKVAndSessions verifies that a V2
// snapshot (SnapshotData carrying both KVData and Sessions) received via the
// apply channel is fully restored: the key/value data AND the duplicate
// detection sessions. Session restoration is part of the at-most-once guarantee
// (Raft paper Section 8): a follower that installs a snapshot must keep
// deduplicating client retries even after being promoted to leader.
func TestInstallSnapshotFromApplyMsgV2RestoresKVAndSessions(t *testing.T) {
	kvs := NewKVStore(0)
	defer kvs.Close()

	sd := &SnapshotData{
		KVData: map[string]string{"a": "1", "b": "2"},
		Sessions: map[string]*ClientSession{
			"client-a": {LastSeqNum: 7, LastResult: Result{Value: "cached-7"}},
		},
	}
	raw, err := json.Marshal(sd)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}

	kvs.installSnapshotFromApplyMsg(&raft.ApplyMsg{
		SnapshotValid: true,
		SnapshotIndex: 10,
		SnapshotTerm:  3,
		SnapshotData:  raw,
	})

	// KV data restored.
	if got := kvs.GetSnapshot()["a"]; got != "1" {
		t.Fatalf("KVData not restored: key a = %q, want %q", got, "1")
	}
	if got := kvs.GetSnapshot()["b"]; got != "2" {
		t.Fatalf("KVData not restored: key b = %q, want %q", got, "2")
	}

	// Sessions restored: a retry reusing seqNum 7 from client-a must be treated
	// as a duplicate, returning the cached result WITHOUT re-applying the write.
	r := kvs.executeCommand(&Command{Op: OpPut, Key: "z", Value: "should-not-write", ClientID: "client-a", SeqNum: 7})
	if r.Value != "cached-7" {
		t.Fatalf("duplicate seqNum did not return cached result: got %+v", r)
	}
	if _, exists := kvs.GetSnapshot()["z"]; exists {
		t.Fatalf("duplicate request was re-applied: key z should not exist")
	}
}

// TestInstallSnapshotFromApplyMsgV1BackwardCompat verifies that a legacy V1
// snapshot (a bare map[string]string) is still accepted after the receiver was
// upgraded to prefer V2. This preserves compatibility with snapshots produced
// by the V1 save path.
func TestInstallSnapshotFromApplyMsgV1BackwardCompat(t *testing.T) {
	kvs := NewKVStore(0)
	defer kvs.Close()

	raw, err := json.Marshal(map[string]string{"legacy": "value"})
	if err != nil {
		t.Fatalf("marshal V1 snapshot: %v", err)
	}

	kvs.installSnapshotFromApplyMsg(&raft.ApplyMsg{
		SnapshotValid: true,
		SnapshotIndex: 5,
		SnapshotTerm:  2,
		SnapshotData:  raw,
	})

	if got := kvs.GetSnapshot()["legacy"]; got != "value" {
		t.Fatalf("V1 snapshot not restored: got %q, want %q", got, "value")
	}
}

// TestApplyLoopUpdatesLastAppliedTerm verifies that applying a committed command
// advances both lastAppliedIndex and lastAppliedTerm using the absolute term
// carried on ApplyMsg.CommandTerm. A correct lastAppliedTerm is required so that
// a snapshot taken from the apply loop records the right LastIncludedTerm.
func TestApplyLoopUpdatesLastAppliedTerm(t *testing.T) {
	kvs := NewKVStore(0)
	defer kvs.Close()

	cmd := Command{Op: OpPut, Key: "k", Value: "v", ID: "op-1"}
	cmdBytes, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal command: %v", err)
	}

	kvs.GetApplyCh() <- raft.ApplyMsg{
		CommandValid: true,
		Command:      cmdBytes,
		CommandIndex: 5,
		CommandTerm:  3,
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		kvs.mu.RLock()
		idx, term := kvs.lastAppliedIndex, kvs.lastAppliedTerm
		kvs.mu.RUnlock()
		if idx == 5 && term == 3 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	kvs.mu.RLock()
	idx, term := kvs.lastAppliedIndex, kvs.lastAppliedTerm
	kvs.mu.RUnlock()
	t.Fatalf("lastApplied not updated: index=%d (want 5), term=%d (want 3)", idx, term)
}
