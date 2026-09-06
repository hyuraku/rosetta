package kvstore

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"rosetta/raft"
)

// failNSavesPersister is a minimal raft.Persister that can be told to fail the
// next N calls to SaveRaftState. It exists so a test can make raft.Start return
// a durability error deterministically, without any timing dependence, to
// exercise the pendingOps cleanup path added for KNOWN_ISSUES.md R9.
type failNSavesPersister struct {
	mu       sync.Mutex
	saved    raft.PersistentState
	failures int
}

func (p *failNSavesPersister) setFailures(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failures = n
}

func (p *failNSavesPersister) SaveRaftState(state *raft.PersistentState) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.failures != 0 {
		if p.failures > 0 {
			p.failures--
		}
		return fmt.Errorf("simulated persist failure")
	}

	p.saved = *state
	p.saved.Log = append([]raft.LogEntry(nil), state.Log...)
	return nil
}

func (p *failNSavesPersister) LoadRaftState() (*raft.PersistentState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	loaded := p.saved
	loaded.Log = append([]raft.LogEntry(nil), p.saved.Log...)
	return &loaded, nil
}

// newLeaderKVStoreWithPersister is newLeaderKVStore (store_leadership_test.go)
// with an injectable raft.Persister, so a test can fail a specific future
// persist deterministically.
func newLeaderKVStoreWithPersister(t *testing.T, persister raft.Persister) (*KVStore, *raft.RaftNode) {
	t.Helper()

	kvs := NewKVStore(1000)
	transport := raft.NewMockTransport()
	node, err := raft.NewRaftNodeWithPersister("node1", []string{"node1"}, transport, kvs.GetApplyCh(), persister)
	if err != nil {
		t.Fatalf("NewRaftNodeWithPersister: %v", err)
	}
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

// TestExecuteOperationWithResultCleansUpPendingOpOnStartFailure guards the
// leak-prevention half of the R9 fix: moving the pendingOps registration to
// before raft.Start means a Start that fails (here: a persist error, so
// isLeader stays true but err != nil) must explicitly remove the registration
// it just added, or it leaks forever -- nothing will ever be applied for an
// opID that was never durably appended, so nothing will ever deliver to (or
// clean up) that channel.
func TestExecuteOperationWithResultCleansUpPendingOpOnStartFailure(t *testing.T) {
	persister := &failNSavesPersister{}
	kvs, node := newLeaderKVStoreWithPersister(t, persister)
	defer kvs.Close()
	defer node.Kill()

	kvs.opMu.RLock()
	before := len(kvs.pendingOps)
	kvs.opMu.RUnlock()

	// Fail exactly the next persist, which is the one raft.Start triggers below.
	persister.setFailures(1)

	result := kvs.executeOperationWithResult(OpPut, "k", "v", "", 0)
	if result.Err == nil {
		t.Fatal("expected an error when the raft persist backing Start fails")
	}

	kvs.opMu.RLock()
	after := len(kvs.pendingOps)
	kvs.opMu.RUnlock()
	if after != before {
		t.Fatalf("pendingOps leaked an entry after a failed Start: had %d before, %d after", before, after)
	}
}

// TestLateRegistrationLosesCommittedResult documents, and would have caught,
// the exact failure mode R9 fixed: a pendingOps entry registered only after
// commit+apply already happened for that opID never receives a result, because
// applyLoop's delivery (kvstore/store.go, "if ch, exists := kvs.pendingOps[cmd.ID]")
// only fires once, at apply time, and finds nothing to send to.
//
// This cannot be demonstrated as a black-box race against the fixed
// executeOperationWithResult itself: the window the old code left open (between
// raft.Start returning and the very next line registering pendingOps) was a
// handful of nanoseconds, while commit+apply for a single-node cluster is
// driven by the 50ms heartbeat tick (raft/state.go's raftTickInterval) -- a
// fixed constant in raft/, out of scope for this change (a concurrent branch is
// modifying raft/rpc.go). Forcing the two to interleave deterministically would
// need a test seam inside raft/ or inside store.go's hot path, and neither is a
// minimal change for this fix. Instead, this test drives the same real
// components the production code does (raft.Start, and the real applyLoop
// consuming the real applyCh) but registers deliberately late, to prove the
// invariant the fix now guarantees can never occur in practice: register before
// Start, never after.
func TestLateRegistrationLosesCommittedResult(t *testing.T) {
	kvs, node := newLeaderKVStore(t)
	defer kvs.Close()
	defer node.Kill()

	const key, value = "late-key", "late-value"
	opID := fmt.Sprintf("%s-late-registration-demo", node.GetNodeID())
	cmd := Command{Op: OpPut, Key: key, Value: value, ID: opID}
	cmdBytes, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal command: %v", err)
	}

	if _, _, isLeader, startErr := node.Start(string(cmdBytes)); !isLeader || startErr != nil {
		t.Fatalf("Start: isLeader=%v err=%v", isLeader, startErr)
	}

	// Let the single-node cluster's own heartbeat tick commit and apply the
	// entry -- well past raftTickInterval -- with nobody registered for its
	// opID yet. This is the scenario the old code risked on every operation,
	// just stretched from nanoseconds to milliseconds so the test is
	// deterministic instead of relying on scheduler luck.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		kvs.mu.RLock()
		got, ok := kvs.data[key]
		kvs.mu.RUnlock()
		if ok && got == value {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	kvs.mu.RLock()
	got, ok := kvs.data[key]
	kvs.mu.RUnlock()
	if !ok || got != value {
		t.Fatal("expected the entry to already be committed and applied before registering pendingOps")
	}

	// This is the pre-R9 order: register only now, after apply already ran and
	// found nothing in pendingOps to deliver the result to.
	resultCh := make(chan Result, 1)
	kvs.opMu.Lock()
	kvs.pendingOps[opID] = resultCh
	kvs.opMu.Unlock()

	select {
	case r := <-resultCh:
		t.Fatalf("expected no result to ever arrive for a registration this late, got %+v", r)
	case <-time.After(200 * time.Millisecond):
		// No result ever arrives: this is exactly the silent drop R9 describes.
	}
}
