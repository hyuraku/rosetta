package kvstore

import (
	"testing"
	"time"

	"rosetta/raft"
)

// newLeaderKVStore spins up a single-node cluster and waits for it to win the
// election so that operations can be submitted and committed.
func newLeaderKVStore(t *testing.T) (*KVStore, *raft.RaftNode) {
	t.Helper()

	kvs := NewKVStore(1000)
	transport := raft.NewMockTransport()
	node := raft.NewRaftNode("node1", []string{"node1"}, transport, kvs.GetApplyCh())
	transport.RegisterNode("node1", node)
	kvs.SetRaft(node)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if node.IsLeader() {
			return kvs, node
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("node did not become leader")
	return nil, nil
}

// TestAwaitOperationResultAfterLeadershipLoss verifies that once a command's
// result has been delivered (meaning the entry was committed and applied), the
// operation returns that result even if this node has since stepped down. A
// committed entry is durable under Raft's Leader Completeness Property, so
// reporting "leadership lost" after apply would be a false negative.
func TestAwaitOperationResultAfterLeadershipLoss(t *testing.T) {
	kvs, node := newLeaderKVStore(t)
	defer kvs.Close()
	defer node.Kill()

	opID := "node1-test-leadership-loss"
	resultCh := make(chan Result, 1)
	kvs.opMu.Lock()
	kvs.pendingOps[opID] = resultCh
	kvs.opMu.Unlock()

	// The apply loop delivers the committed result before the node steps down.
	committed := Result{Value: "committed-value"}
	resultCh <- committed

	// Leadership is lost after the entry was already committed and applied.
	node.GetRaftState().SetState(raft.Follower)

	got := kvs.awaitOperationResult(resultCh, opID)
	if got.Err != nil {
		t.Fatalf("expected committed result after leadership loss, got error: %v", got.Err)
	}
	if got.Value != committed.Value {
		t.Fatalf("expected value %q, got %q", committed.Value, got.Value)
	}
}

// TestAwaitOperationResultAfterTermChange verifies the same durability guarantee
// when the term has advanced past the term at which the command was submitted.
func TestAwaitOperationResultAfterTermChange(t *testing.T) {
	kvs, node := newLeaderKVStore(t)
	defer kvs.Close()
	defer node.Kill()

	opID := "node1-test-term-change"
	resultCh := make(chan Result, 1)
	kvs.opMu.Lock()
	kvs.pendingOps[opID] = resultCh
	kvs.opMu.Unlock()

	committed := Result{Value: "committed-value"}
	resultCh <- committed

	// The term advances after the entry was already committed and applied.
	node.GetRaftState().IncrementTerm()

	got := kvs.awaitOperationResult(resultCh, opID)
	if got.Err != nil {
		t.Fatalf("expected committed result after term change, got error: %v", got.Err)
	}
	if got.Value != committed.Value {
		t.Fatalf("expected value %q, got %q", committed.Value, got.Value)
	}
}
