package integration

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"rosetta/kvstore"
	"rosetta/persistence"
	"rosetta/raft"
)

// kvNode bundles the full production stack for one node: real file storage, a
// KV snapshotter, the production raft.Snapshotter, a KVStore, and a RaftNode,
// wired exactly as main.go wires them (SetRaft + SetSnapshotter).
type kvNode struct {
	id      string
	dir     string
	storage persistence.Storage
	kvSnap  *persistence.KVSnapshotter
	kvs     *kvstore.KVStore
	raft    *raft.RaftNode
}

func newKVNode(t *testing.T, id string, peers []string, baseDir string,
	transport *raft.MockTransport, maxRaftState int) *kvNode {
	t.Helper()

	dir := filepath.Join(baseDir, id)
	storage, err := persistence.NewFileStorage(dir)
	if err != nil {
		t.Fatalf("create storage for %s: %v", id, err)
	}
	persister := persistence.NewRaftPersister(storage)
	kvSnap := persistence.NewKVSnapshotter(storage)
	raftSnap := persistence.NewRaftSnapshotter(storage)

	kvs := kvstore.NewKVStoreWithSnapshotter(maxRaftState, kvSnap)
	applyCh := kvs.GetApplyCh()

	rn, err := raft.NewRaftNodeWithPersister(id, peers, transport, applyCh, persister)
	if err != nil {
		t.Fatalf("create raft node %s: %v", id, err)
	}
	kvs.SetRaft(rn)
	rn.SetSnapshotter(raftSnap) // production wiring: leader can send InstallSnapshot
	transport.RegisterNode(id, rn)

	return &kvNode{id: id, dir: dir, storage: storage, kvSnap: kvSnap, kvs: kvs, raft: rn}
}

// TestKVStoreInstallSnapshotProductionWiring exercises the real compaction path
// end to end (defect 4, and defects 1-3 in an integrated setting):
//
//  1. Bring up a 2-node majority (node1, node2) each with the full persistence
//     stack and SetSnapshotter, plus a small maxRaftState so writes trigger
//     compaction.
//  2. Drive writes through the leader's KVStore until its log is compacted.
//  3. Late-join node3; the leader must serve InstallSnapshot using its real
//     RaftSnapshotter (reading the V2 bytes the kvstore persisted to disk).
//  4. Verify node3's KVStore recovered the data AND persisted the snapshot to
//     its own disk, and that a restart (fresh KVStore over the same storage)
//     restores both the KV data and the duplicate-detection session.
//
// The pre-existing TestInstallSnapshotCatchUp covers the same raft mechanics
// with a fakeSnapshotter; this test drives the real persistence + kvstore stack
// so the fakeSnapshotter-only path is finally exercised with production wiring.
func TestKVStoreInstallSnapshotProductionWiring(t *testing.T) {
	peers := []string{"node1", "node2", "node3"}
	const laggerID = "node3"
	const maxRaftState = 10
	baseDir := t.TempDir()

	transport := raft.NewMockTransport()

	nodes := make(map[string]*kvNode)
	for _, id := range peers {
		if id == laggerID {
			continue
		}
		nodes[id] = newKVNode(t, id, peers, baseDir, transport, maxRaftState)
	}
	defer func() {
		for _, n := range nodes {
			n.raft.Kill()
		}
	}()

	// Wait for a leader among node1/node2.
	leader := waitForKVLeader(t, nodes)

	// Drive writes through the leader's KVStore. A single client with an
	// advancing seqNum also seeds a duplicate-detection session that must be
	// carried in the snapshot.
	const numKeys = 25
	const clientID = "client-42"
	for i := 0; i < numKeys; i++ {
		key := fmt.Sprintf("key-%d", i)
		if err := putThroughLeader(t, nodes, &leader, key, fmt.Sprintf("val-%d", i), clientID, i+1); err != nil {
			t.Fatalf("put %s failed: %v", key, err)
		}
	}

	// The leader must have compacted its log (snapshot boundary advanced).
	boundary, _ := leader.raft.GetRaftState().GetSnapshotMetadata()
	if boundary <= 0 {
		t.Fatalf("leader did not compact: snapshot boundary=%d", boundary)
	}
	t.Logf("leader %s compacted to snapshot boundary %d", leader.id, boundary)

	// Confirm the leader actually persisted V2 snapshot bytes its RaftSnapshotter
	// can ship (this is what a lagging follower will receive).
	raftSnap := persistence.NewRaftSnapshotter(leader.storage)
	shipped, err := raftSnap.ReadSnapshot()
	if err != nil || shipped == nil {
		t.Fatalf("leader RaftSnapshotter.ReadSnapshot returned (%v, %v); expected bytes", shipped, err)
	}

	// Late-join the lagger with the full production stack.
	lagger := newKVNode(t, laggerID, peers, baseDir, transport, maxRaftState)
	nodes[laggerID] = lagger

	// The lagger must catch up via InstallSnapshot: its raft snapshot boundary
	// advances to the leader's boundary.
	if !waitForRaftSnapshot(lagger.raft, boundary) {
		idx, _ := lagger.raft.GetRaftState().GetSnapshotMetadata()
		t.Fatalf("lagger %s did not install snapshot: boundary=%d, want>=%d", laggerID, idx, boundary)
	}

	// The lagger's KVStore must contain the data (from snapshot + any tail).
	if !waitForKVKey(lagger.kvs, "key-0", "val-0") {
		t.Fatalf("lagger %s KVStore missing key-0 after snapshot install", laggerID)
	}

	// Defect 1: the lagger must have persisted the installed snapshot to disk.
	snap, err := lagger.storage.LoadSnapshot()
	if err != nil || snap == nil {
		t.Fatalf("lagger %s did not persist snapshot to disk: snap=%v err=%v", laggerID, snap, err)
	}
	if snap.LastIncludedIndex != boundary {
		t.Fatalf("lagger persisted snapshot boundary=%d, want %d", snap.LastIncludedIndex, boundary)
	}

	// Defect 1 + 2: simulate a lagger restart and verify recovery from disk.
	verifyLaggerRestart(t, lagger.dir, maxRaftState, clientID)
}

