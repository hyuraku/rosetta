package kvstore

import (
	"testing"
	"time"

	"rosetta/raft"
)

// TestApplyLoopSkipsNoOp verifies that a no-op entry delivered on the apply
// channel advances lastAppliedIndex/lastAppliedTerm (so compaction accounting
// and ReadIndex catch-up stay correct) without mutating the KV data or matching
// a pending op.
func TestApplyLoopSkipsNoOp(t *testing.T) {
	kvs := NewKVStore(1000)
	defer kvs.Close()

	kvs.GetApplyCh() <- raft.ApplyMsg{
		CommandValid: true,
		Command:      raft.NoOpCommand,
		CommandIndex: 7,
		CommandTerm:  3,
	}

	deadline := time.Now().Add(time.Second)
	for {
		kvs.mu.RLock()
		idx := kvs.lastAppliedIndex
		term := kvs.lastAppliedTerm
		kvs.mu.RUnlock()
		if idx == 7 {
			if term != 3 {
				t.Errorf("lastAppliedTerm = %d, want 3", term)
			}
			if kvs.Size() != 0 {
				t.Errorf("no-op mutated the KV store: size = %d, want 0", kvs.Size())
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("applyLoop did not advance lastAppliedIndex over no-op: got %d, want 7", idx)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestWaitForAppliedBlocksUntilCaughtUp verifies waitForApplied blocks until the
// applied index reaches the requested read index, then returns nil. This is the
// local half of a ReadIndex read (§6.4 step 3).
func TestWaitForAppliedBlocksUntilCaughtUp(t *testing.T) {
	kvs := NewKVStore(1000)
	defer kvs.Close()

	done := make(chan error, 1)
	go func() { done <- kvs.waitForApplied(3) }()

	select {
	case <-done:
		t.Fatal("waitForApplied returned before the applied index reached the target")
	case <-time.After(30 * time.Millisecond):
	}

	kvs.mu.Lock()
	kvs.lastAppliedIndex = 3
	kvs.mu.Unlock()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waitForApplied returned error after catch-up: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waitForApplied did not return after the applied index caught up")
	}
}

// TestGetLinearizableReturnsWrittenValue verifies the ReadIndex read path end to
// end on a single-node leader: a value written with Put is returned by Get.
func TestGetLinearizableReturnsWrittenValue(t *testing.T) {
	kvs, node := newLeaderKVStore(t)
	defer kvs.Close()
	defer node.Kill()

	if err := kvs.Put("k", "v1"); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	got, err := kvs.Get("k")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got != "v1" {
		t.Errorf("Get(k) = %q, want v1", got)
	}
}
