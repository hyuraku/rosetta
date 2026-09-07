package raft

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// Cluster-level tests for a configuration change end to end (KNOWN_ISSUES.md
// R14, paper §6): proposing one, both phases of the transition, and what the
// quorum becomes afterwards.
//
// Everything runs on MockTransport and in-memory persisters, so nothing depends
// on network timing; the only waiting is for the node's own 50ms tick to carry
// entries between nodes, which the poll helpers bound.

// testCluster is a set of RaftNodes wired to one MockTransport, each with its
// own in-memory persister and a drained apply channel.
type testCluster struct {
	t          *testing.T
	transport  *MockTransport
	nodes      map[string]*RaftNode
	persisters map[string]*memPersister
	stopDrain  chan struct{}
	drainWG    sync.WaitGroup
}

func newTestCluster(t *testing.T) *testCluster {
	t.Helper()
	return &testCluster{
		t:          t,
		transport:  NewMockTransport(),
		nodes:      make(map[string]*RaftNode),
		persisters: make(map[string]*memPersister),
		stopDrain:  make(chan struct{}),
	}
}

// start brings up one node whose initial configuration is peers. Passing a peers
// list that does not contain id is how a server destined to be *added* to a
// running cluster is started: it holds the existing cluster's configuration, is
// not a voter in it, and therefore never campaigns until a configuration entry
// admits it.
func (c *testCluster) start(id string, peers []string) *RaftNode {
	c.t.Helper()

	persister, existing := c.persisters[id]
	if !existing {
		persister = &memPersister{}
		c.persisters[id] = persister
	}

	applyCh := make(chan ApplyMsg, 256)
	node, err := NewRaftNodeWithPersister(id, peers, c.transport, applyCh, persister)
	if err != nil {
		c.t.Fatalf("start %s: %v", id, err)
	}
	c.nodes[id] = node
	c.transport.RegisterNode(id, node)

	// Drain the apply channel so the applier is never the thing that stalls.
	c.drainWG.Add(1)
	go func() {
		defer c.drainWG.Done()
		for {
			select {
			case <-c.stopDrain:
				return
			case <-applyCh:
			}
		}
	}()
	return node
}

func (c *testCluster) stopAll() {
	for _, node := range c.nodes {
		node.Kill()
	}
	close(c.stopDrain)
	c.drainWG.Wait()
}

// kill removes a node from the cluster entirely: it stops the node and takes it
// off the transport, so its peers see it as unreachable.
func (c *testCluster) kill(id string) {
	c.t.Helper()
	node, ok := c.nodes[id]
	if !ok {
		return
	}
	c.transport.RemoveNode(id)
	node.Kill()
	delete(c.nodes, id)
}

// waitLeader returns the single leader once exactly one node claims the role.
func (c *testCluster) waitLeader() *RaftNode {
	c.t.Helper()
	var leader *RaftNode
	c.waitFor("a single leader", func() bool {
		leader = c.currentLeader()
		return leader != nil
	})
	return leader
}

func (c *testCluster) waitFor(what string, cond func() bool) {
	c.t.Helper()
	deadline := time.Now().Add(configTestTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.t.Fatalf("timed out waiting for %s", what)
}

// waitConfig waits until every live node holds a non-joint configuration with
// exactly the given voters.
func (c *testCluster) waitConfig(voters ...string) {
	c.t.Helper()
	c.waitFor(fmt.Sprintf("every node to settle on the configuration %v", voters), func() bool {
		for _, node := range c.nodes {
			config := node.GetClusterConfig()
			if config.IsJoint() || len(config.Voters) != len(voters) {
				return false
			}
			for _, id := range voters {
				if !config.IsVoter(id) {
					return false
				}
			}
		}
		return true
	})
}

// commitOne appends one command through whichever node is currently the leader
// and waits for it to commit, which is the end-to-end proof that the current
// configuration can still form a quorum.
//
// The leader is re-resolved on every attempt rather than taken as an argument:
// under load an election can land between "this node is the leader" and the
// Start that follows, and a test that asserts a quorum still exists must not
// fail because leadership moved while it was asking.
func (c *testCluster) commitOne(command string) {
	c.t.Helper()

	var (
		committed bool
		lastErr   error
	)
	c.waitFor(fmt.Sprintf("the command %q to commit", command), func() bool {
		leader := c.currentLeader()
		if leader == nil {
			return false
		}
		index, _, isLeader, err := leader.Start(command)
		if !isLeader || err != nil {
			lastErr = fmt.Errorf("Start: isLeader=%v err=%w", isLeader, err)
			return false
		}
		// Give this attempt its own bounded wait: if leadership moves before the
		// entry commits, the next attempt starts over on the new leader.
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if leader.GetRaftState().GetCommitIndex() >= index {
				committed = true
				return true
			}
			time.Sleep(5 * time.Millisecond)
		}
		lastErr = fmt.Errorf("index %d did not commit before leadership was re-checked", index)
		return false
	})
	if !committed {
		c.t.Fatalf("committing %q: %v", command, lastErr)
	}
}

