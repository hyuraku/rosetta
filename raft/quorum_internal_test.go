package raft

import (
	"context"
	"testing"
	"time"
)

// White-box tests for quorum decisions made from the cluster configuration
// (KNOWN_ISSUES.md R14, paper §6), and for the two protections that keep a
// server outside the configuration from disrupting one.

// TestQuorumReached pins the arithmetic itself, including the cases the cluster
// tests can only reach indirectly.
func TestQuorumReached(t *testing.T) {
	single := NewClusterConfig([]string{"n1", "n2", "n3"})
	joint := &ClusterConfig{
		Voters:    NewClusterConfig([]string{"n3", "n4", "n5"}).Voters,
		OldVoters: NewClusterConfig([]string{"n1", "n2", "n3"}).Voters,
	}

	cases := []struct {
		name   string
		config *ClusterConfig
		agree  []string
		want   bool
	}{
		{"single: majority", single, []string{"n1", "n2"}, true},
		{"single: minority", single, []string{"n1"}, false},
		{"single: outsiders do not count", single, []string{"n1", "n9", "n8"}, false},
		{"joint: majority of old only", joint, []string{"n1", "n2"}, false},
		{"joint: majority of new only", joint, []string{"n4", "n5"}, false},
		{"joint: majority of both", joint, []string{"n1", "n3", "n4"}, true},
		{"joint: the shared server alone", joint, []string{"n3"}, false},
		{"nil configuration is never a quorum", nil, []string{"n1", "n2", "n3"}, false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			agree := make(map[string]bool, len(testCase.agree))
			for _, id := range testCase.agree {
				agree[id] = true
			}
			if got := testCase.config.QuorumReached(agree); got != testCase.want {
				t.Errorf("QuorumReached(%v) = %v, want %v", testCase.agree, got, testCase.want)
			}
		})
	}
}

// selectiveVoteTransport grants a vote only to the peers named in granters and
// reports every other peer as unreachable. It makes "which servers voted" an
// exact input, which is what a joint-quorum test needs.
type selectiveVoteTransport struct {
	granters map[string]bool
}

func (s *selectiveVoteTransport) SendRequestVote(
	_ context.Context, target string, args *RequestVoteArgs,
) (*RequestVoteReply, error) {
	if !s.granters[target] {
		return nil, context.DeadlineExceeded
	}
	return &RequestVoteReply{Term: args.Term, VoteGranted: true}, nil
}

func (s *selectiveVoteTransport) SendAppendEntries(
	_ context.Context, _ string, args *AppendEntriesArgs,
) (*AppendEntriesReply, error) {
	return &AppendEntriesReply{Term: args.Term, Success: true}, nil
}

func (s *selectiveVoteTransport) SendInstallSnapshot(
	_ context.Context, _ string, args *InstallSnapshotArgs,
) (*InstallSnapshotReply, error) {
	return &InstallSnapshotReply{Term: args.Term}, nil
}

// jointCandidate builds a candidate whose log already holds a joint
// configuration entry, so its elections are decided by both majorities. The
// entry arrives the way a follower's would, through AppendEntries.
func jointCandidate(t *testing.T, oldVoters, newVoters []string) *RaftState {
	t.Helper()

	rs := NewRaftState("n1", oldVoters, make(chan ApplyMsg, 16))
	t.Cleanup(rs.Stop)

	joint := &ClusterConfig{
		Voters:    NewClusterConfig(newVoters).Voters,
		OldVoters: NewClusterConfig(oldVoters).Voters,
	}
	reply := &AppendEntriesReply{}
	rs.AppendEntries(&AppendEntriesArgs{
		Term: configEntryTerm, LeaderID: "n2", PrevLogIndex: 0, PrevLogTerm: 0,
		Entries: []LogEntry{configEntry(t, joint)},
	}, reply)
	if !reply.Success {
		t.Fatalf("setup AppendEntries rejected: %+v", reply)
	}
	if !rs.GetClusterConfig().IsJoint() {
		t.Fatal("setup did not leave the node in a joint configuration")
	}

	// The setup above made this node believe in a leader, which would make the
	// election below look like a disruption. Clear that, as a real election
	// timeout would.
	rs.mu.Lock()
	rs.currentLeader = ""
	rs.mu.Unlock()
	return rs
}

