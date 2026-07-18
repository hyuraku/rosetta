package integration

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"rosetta/raft"
)

// fakeSnapshotter is a minimal in-memory raft.Snapshotter used to drive
// integration tests without standing up a real state machine. It records
// whatever bytes are passed via InstallSnapshot so the test can assert
// that a follower received and processed an InstallSnapshot RPC.
type fakeSnapshotter struct {
	mu                sync.Mutex
	data              []byte
	lastIncludedIndex int
	lastIncludedTerm  int
}

func (f *fakeSnapshotter) CreateSnapshot(idx, term int) ([]byte, error) {
	// Not exercised in this test (kvstore drives CreateSnapshot in production).
	return f.snapshot(), nil
}

func (f *fakeSnapshotter) InstallSnapshot(data []byte, idx, term int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data = append(f.data[:0], data...)
	f.lastIncludedIndex = idx
	f.lastIncludedTerm = term
	return nil
}

func (f *fakeSnapshotter) ReadSnapshot() ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.data == nil {
		return nil, nil
	}
	out := make([]byte, len(f.data))
	copy(out, f.data)
	return out, nil
}

func (f *fakeSnapshotter) snapshot() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.data == nil {
		return nil
	}
	out := make([]byte, len(f.data))
	copy(out, f.data)
	return out
}

// seed lets the test pre-populate the leader's snapshot bytes as if its
// state machine had just persisted a snapshot at the given index/term.
func (f *fakeSnapshotter) seed(data []byte, idx, term int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data = append([]byte(nil), data...)
	f.lastIncludedIndex = idx
	f.lastIncludedTerm = term
}

// TestInstallSnapshotCatchUp drives a 3-node cluster through:
//  1. start a 2-node majority (node1, node2). node3 exists in peers but
//     is not connected to the transport — it is a "late joiner".
//  2. submit many commands so the leader builds up a log.
//  3. compact the leader's log past where node3 would need to start.
//  4. spin up node3 and attach it to the transport.
//
// and verifies that node3 catches up via InstallSnapshot, not AppendEntries.
//
// Why we do not use partition+heal: removing a node from MockTransport only
// blocks deliveries to that node — it keeps running its own election timer
// and emits RequestVotes that disrupt the live majority. The "late joiner"
// pattern avoids that and is a more realistic test of InstallSnapshot.
func TestInstallSnapshotCatchUp(t *testing.T) {
	peers := []string{"node1", "node2", "node3"}
	const laggerID = "node3"

	transport := raft.NewMockTransport()

	// 1) Bring up only the two-node majority. node3 is intentionally absent
	//    from both nodes and transport so the leader sees its RPCs to node3
	//    time out (DeadlineExceeded), letting the cluster operate on
	//    node1+node2 alone.
	nodes, snapshotters := startMajority(peers, laggerID, transport)
	defer func() {
		for _, n := range nodes {
			n.Kill()
		}
	}()

	// 2) Wait for a leader to emerge among node1/node2.
	leader, leaderID := waitForLeader(t, nodes)
	t.Logf("leader=%s (lagger %s not yet attached)", leaderID, laggerID)

	// 3) Submit enough commands that the leader's log grows.
	const numCmds = 30
	for i := 0; i < numCmds; i++ {
		if _, _, ok := leader.Start(fmt.Sprintf("cmd-%d", i)); !ok {
			t.Fatalf("leader rejected Start at cmd %d", i)
		}
	}
	time.Sleep(300 * time.Millisecond)

	preLen := leader.GetLogLength()
	if preLen < numCmds {
		t.Fatalf("expected leader log >= %d, got %d", numCmds, preLen)
	}

	// 4) Compact the leader's log up to the next-to-last few entries.
	//    In production kvstore would persist a snapshot first; here we seed
	//    the snapshotter directly, then call TriggerSnapshot to truncate.
	compactUpTo := numCmds - 5 // keep last 5 entries in the live log
	snapshotters[leaderID].seed(
		[]byte(fmt.Sprintf("kv-snapshot-up-to-%d", compactUpTo)),
		compactUpTo,
		1, // term will be 1 in a quiet cluster; not strictly checked
	)
	leader.TriggerSnapshot(compactUpTo)
	time.Sleep(200 * time.Millisecond) // wait for async TruncateLogTo

	postLen := leader.GetLogLength()
	if postLen >= preLen {
		t.Fatalf("leader log did not shrink after TriggerSnapshot: pre=%d post=%d", preLen, postLen)
	}
	t.Logf("leader log compacted: %d -> %d entries (snapshot at index %d)",
		preLen, postLen, compactUpTo)

	// 5) Late-join the lagger. From its first heartbeat the leader will
	//    notice node3's nextIndex falls under LastIncludedIndex and must
	//    send InstallSnapshot instead of AppendEntries.
	laggerApplyCh := make(chan raft.ApplyMsg, 256)
	go func() {
		for range laggerApplyCh {
		}
	}()
	laggerNode := raft.NewRaftNode(laggerID, peers, transport, laggerApplyCh)
	laggerSnap := &fakeSnapshotter{}
	laggerNode.SetSnapshotter(laggerSnap)
	nodes[laggerID] = laggerNode
	snapshotters[laggerID] = laggerSnap
	transport.RegisterNode(laggerID, laggerNode)

	// Verify the lagger caught up via InstallSnapshot by polling its
	// snapshot metadata. We assert on LastIncludedIndex (option b) rather
	// than on raw snapshot bytes (a) because the metadata advance proves
	// the follower's raft state — not just the state machine — accepted
	// the snapshot and rewrote its log boundary.
	if !waitForSnapshotCatchUp(nodes, laggerID, compactUpTo) {
		t.Errorf("lagger %s did not catch up via InstallSnapshot within deadline", laggerID)
	}
}

// startMajority brings up every peer except laggerID as a live RaftNode wired
// to transport, each with its own drained applyCh and fakeSnapshotter.
func startMajority(
	peers []string, laggerID string, transport *raft.MockTransport,
) (nodes map[string]*raft.RaftNode, snapshotters map[string]*fakeSnapshotter) {
	nodes = make(map[string]*raft.RaftNode)
	snapshotters = make(map[string]*fakeSnapshotter)
	for _, id := range peers {
		if id == laggerID {
			continue
		}
		applyCh := make(chan raft.ApplyMsg, 256)
		// Drain applyCh so it never blocks the raft state machine.
		go func() {
			for range applyCh {
			}
		}()

		node := raft.NewRaftNode(id, peers, transport, applyCh)
		sn := &fakeSnapshotter{}
		node.SetSnapshotter(sn)
		nodes[id] = node
		snapshotters[id] = sn
		transport.RegisterNode(id, node)
	}
	return nodes, snapshotters
}

// waitForLeader polls the given nodes until one reports leadership, failing the
// test if none is elected within the budget.
func waitForLeader(t *testing.T, nodes map[string]*raft.RaftNode) (leader *raft.RaftNode, leaderID string) {
	t.Helper()
	for attempt := 0; attempt < 20; attempt++ {
		time.Sleep(100 * time.Millisecond)
		for id, n := range nodes {
			if n.IsLeader() {
				return n, id
			}
		}
	}
	t.Fatal("no leader elected within budget")
	return nil, ""
}

// waitForSnapshotCatchUp polls the lagger's snapshot metadata until its
// LastIncludedIndex reaches compactUpTo, returning false on timeout.
func waitForSnapshotCatchUp(nodes map[string]*raft.RaftNode, laggerID string, compactUpTo int) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		idx, _ := nodes[laggerID].GetRaftState().GetSnapshotMetadata()
		if idx >= compactUpTo {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}