// currentLeader returns the single leader, or nil when there is not exactly one.
func (c *testCluster) currentLeader() *RaftNode {
	var leader *RaftNode
	count := 0
	for _, node := range c.nodes {
		if node.IsLeader() {
			count++
			leader = node
		}
	}
	if count != 1 {
		return nil
	}
	return leader
}

// proposeWhenReady retries a configuration change until the leader is ready to
// take one. A freshly elected leader refuses until its own no-op has committed
// (§5.4.2), which is a matter of one heartbeat round.
func (c *testCluster) proposeWhenReady(leader *RaftNode, add bool, nodeID, addr string) {
	c.t.Helper()
	var lastErr error
	c.waitFor(fmt.Sprintf("the leader to accept a membership change for %s", nodeID), func() bool {
		_, lastErr = leader.ProposeConfigChange(add, nodeID, addr)
		return lastErr == nil
	})
	if lastErr != nil {
		c.t.Fatalf("ProposeConfigChange(%v, %s): %v", add, nodeID, lastErr)
	}
}

// TestAddNodeGrowsTheQuorum is the 3 -> 4 case. It asserts more than "the
// configuration changed": once C_new is committed, the fourth server's
// acknowledgement is *required*, which is checked by taking one of the original
// three away and showing that a command still commits — which needs three of the
// four, and so cannot happen without the newcomer.
func TestAddNodeGrowsTheQuorum(t *testing.T) {
	cluster := newTestCluster(t)
	defer cluster.stopAll()

	original := []string{"n1", "n2", "n3"}
	for _, id := range original {
		cluster.start(id, original)
	}
	leader := cluster.waitLeader()

	// The newcomer starts with the *existing* cluster's configuration, so it is
	// not a voter and will not campaign until it is admitted.
	newcomer := cluster.start("n4", original)
	if newcomer.GetClusterConfig().IsVoter("n4") {
		t.Fatal("a server started with the existing cluster's peer list must not be a voter yet")
	}

	cluster.proposeWhenReady(leader, true, "n4", addrN4)
	cluster.waitConfig("n1", "n2", "n3", "n4")

	// Now take away one of the original three.
	victim := original[len(original)-1]
	if leader.GetNodeID() == victim {
		victim = original[len(original)-2]
	}
	cluster.kill(victim)

	cluster.commitOne("after-growth")

	// Committing at all already proves it: three of the four servers have to
	// agree and only three are alive. Asserting the leader's view of the
	// newcomer's progress as well makes the failure legible if that ever stops
	// being true.
	cluster.waitFor("the newcomer's MatchIndex to advance on the leader", func() bool {
		leader := cluster.currentLeader()
		if leader == nil {
			return false
		}
		state := leader.GetRaftState()
		state.mu.RLock()
		defer state.mu.RUnlock()
		return state.leader != nil && state.leader.MatchIndex["n4"] > 0
	})
}

// TestRemoveNodesShrinksTheQuorum is the 5 -> 3 case, removing two servers that
// are not the leader. The removed servers are killed only *after* the change
// completes, so the test also covers a cluster that keeps working while servers
// it no longer counts are still running.
func TestRemoveNodesShrinksTheQuorum(t *testing.T) {
	cluster := newTestCluster(t)
	defer cluster.stopAll()

	all := []string{"n1", "n2", "n3", "n4", "n5"}
	for _, id := range all {
		cluster.start(id, all)
	}
	leader := cluster.waitLeader()

	remaining := []string{}
	removed := []string{}
	for _, id := range all {
		switch {
		case id == leader.GetNodeID() || len(remaining) < 2:
			remaining = append(remaining, id)
		case len(removed) < 2:
			removed = append(removed, id)
		default:
			remaining = append(remaining, id)
		}
	}

	for _, id := range removed {
		cluster.proposeWhenReady(leader, false, id, "")
		cluster.waitFor("the change removing "+id+" to complete", func() bool {
			config := leader.GetClusterConfig()
			return !config.IsJoint() && !config.IsVoter(id)
		})
	}

	cluster.waitFor("the survivors to settle on the three-server configuration", func() bool {
		for _, id := range remaining {
			config := cluster.nodes[id].GetClusterConfig()
			if config.IsJoint() || len(config.Voters) != len(remaining) {
				return false
			}
		}
		return true
	})

	// With three voters left, two of them are a majority: the cluster keeps
	// committing after both removed servers are gone for good.
	for _, id := range removed {
		cluster.kill(id)
	}
	cluster.commitOne("after-shrink")
}

