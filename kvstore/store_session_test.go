package kvstore

import (
	"testing"
)

// Shared literals, factored out to satisfy the goconst linter.
const (
	sessKey     = "k"
	sessV1      = "v1"
	sessV2      = "v2"
	sessV3      = "v3"
	sessClient  = "client-a"
	sessChanged = "changed"
)

// TestExecuteCommandDedupSkipsReapplication verifies that a duplicate request
// (same ClientID and SeqNum) returns the cached result and does NOT re-apply
// the operation to the state machine (Raft paper Section 8, at-most-once).
func TestExecuteCommandDedupSkipsReapplication(t *testing.T) {
	kvs := NewKVStore(1000)
	defer kvs.Close()

	// First application of seq 1 from client "c" sets the value.
	if r := kvs.executeCommand(&Command{Op: OpPut, Key: sessKey, Value: sessV1, ID: "op-1", ClientID: sessClient, SeqNum: 1}); r.Err != nil {
		t.Fatalf("first apply should succeed, got %v", r.Err)
	}

	// An unrelated client changes the value so we can detect a re-application.
	kvs.executeCommand(&Command{Op: OpPut, Key: sessKey, Value: sessChanged, ID: "op-x", ClientID: "other", SeqNum: 1})

	// A retry from client "client-a" with the same seq must be treated as a
	// duplicate: the cached (successful) result is returned and the write skipped.
	r := kvs.executeCommand(&Command{Op: OpPut, Key: sessKey, Value: sessV1, ID: "op-2", ClientID: sessClient, SeqNum: 1})
	if r.Err != nil {
		t.Fatalf("duplicate request should return cached success, got %v", r.Err)
	}

	if got := kvs.GetSnapshot()[sessKey]; got != sessChanged {
		t.Fatalf("duplicate must not re-apply the write: expected %q, got %q", sessChanged, got)
	}
}

// TestExecuteCommandAppliesAdvancingSeqNum verifies that requests with an
// advancing sequence number are applied normally.
func TestExecuteCommandAppliesAdvancingSeqNum(t *testing.T) {
	kvs := NewKVStore(1000)
	defer kvs.Close()

	kvs.executeCommand(&Command{Op: OpPut, Key: sessKey, Value: sessV1, ID: "op-1", ClientID: sessClient, SeqNum: 1})
	kvs.executeCommand(&Command{Op: OpPut, Key: sessKey, Value: sessV2, ID: "op-2", ClientID: sessClient, SeqNum: 2})

	if got := kvs.GetSnapshot()[sessKey]; got != sessV2 {
		t.Fatalf("advancing seq should apply: expected %q, got %q", sessV2, got)
	}
}

// TestExecuteCommandNoDedupWithoutClientID verifies backward compatibility:
// commands without a ClientID bypass duplicate detection and always apply.
func TestExecuteCommandNoDedupWithoutClientID(t *testing.T) {
	kvs := NewKVStore(1000)
	defer kvs.Close()

	kvs.executeCommand(&Command{Op: OpPut, Key: sessKey, Value: sessV1, ID: "op-1"})
	kvs.executeCommand(&Command{Op: OpPut, Key: sessKey, Value: sessV2, ID: "op-2"})

	if got := kvs.GetSnapshot()[sessKey]; got != sessV2 {
		t.Fatalf("no-ClientID commands should always apply: expected %q, got %q", sessV2, got)
	}
}

// TestPutWithSessionResendNotReapplied exercises the full write path through
// Raft (PutWithSession -> Start -> applyLoop -> executeCommand). A resend with
// the same SeqNum must be deduplicated even though it produces a new opID, and
// the cached result must still be delivered back to the caller (proving the
// pendingOps handoff works for the retried opID).
func TestPutWithSessionResendNotReapplied(t *testing.T) {
	kvs, node := newLeaderKVStore(t)
	defer kvs.Close()
	defer node.Kill()

	if err := kvs.PutWithSession(sessKey, sessV1, sessClient, 1); err != nil {
		t.Fatalf("initial PutWithSession failed: %v", err)
	}

	// Simulate a client retry after a timeout: same SeqNum, different value.
	// Duplicate detection must return the cached result and skip the write.
	if err := kvs.PutWithSession(sessKey, "v2-should-be-ignored", sessClient, 1); err != nil {
		t.Fatalf("resend with same SeqNum should return cached result, got: %v", err)
	}
	if got := kvs.GetSnapshot()[sessKey]; got != sessV1 {
		t.Fatalf("resend must not overwrite state: expected %q, got %q", sessV1, got)
	}

	// A new SeqNum applies normally.
	if err := kvs.PutWithSession(sessKey, sessV3, sessClient, 2); err != nil {
		t.Fatalf("PutWithSession with advancing SeqNum failed: %v", err)
	}
	if got := kvs.GetSnapshot()[sessKey]; got != sessV3 {
		t.Fatalf("advancing SeqNum should apply: expected %q, got %q", sessV3, got)
	}
}

// TestDeleteWithSessionResendNotReapplied verifies the same at-most-once
// guarantee for Delete: a resend of the same SeqNum is deduplicated.
func TestDeleteWithSessionResendNotReapplied(t *testing.T) {
	kvs, node := newLeaderKVStore(t)
	defer kvs.Close()
	defer node.Kill()

	if err := kvs.PutWithSession(sessKey, sessV1, sessClient, 1); err != nil {
		t.Fatalf("PutWithSession failed: %v", err)
	}
	if err := kvs.DeleteWithSession(sessKey, sessClient, 2); err != nil {
		t.Fatalf("DeleteWithSession failed: %v", err)
	}
	if _, ok := kvs.GetSnapshot()[sessKey]; ok {
		t.Fatalf("key should be deleted")
	}

	// Re-add the key out of band, then resend the delete with the same SeqNum.
	// The duplicate delete must be skipped, leaving the key in place.
	kvs.executeCommand(&Command{Op: OpPut, Key: sessKey, Value: sessChanged, ID: "op-readd"})
	if err := kvs.DeleteWithSession(sessKey, sessClient, 2); err != nil {
		t.Fatalf("resend delete should return cached result, got: %v", err)
	}
	if got, ok := kvs.GetSnapshot()[sessKey]; !ok || got != sessChanged {
		t.Fatalf("duplicate delete must not re-apply: expected key present with %q, got ok=%v val=%q", sessChanged, ok, got)
	}
}
