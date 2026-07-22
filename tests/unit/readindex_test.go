package unit

import (
	"testing"
	"time"

	"rosetta/raft"
)

// drain consumes an apply channel so the raft state machine never blocks.
func drain(ch chan raft.ApplyMsg) {
	go func() {
		for range ch { //nolint:revive // draining
		}
	}()
}

// pollUntil returns true if cond becomes true within d.
func pollUntil(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// TestLeaderAppendsNoOpOnElection verifies that a freshly elected leader appends
// a current-term no-op entry as the first thing it does (§5.4.2 / §6.4).
func TestLeaderAppendsNoOpOnElection(t *testing.T) {
	transport := raft.NewMockTransport()
	applyCh := make(chan raft.ApplyMsg, 16)
	drain(applyCh)

	node := raft.NewRaftNode("node1", []string{"node1"}, transport, applyCh)
	transport.RegisterNode("node1", node)
	defer node.Kill()

	if !pollUntil(2*time.Second, node.IsLeader) {
		t.Fatal("single-node cluster did not elect a leader")
	}

	term, _ := node.GetState()
	entry := node.GetRaftState().GetLogEntry(1)
	if entry == nil {
		t.Fatal("expected a no-op entry at index 1 after election, got nil")
	}
	if entry.Term != term {
		t.Errorf("no-op term = %d, want current term %d", entry.Term, term)
	}
	if s, ok := entry.Command.(string); !ok || s != raft.NoOpCommand {
		t.Errorf("index 1 command = %v, want no-op marker %q", entry.Command, raft.NoOpCommand)
	}
}

// TestReadIndexConfirmsWithQuorum verifies the ReadIndex happy path on a healthy
// multi-node cluster: it returns a committed index (>= the current commit index)
// after confirming leadership with a quorum of heartbeats. The no-op guarantees
// a current-term commit exists so ReadIndex does not have to wait.
func TestReadIndexConfirmsWithQuorum(t *testing.T) {
	peers := []string{"n1", "n2", "n3"}
	transport := raft.NewMockTransport()
	nodes := make(map[string]*raft.RaftNode)

	for _, id := range peers {
		ch := make(chan raft.ApplyMsg, 100)
		drain(ch)
		node := raft.NewRaftNode(id, peers, transport, ch)
		nodes[id] = node
		transport.RegisterNode(id, node)
	}
	defer func() {
		for _, n := range nodes {
			n.Kill()
		}
	}()

	var leader *raft.RaftNode
	if !pollUntil(3*time.Second, func() bool {
		for _, n := range nodes {
			if n.IsLeader() {
				leader = n
				return true
			}
		}
		return false
	}) {
		t.Fatal("no leader elected")
	}

	// Wait for the no-op to commit so a current-term entry exists.
	if !pollUntil(2*time.Second, func() bool {
		return leader.GetRaftState().GetCommitIndex() >= 1
	}) {
		t.Fatal("leader did not commit its no-op")
	}

	commit := leader.GetRaftState().GetCommitIndex()
	readIndex, err := leader.ReadIndex()
	if err != nil {
		t.Fatalf("ReadIndex on healthy leader failed: %v", err)
	}
	if readIndex < commit {
		t.Errorf("ReadIndex = %d, want >= commit index %d", readIndex, commit)
	}

	// A follower must refuse: reads are leader-only.
	for _, n := range nodes {
		if !n.IsLeader() {
			if _, err := n.ReadIndex(); err == nil {
				t.Errorf("follower %s ReadIndex succeeded, want an error", n.GetNodeID())
			}
			break
		}
	}
}