// TestRemovingTheLeaderStepsItDownAfterCNewCommits covers §6's rule for a leader
// that is not in C_new: it keeps leading until C_new commits — it is the only
// server that can get it committed — and steps down immediately after. The
// remaining servers must then elect a leader among themselves.
func TestRemovingTheLeaderStepsItDownAfterCNewCommits(t *testing.T) {
	cluster := newTestCluster(t)
	defer cluster.stopAll()

	all := []string{"n1", "n2", "n3"}
	for _, id := range all {
		cluster.start(id, all)
	}
	leader := cluster.waitLeader()
	leaderID := leader.GetNodeID()

	cluster.proposeWhenReady(leader, false, leaderID, "")

	cluster.waitFor("the removed leader to step down", func() bool {
		return !leader.IsLeader()
	})
	if config := leader.GetClusterConfig(); config.IsVoter(leaderID) {
		t.Errorf("the removed leader still considers itself a voter: %+v", config.Voters)
	}

	// The remaining two form the new cluster and must elect one of themselves.
	cluster.kill(leaderID)
	newLeader := cluster.waitLeader()
	if newLeader.GetNodeID() == leaderID {
		t.Fatal("the removed leader was elected again")
	}
	cluster.waitConfig(withoutID(all, leaderID)...)
	cluster.commitOne("after-leader-removal")
}

func withoutID(ids []string, drop string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != drop {
			out = append(out, id)
		}
	}
	return out
}

// TestSecondConfigChangeRejected covers §6's "one at a time" rule. The single
// voter here commits on its own, so the configuration admitting n2 as a learner
// is committed almost at once — but n2 does not exist and never catches up, so
// the *addition* is still in flight and any further change has to be refused.
// Removing that learner is the deliberate exception and has its own test
// (TestSecondChangeDuringCatchUpRejected).
func TestSecondConfigChangeRejected(t *testing.T) {
	cluster := newTestCluster(t)
	defer cluster.stopAll()

	node := cluster.start("n1", []string{"n1"})
	cluster.waitLeader()

	cluster.proposeWhenReady(node, true, "n2", "addr-n2")

	if _, err := node.ProposeConfigChange(true, "n3", "addr-n3"); !errors.Is(err, ErrConfigChangeInProgress) {
		t.Fatalf("second change: got %v, want ErrConfigChangeInProgress", err)
	}
	if _, err := node.ProposeConfigChange(false, "n1", ""); !errors.Is(err, ErrConfigChangeInProgress) {
		t.Fatalf("voter removal during catch-up: got %v, want ErrConfigChangeInProgress", err)
	}
}

// TestProposeConfigChangeRejectsNonLeaders keeps the HTTP layer's redirect
// contract honest: only the leader starts a change, and the error a follower
// returns is the one main.go maps onto the 503 leader redirect.
func TestProposeConfigChangeRejectsNonLeaders(t *testing.T) {
	rs := NewRaftState("n1", []string{"n1", "n2", "n3"}, make(chan ApplyMsg, 16))
	defer rs.Stop()

	if _, err := rs.ProposeConfigChange(true, "n4", addrN4); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("follower: got %v, want ErrNotLeader", err)
	}
}

// TestKillDuringConfigChange is the R19 shutdown contract under a configuration
// change: Kill has to join every goroutine even while the leader is replicating
// a joint configuration to a peer that will never answer.
func TestKillDuringConfigChange(t *testing.T) {
	cluster := newTestCluster(t)
	defer cluster.stopAll()

	all := []string{"n1", "n2", "n3"}
	for _, id := range all {
		cluster.start(id, all)
	}
	leader := cluster.waitLeader()

	// Add a server that does not exist, so the joint configuration is still being
	// replicated to an unreachable peer when the nodes go down.
	cluster.proposeWhenReady(leader, true, "ghost", "addr-ghost")
	cluster.waitFor("the joint configuration to reach every node", func() bool {
		for _, node := range cluster.nodes {
			if !node.GetClusterConfig().Contains("ghost") {
				return false
			}
		}
		return true
	})

	for _, id := range all {
		killWithin(t, cluster.nodes[id], configTestTimeout)
		delete(cluster.nodes, id)
	}
}
