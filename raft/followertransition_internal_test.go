package raft

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// White-box tests for the single follower transition (KNOWN_ISSUES.md R6).
//
// becomeLeader stops the election timer, so every path that leaves the Leader
// state has to re-arm it. The three reply paths (requestVoteFromPeer,
// replicateToPeer, sendSnapshotToPeer) and ReadIndex's stepDown used to set
// state/term/VotedFor and persist without touching the timer, which left the
// demoted node a follower that could never campaign: it stayed idle until some
// other leader reached it, and in a partition that is never.

// demotionTermJump is how far ahead the fake peer's term is, so the demotion is
// unambiguous in the assertions below.
const demotionTermJump = 5

// demotingTransport lets the node under test win its first election, then
// answers the first AppendEntries with a much higher term. The node is therefore
// demoted by an RPC *reply* while it is the leader — the exact R6 path — and
// nothing ever sends it an AppendEntries afterwards, so only its own election
// timer can bring it back.
type demotingTransport struct {
	mu         sync.Mutex
	demoted    bool
	demoteTerm int
	sawLeader  chan struct{}
}

func newDemotingTransport() *demotingTransport {
	return &demotingTransport{sawLeader: make(chan struct{})}
}

func (d *demotingTransport) SendRequestVote(
	_ context.Context, _ string, args *RequestVoteArgs,
) (*RequestVoteReply, error) {
	return &RequestVoteReply{Term: args.Term, VoteGranted: true}, nil
}

func (d *demotingTransport) SendAppendEntries(
	_ context.Context, _ string, args *AppendEntriesArgs,
) (*AppendEntriesReply, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.demoted {
		d.demoted = true
		d.demoteTerm = args.Term + demotionTermJump
		// Only a leader replicates, so reaching here proves the node was elected
		// and its election timer had been stopped by becomeLeader.
		close(d.sawLeader)
		return &AppendEntriesReply{Term: d.demoteTerm, Success: false}, nil
	}
	return &AppendEntriesReply{Term: args.Term, Success: true}, nil
}

func (d *demotingTransport) SendInstallSnapshot(
	_ context.Context, _ string, args *InstallSnapshotArgs,
) (*InstallSnapshotReply, error) {
	return &InstallSnapshotReply{Term: args.Term}, nil
}

func (d *demotingTransport) demotedAt() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.demoteTerm
}

// TestDemotedLeaderCampaignsAgain is the R6 regression test. A leader is demoted
// by a higher term carried in an AppendEntries reply and then hears from nobody.
// It must still reach its own election timeout and campaign, raising the term
// past the one that demoted it. Before the fix the node stayed a follower with a
// stopped timer for the rest of its life.
func TestDemotedLeaderCampaignsAgain(t *testing.T) {
	applyCh := make(chan ApplyMsg, 64)
	go func() {
		for range applyCh {
		}
	}()

	transport := newDemotingTransport()
	node := NewRaftNode("n1", []string{"n1", "n2", "n3"}, transport, applyCh)
	t.Cleanup(node.Kill)

	select {
	case <-transport.sawLeader:
	case <-time.After(5 * time.Second):
		t.Fatal("node never became leader, so the demotion path was never exercised")
	}

	demoteTerm := transport.demotedAt()

	deadline := time.After(5 * time.Second)
	for {
		term, _ := node.GetState()
		if term > demoteTerm {
			return // campaigned again on its own
		}
		select {
		case <-deadline:
			t.Fatalf("node demoted to term %d is stuck at term %d: the election timer "+
				"becomeLeader stopped was never re-armed on demotion (R6)", demoteTerm, term)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// unreachablePeers fails every RPC, so a node using it can never win an election.
type unreachablePeers struct{}

func (unreachablePeers) SendRequestVote(
	_ context.Context, _ string, _ *RequestVoteArgs,
) (*RequestVoteReply, error) {
	return nil, errors.New("unreachable")
}

func (unreachablePeers) SendAppendEntries(
	_ context.Context, _ string, _ *AppendEntriesArgs,
) (*AppendEntriesReply, error) {
	return nil, errors.New("unreachable")
}

func (unreachablePeers) SendInstallSnapshot(
	_ context.Context, _ string, _ *InstallSnapshotArgs,
) (*InstallSnapshotReply, error) {
	return nil, errors.New("unreachable")
}

// flakyPersister fails SaveRaftState while its failure flag is set, standing in
// for storage that is temporarily unwritable.
type flakyPersister struct {
	mu    sync.Mutex
	fail  bool
	saved *PersistentState
}

func (p *flakyPersister) SaveRaftState(state *PersistentState) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail {
		return errors.New("simulated storage failure")
	}
	saved := *state
	p.saved = &saved
	return nil
}

func (p *flakyPersister) LoadRaftState() (*PersistentState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.saved == nil {
		return nil, nil
	}
	loaded := *p.saved
	return &loaded, nil
}

func (p *flakyPersister) setFail(fail bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fail = fail
}

// TestStartElectionRearmsTimerAfterPersistFailure covers the other half of R6:
// an election aborted because the candidacy could not be made durable must leave
// a follower whose timer is armed, so the node campaigns again once storage
// recovers. The abort path now goes through the same becomeFollowerLocked as
// every demotion.
func TestStartElectionRearmsTimerAfterPersistFailure(t *testing.T) {
	persister := &flakyPersister{}
	applyCh := make(chan ApplyMsg, 8)
	rs, err := NewRaftStateWithPersister("n1", []string{"n1", "n2", "n3"}, applyCh, persister)
	if err != nil {
		t.Fatalf("NewRaftStateWithPersister: %v", err)
	}

	persister.setFail(true)
	rs.startElection(unreachablePeers{})

	if got := rs.GetNodeState(); got != Follower {
		t.Fatalf("state after a non-durable candidacy = %v, want Follower", got)
	}

	select {
	case <-rs.ElectionTimer():
	case <-time.After(5 * time.Second):
		t.Fatal("election timer was not re-armed after the aborted election; " +
			"this node would never campaign again")
	}

	// Storage recovers: the next timeout must produce a real candidacy.
	persister.setFail(false)
	rs.startElection(unreachablePeers{})
	if got := rs.GetNodeState(); got != Candidate {
		t.Fatalf("state after storage recovered = %v, want Candidate", got)
	}
}
