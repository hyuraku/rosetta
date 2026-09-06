package integration

import (
	"errors"
	"sync"
	"testing"
	"time"

	"rosetta/raft"
)

// errInjectedPersist is the storage failure injectablePersister reports.
var errInjectedPersist = errors.New("injected persist failure")

const (
	nodeA = "node1"
	nodeB = "node2"
	nodeC = "node3"

	// durableCommand is the entry whose commit must wait for a durable ACK.
	durableCommand = "durable-command"
	// alwaysFail makes injectablePersister fail every save until it is reset.
	alwaysFail = -1
)

// injectablePersister is an in-memory raft.Persister that can be told to fail
// its saves, and that hands back the last successfully saved state on load — so
// a test can rebuild a node from exactly what survived a crash.
type injectablePersister struct {
	mu       sync.Mutex
	saved    raft.PersistentState
	failures int
}

func (p *injectablePersister) setFailures(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failures = n
}

func (p *injectablePersister) SaveRaftState(state *raft.PersistentState) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.failures != 0 {
		if p.failures > 0 {
			p.failures--
		}
		return errInjectedPersist
	}
	p.saved = copyState(state)
	return nil
}

func (p *injectablePersister) LoadRaftState() (*raft.PersistentState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	loaded := copyState(&p.saved)
	return &loaded, nil
}

func (p *injectablePersister) savedLog() []raft.LogEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	return copyState(&p.saved).Log
}

func copyState(state *raft.PersistentState) raft.PersistentState {
	clone := *state
	clone.Log = append(make([]raft.LogEntry, 0, len(state.Log)), state.Log...)
	if state.VotedFor != nil {
		votedFor := *state.VotedFor
		clone.VotedFor = &votedFor
	}
	return clone
}

// startNode brings up one raft node over the shared mock transport, registers it
// and drains its apply channel.
func startNode(t *testing.T, transport *raft.MockTransport, id string, peers []string, p raft.Persister) *raft.RaftNode {
	t.Helper()

	applyCh := make(chan raft.ApplyMsg, 64)
	go func() {
		for range applyCh { //nolint:revive // draining
		}
	}()

	node, err := raft.NewRaftNodeWithPersister(id, peers, transport, applyCh, p)
	if err != nil {
		t.Fatalf("start %s: %v", id, err)
	}
	transport.RegisterNode(id, node)
	return node
}

func logHolds(node *raft.RaftNode, index int, command interface{}) bool {
	entry := node.GetRaftState().GetLogEntry(index)
	return entry != nil && entry.Command == command
}

// leaderAmong waits until one of the given nodes reports itself leader and
// returns its id.
func leaderAmong(t *testing.T, nodes map[string]*raft.RaftNode, d time.Duration) string {
	t.Helper()

	var found string
	if !waitFor(d, func() bool {
		for id, node := range nodes {
			if node.IsLeader() {
				found = id
				return true
			}
		}
		return false
	}) {
		t.Fatal("no leader was elected")
	}
	return found
}

// requireCommitBelow fails as soon as the leader's commit index reaches index,
// and keeps watching for the whole of d.
func requireCommitBelow(t *testing.T, leader *raft.RaftNode, index int, d time.Duration) {
	t.Helper()

	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if got := leader.GetRaftState().GetCommitIndex(); got >= index {
			t.Fatalf("leader committed index %d on an ACK from a follower that never persisted it (commit=%d)",
				index, got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// R2, cluster level: a leader must not commit on the strength of an ACK from a
// follower that could not write the entries to stable storage, and once that
// follower does ACK, the entry must survive the follower's crash and the
// election that follows.
//
// The cluster is three peers but only two are alive, so the one reachable
// follower's ACK is the deciding vote for every commit. With its storage broken
// the follower keeps being sent the same entry; before the fix its second reply
// was Success=true (the un-persisted merge looked like a duplicate), the leader
// committed, and a crash of that follower would have lost a committed entry.
func TestCommitWaitsForDurableFollowerAck(t *testing.T) {
	transport := raft.NewMockTransport()
	peers := []string{nodeA, nodeB, nodeC}

	persisters := map[string]*injectablePersister{
		nodeA: {},
		nodeB: {},
	}
	nodes := map[string]*raft.RaftNode{
		nodeA: startNode(t, transport, nodeA, peers, persisters[nodeA]),
		nodeB: startNode(t, transport, nodeB, peers, persisters[nodeB]),
	}

	leaderID := leaderAmong(t, nodes, 5*time.Second)
	followerID := nodeA
	if leaderID == nodeA {
		followerID = nodeB
	}
	leader, follower := nodes[leaderID], nodes[followerID]
	// Both are killed explicitly further down; the deferred calls only cover an
	// early t.Fatal, so they must be idempotent.
	killLeader := sync.OnceFunc(leader.Kill)
	killFollower := sync.OnceFunc(follower.Kill)
	defer killLeader()
	defer killFollower()

	// The election no-op has to commit first, which proves the follower's ACKs
	// are what carries the quorum (the third peer is absent).
	if !waitFor(5*time.Second, func() bool { return leader.GetRaftState().GetCommitIndex() >= 1 }) {
		t.Fatalf("leader %s never committed its election no-op", leaderID)
	}

	// Break the only follower's storage, then start a command.
	persisters[followerID].setFailures(alwaysFail)
	index, _, isLeader, err := leader.Start(durableCommand)
	if !isLeader || err != nil {
		t.Fatalf("Start on leader %s = (isLeader %v, err %v)", leaderID, isLeader, err)
	}

	// The follower cannot make the entry durable, so it must never ACK it and
	// the commit index must stay below it, however many times it is resent.
	requireCommitBelow(t, leader, index, time.Second)

	// Storage recovers: the same resends now persist and the entry commits.
	persisters[followerID].setFailures(0)
	if !waitFor(5*time.Second, func() bool { return leader.GetRaftState().GetCommitIndex() >= index }) {
		t.Fatalf("leader %s did not commit index %d after the follower's storage recovered", leaderID, index)
	}
	saved := persisters[followerID].savedLog()
	if len(saved) < index || saved[index-1].Command != durableCommand {
		t.Fatalf("follower %s persisted %+v, want %q at index %d", followerID, saved, durableCommand, index)
	}

	// Crash the follower and rebuild it from what actually reached its storage.
	killFollower()
	transport.RemoveNode(followerID)
	restarted := startNode(t, transport, followerID, peers, persisters[followerID])
	defer restarted.Kill()
	if !logHolds(restarted, index, durableCommand) {
		t.Fatalf("restarted %s lost the committed entry at index %d", followerID, index)
	}

	// Now lose the leader too and let the survivors elect a new one. The third
	// peer comes up empty, so the Election Restriction (§5.4.1) must hand
	// leadership to the node that holds the committed entry.
	killLeader()
	transport.RemoveNode(leaderID)
	fresh := startNode(t, transport, nodeC, peers, &injectablePersister{})
	defer fresh.Kill()

	survivors := map[string]*raft.RaftNode{followerID: restarted, nodeC: fresh}
	newLeaderID := leaderAmong(t, survivors, 10*time.Second)
	if !logHolds(survivors[newLeaderID], index, durableCommand) {
		t.Fatalf("new leader %s does not hold the committed entry at index %d", newLeaderID, index)
	}
}
