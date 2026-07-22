package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	"rosetta/kvstore"
	"rosetta/raft"
)

// linkRegistry is a directed, partitionable in-memory network shared by a set of
// per-node transports. A node marked isolated has every link to and from it cut
// in both directions, which — unlike raft.MockTransport.RemoveNode (which only
// blocks deliveries TO a node) — lets a test sever a leader's OUTGOING RPCs and
// so genuinely strip it of its quorum.
type linkRegistry struct {
	mu       sync.RWMutex
	nodes    map[string]*raft.RaftNode
	isolated map[string]bool
}

func newLinkRegistry() *linkRegistry {
	return &linkRegistry{
		nodes:    make(map[string]*raft.RaftNode),
		isolated: make(map[string]bool),
	}
}

func (r *linkRegistry) register(id string, n *raft.RaftNode) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nodes[id] = n
}

func (r *linkRegistry) isolate(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.isolated[id] = true
}

func (r *linkRegistry) blocked(from, to string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.isolated[from] || r.isolated[to]
}

func (r *linkRegistry) node(id string) *raft.RaftNode {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.nodes[id]
}

// partTransport is one node's view of the shared linkRegistry. It knows its own
// source ID, so it can honor directional partitions.
type partTransport struct {
	source string
	reg    *linkRegistry
}

func (p *partTransport) SendRequestVote(
	_ context.Context, target string, args *raft.RequestVoteArgs,
) (*raft.RequestVoteReply, error) {
	if p.reg.blocked(p.source, target) {
		return nil, context.DeadlineExceeded
	}
	n := p.reg.node(target)
	if n == nil {
		return nil, context.DeadlineExceeded
	}
	reply := &raft.RequestVoteReply{}
	err := n.RequestVote(args, reply)
	return reply, err
}

func (p *partTransport) SendAppendEntries(
	_ context.Context, target string, args *raft.AppendEntriesArgs,
) (*raft.AppendEntriesReply, error) {
	if p.reg.blocked(p.source, target) {
		return nil, context.DeadlineExceeded
	}
	n := p.reg.node(target)
	if n == nil {
		return nil, context.DeadlineExceeded
	}
	reply := &raft.AppendEntriesReply{}
	err := n.AppendEntries(args, reply)
	return reply, err
}

// SendInstallSnapshot is never exercised here (no compaction), so it simply
// reports the link as unavailable.
func (p *partTransport) SendInstallSnapshot(
	_ context.Context, _ string, _ *raft.InstallSnapshotArgs,
) (*raft.InstallSnapshotReply, error) {
	return nil, context.DeadlineExceeded
}

// TestReadIndexRefusesStaleReadWhenLeaderPartitioned is the key safety
// regression: a leader that has been cut off from a majority must NOT keep
// serving reads from its local state. Under the removed lease optimization the
// ex-leader would return the stale value for a full election-timeout window
// (and indefinitely, since lastLeaderConfirmation was bumped by any single
// follower reply). With ReadIndex it cannot confirm a quorum, so the read fails.
func TestReadIndexRefusesStaleReadWhenLeaderPartitioned(t *testing.T) {
	peers := []string{"n1", "n2", "n3"}
	reg := newLinkRegistry()
	stores := make(map[string]*kvstore.KVStore)
	nodes := make(map[string]*raft.RaftNode)

	for _, id := range peers {
		kvs := kvstore.NewKVStore(1000)
		node := raft.NewRaftNode(id, peers, &partTransport{source: id, reg: reg}, kvs.GetApplyCh())
		kvs.SetRaft(node)
		reg.register(id, node)
		stores[id] = kvs
		nodes[id] = node
	}
	// Only kill the raft nodes on teardown. We deliberately do NOT Close() the
	// stores: Close closes each apply channel, which would race with in-flight
	// AppendEntries handlers on peer nodes still pushing applied entries. The
	// leaked applyLoop goroutines are harmless for the lifetime of the test.
	defer func() {
		for _, n := range nodes {
			n.Kill()
		}
	}()

	// Wait for a stable leader.
	var leaderID string
	if !waitFor(3*time.Second, func() bool {
		leaderID = ""
		count := 0
		for id, n := range nodes {
			if n.IsLeader() {
				leaderID = id
				count++
			}
		}
		return count == 1
	}) {
		t.Fatal("no single leader elected")
	}

	// Write a value and confirm it reads back while the cluster is healthy.
	if err := stores[leaderID].Put("k", "v1"); err != nil {
		t.Fatalf("Put on leader %s failed: %v", leaderID, err)
	}
	if got, err := stores[leaderID].Get("k"); err != nil || got != "v1" {
		t.Fatalf("healthy Get = %q, err=%v; want v1, nil", got, err)
	}

	// Cut the leader off from the other two nodes (it is now a minority of 1/3).
	reg.isolate(leaderID)

	// The isolated ex-leader must refuse the read rather than return "v1".
	if !waitFor(2*time.Second, func() bool {
		_, err := stores[leaderID].Get("k")
		return err != nil
	}) {
		t.Fatal("partitioned ex-leader kept serving reads; ReadIndex failed to refuse (stale-read hazard)")
	}

	// And it must specifically not have returned the stale value.
	if got, err := stores[leaderID].Get("k"); err == nil {
		t.Fatalf("partitioned ex-leader returned a value %q for a read it could not linearize", got)
	}
}

// waitFor polls cond until it is true or d elapses.
func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}