// TestJointElectionNeedsBothMajorities is the safety property joint consensus
// exists for. While C_old,new is in effect, a majority of only one of the two
// configurations must not be enough to win an election — that is precisely the
// window in which two disjoint majorities could otherwise elect two leaders in
// the same term.
//
// It is also what a leader crashing mid-change comes down to: whoever campaigns
// next holds the joint entry and is bound by this rule, so the survivors of one
// configuration alone cannot elect a leader between them.
func TestJointElectionNeedsBothMajorities(t *testing.T) {
	oldVoters := []string{"n1", "n2", "n3"}
	newVoters := []string{"n1", "n2", "n3", "n4", "n5"}

	// n1 + n2 is a majority of the old configuration (2 of 3) but not of the new
	// one (2 of 5).
	onlyOld := jointCandidate(t, oldVoters, newVoters)
	onlyOld.startElection(&selectiveVoteTransport{granters: map[string]bool{"n2": true}})
	if onlyOld.GetNodeState() == Leader {
		t.Error("a majority of C_old alone elected a leader during a joint configuration")
	}

	// n4 + n5 joining in makes it 2 of 3 old and 4 of 5 new: a quorum in both.
	both := jointCandidate(t, oldVoters, newVoters)
	both.startElection(&selectiveVoteTransport{
		granters: map[string]bool{"n2": true, "n4": true, "n5": true},
	})
	waitForState(t, both, Leader)
}

func waitForState(t *testing.T, rs *RaftState, want NodeState) {
	t.Helper()
	deadline := time.Now().Add(configTestTimeout)
	for time.Now().Before(deadline) {
		if rs.GetNodeState() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("node never reached %v (still %v)", want, rs.GetNodeState())
}

// TestRemovedServerCannotDisruptTheLeader covers the last paragraph of §6. A
// server that has been removed stops receiving heartbeats, times out, and
// campaigns with ever-higher terms. A voter that is still hearing from its
// leader must disregard those requests entirely — not merely refuse the vote,
// but leave its term alone, since adopting the term is what would depose a
// perfectly healthy leader.
func TestRemovedServerCannotDisruptTheLeader(t *testing.T) {
	rs := NewRaftState("n2", []string{"n1", "n2", "n3"}, make(chan ApplyMsg, 16))
	defer rs.Stop()

	reply := &AppendEntriesReply{}
	rs.AppendEntries(&AppendEntriesArgs{
		Term: 4, LeaderID: "n1", PrevLogIndex: 0, PrevLogTerm: 0,
	}, reply)
	if !reply.Success {
		t.Fatalf("setup AppendEntries rejected: %+v", reply)
	}

	voteReply := &RequestVoteReply{}
	rs.RequestVote(&RequestVoteArgs{
		Term: 9, CandidateID: "removed", LastLogIndex: 0, LastLogTerm: 0,
	}, voteReply)

	if voteReply.VoteGranted {
		t.Error("granted a vote to a candidate while a current leader was known")
	}
	if got := rs.GetCurrentTerm(); got != 4 {
		t.Errorf("the disruptive RequestVote raised our term to %d; it must stay at 4", got)
	}
	if leader := rs.GetCurrentLeader(); leader != "n1" {
		t.Errorf("the disruptive RequestVote cleared our leader (%q)", leader)
	}
}

// TestDisruptionCheckLapsesWhenTheLeaderIsSilent is the other half of the rule:
// the protection is time-boxed, so a genuinely dead leader does not block
// elections forever.
func TestDisruptionCheckLapsesWhenTheLeaderIsSilent(t *testing.T) {
	rs := NewRaftState("n2", []string{"n1", "n2", "n3"}, make(chan ApplyMsg, 16))
	defer rs.Stop()

	reply := &AppendEntriesReply{}
	rs.AppendEntries(&AppendEntriesArgs{
		Term: 4, LeaderID: "n1", PrevLogIndex: 0, PrevLogTerm: 0,
	}, reply)
	if !reply.Success {
		t.Fatalf("setup AppendEntries rejected: %+v", reply)
	}

	// Backdate the last contact past the minimum election timeout, which is what
	// the passage of time does in a real cluster.
	rs.mu.Lock()
	rs.lastHeartbeat = time.Now().Add(-2 * minElectionTimeout)
	rs.mu.Unlock()

	voteReply := &RequestVoteReply{}
	rs.RequestVote(&RequestVoteArgs{
		Term: 9, CandidateID: "n3", LastLogIndex: 0, LastLogTerm: 0,
	}, voteReply)

	if !voteReply.VoteGranted {
		t.Errorf("a candidate must be heard once the leader has been silent: %+v", voteReply)
	}
}

// TestNonVoterDoesNotCampaign checks the local half of the disruption
// protection: a server holding a configuration it is not part of never starts an
// election, so a server waiting to be added (and one that has been removed but
// not yet shut down) stays quiet on its own.
func TestNonVoterDoesNotCampaign(t *testing.T) {
	applyCh := make(chan ApplyMsg, 16)
	node := NewRaftNode("outsider", []string{"n1", "n2", "n3"}, NewMockTransport(), applyCh)
	defer node.Kill()

	if node.GetRaftState().IsVoter() {
		t.Fatal("a server absent from its own configuration must not be a voter")
	}

	// Well past the maximum election timeout.
	time.Sleep(2 * (electionTimeoutBaseMs + electionTimeoutJitterMs) * time.Millisecond)

	if term := node.GetRaftState().GetCurrentTerm(); term != 0 {
		t.Errorf("a non-voter campaigned and raised its term to %d", term)
	}
	if node.IsLeader() {
		t.Error("a non-voter elected itself")
	}
}