// verifyLaggerRestart opens a fresh KVStore over the lagger's on-disk storage
// (a simulated restart) and asserts that both the KV data and the
// duplicate-detection session shipped inside the V2 snapshot were recovered.
func verifyLaggerRestart(t *testing.T, dir string, maxRaftState int, clientID string) {
	t.Helper()

	storage, err := persistence.NewFileStorage(dir)
	if err != nil {
		t.Fatalf("reopen lagger storage: %v", err)
	}
	kvSnap := persistence.NewKVSnapshotter(storage)
	recovered := kvstore.NewKVStoreWithSnapshotter(maxRaftState, kvSnap)
	defer recovered.Close()

	if got := recovered.GetSnapshot()["key-0"]; got != "val-0" {
		t.Fatalf("restarted lagger lost KV data: key-0=%q, want %q", got, "val-0")
	}

	sd, _, _, err := kvSnap.LoadSnapshotV2()
	if err != nil {
		t.Fatalf("load restarted snapshot: %v", err)
	}
	if sd == nil || sd.Sessions[clientID] == nil {
		t.Fatalf("restarted lagger lost duplicate-detection session for %s", clientID)
	}
	t.Logf("restarted lagger restored %d keys and session for %s (lastSeq=%d)",
		len(sd.KVData), clientID, sd.Sessions[clientID].LastSeqNum)
}

// putThroughLeader writes via whichever node currently holds leadership,
// refreshing the leader pointer if the write is rejected due to a leadership
// change.
func putThroughLeader(t *testing.T, nodes map[string]*kvNode, leader **kvNode,
	key, value, clientID string, seqNum int) error {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if (*leader).raft.IsLeader() {
			if err := (*leader).kvs.PutWithSession(key, value, clientID, seqNum); err == nil {
				return nil
			} else {
				lastErr = err
			}
		}
		if l := findKVLeader(nodes); l != nil {
			*leader = l
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("no leader accepted write within budget: %w", lastErr)
}

func findKVLeader(nodes map[string]*kvNode) *kvNode {
	for _, n := range nodes {
		if n.raft.IsLeader() {
			return n
		}
	}
	return nil
}

func waitForKVLeader(t *testing.T, nodes map[string]*kvNode) *kvNode {
	t.Helper()
	for attempt := 0; attempt < 30; attempt++ {
		time.Sleep(100 * time.Millisecond)
		if l := findKVLeader(nodes); l != nil {
			return l
		}
	}
	t.Fatal("no leader elected within budget")
	return nil
}

func waitForRaftSnapshot(node *raft.RaftNode, wantBoundary int) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if idx, _ := node.GetRaftState().GetSnapshotMetadata(); idx >= wantBoundary {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func waitForKVKey(kvs *kvstore.KVStore, key, want string) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if kvs.GetSnapshot()[key] == want {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}
