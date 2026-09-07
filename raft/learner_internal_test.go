package raft

import (
	"errors"
	"testing"
)

// Tests for the learner / non-voting catch-up phase (KNOWN_ISSUES.md R20, paper
// §6 "new servers join as non-voting members", Ongaro's dissertation §4.2.1).
//
// The property under test throughout is that a learner is replicated to and
// nothing else: it is contacted, it is not counted, it does not vote, and the
// moment it has caught up it stops being a learner. Everything runs on
// MockTransport and in-memory persisters or drives RaftState directly, so
// nothing depends on network timing.

// newLeaderWithLearner builds a single-voter leader that has already appended
// and committed a configuration admitting learnerID as a non-voting member, plus
// one ordinary entry above it so a learner can be *behind*. It returns the state
// with rs.mu held by the caller's goroutine — every caller here goes on to
// manipulate MatchIndex, which is leader state.
func newLeaderWithLearner(t *testing.T, learnerID string) *RaftState {
	t.Helper()

	rs := NewRaftState("n1", []string{"n1"}, make(chan ApplyMsg, 16))
	rs.mu.Lock()
	rs.persistent.CurrentTerm = configEntryTerm
	rs.state = Leader
	rs.initializeLeaderState()

	cfg := &ClusterConfig{
		Voters:   map[string]string{"n1": "a1"},
		Learners: map[string]string{learnerID: "a-" + learnerID},
	}
	configIndex, err := rs.appendConfigEntryLocked(cfg)
	if err != nil {
		rs.mu.Unlock()
		rs.Stop()
		t.Fatalf("append the learner configuration: %v", err)
	}
	if _, err := rs.appendEntryLocked("cmd", entryTypeCommand); err != nil {
		rs.mu.Unlock()
		rs.Stop()
		t.Fatalf("append a command above the configuration: %v", err)
	}
	// The configuration that introduced the learner is committed; the command
	// above it is not, so the learner has something left to fetch.
	rs.volatile.CommitIndex = configIndex
	rs.syncLeaderPeersLocked()
	return rs
}

// TestAddAppendsALearnerNotAJointConfiguration is the shape of the fix: adding a
// server no longer touches the voter set at all, so there is nothing for joint
// consensus to protect and no new server counted before it is useful.
func TestAddAppendsALearnerNotAJointConfiguration(t *testing.T) {
	cluster := newTestCluster(t)
	defer cluster.stopAll()

	node := cluster.start("n1", []string{"n1"})
	cluster.waitLeader()

	var proposed *ClusterConfig
	var lastErr error
	cluster.waitFor("the leader to accept the addition", func() bool {
		proposed, lastErr = node.ProposeConfigChange(true, "n2", "addr-n2")
		return lastErr == nil
	})

	if proposed.IsJoint() {
		t.Errorf("adding a server appended a joint configuration: %+v", proposed)
	}
	if !proposed.IsLearner("n2") {
		t.Errorf("the added server is not a learner: %+v", proposed)
	}
	if proposed.IsVoter("n2") {
		t.Errorf("the added server was made a voter straight away: %+v", proposed.Voters)
	}
	if len(proposed.Voters) != 1 || !proposed.IsVoter("n1") {
		t.Errorf("the voter set changed: %+v", proposed.Voters)
	}
	if proposed.Learners["n2"] != "addr-n2" {
		t.Errorf("the learner's address did not travel with the configuration: %+v", proposed.Learners)
	}
	// Contains says "one of ours" and must include a learner; IsVoter says
	// "counted" and must not.
	if !proposed.Contains("n2") {
		t.Error("a learner is part of the configuration and Contains should say so")
	}
}

// addrN2Moved is the new address the re-addressing test moves n2 to; it is
// checked in several places, so it is named rather than repeated.
const addrN2Moved = "a2-moved"

// TestReAddingAVoterChangesItsAddressWithoutDemotingIt covers the one add that
// is not a catch-up: /cluster/add for a server that is already a voter is how
// its address is changed. It must go through joint consensus and leave the
// server a voter — putting a counted server into Learners would break the "never
// in both groups" invariant, and a node that found itself there would refuse to
// vote while QuorumReached went on counting it.
func TestReAddingAVoterChangesItsAddressWithoutDemotingIt(t *testing.T) {
	rs := NewRaftState("n1", []string{"n1"}, make(chan ApplyMsg, 16))
	defer rs.Stop()

	rs.mu.Lock()
	rs.persistent.CurrentTerm = configEntryTerm
	rs.state = Leader
	rs.initializeLeaderState()
	current := &ClusterConfig{Voters: map[string]string{"n1": "a1", "n2": "a2"}}
	at, err := rs.appendConfigEntryLocked(current)
	if err != nil {
		rs.mu.Unlock()
		t.Fatalf("append the starting configuration: %v", err)
	}
	rs.volatile.CommitIndex = at
	rs.mu.Unlock()

	// The same address is still a no-op error.
	if _, err := rs.ProposeConfigChange(true, "n2", "a2"); !errors.Is(err, ErrNodeAlreadyVoter) {
		t.Errorf("re-adding at the same address: got %v, want ErrNodeAlreadyVoter", err)
	}

	moved, err := rs.ProposeConfigChange(true, "n2", addrN2Moved)
	if err != nil {
		t.Fatalf("re-adding a voter at a new address: %v", err)
	}

	if !moved.IsJoint() {
		t.Errorf("changing a voter's address moves the voter set and must be joint: %+v", moved)
	}
	if moved.Voters["n2"] != addrN2Moved {
		t.Errorf("the new address did not reach the voter set: %+v", moved.Voters)
	}
	if moved.OldVoters["n2"] != "a2" {
		t.Errorf("the old voter set should hold the previous address: %+v", moved.OldVoters)
	}
	if len(moved.Learners) != 0 {
		t.Errorf("an existing voter must not be demoted to a learner: %+v", moved.Learners)
	}
	assertNeverBothVoterAndLearner(t, moved)

	// The node's own view agrees: it is still counted, and it still votes.
	if !moved.IsVoter("n2") || moved.IsLearner("n2") {
		t.Errorf("n2 is no longer unambiguously a voter: %+v", moved)
	}
	// Members reports the *new* address, not the one the old group still holds.
	if addr := moved.Members()["n2"]; addr != addrN2Moved {
		t.Errorf("Members reported %q, want the updated address", addr)
	}
}

// assertNeverBothVoterAndLearner checks the ClusterConfig invariant that no
// server appears in a voter group and in Learners at the same time.
func assertNeverBothVoterAndLearner(t *testing.T, cfg *ClusterConfig) {
	t.Helper()
	for id := range cfg.Learners {
		if _, ok := cfg.Voters[id]; ok {
			t.Errorf("%s is both a voter and a learner: %+v", id, cfg)
		}
		if _, ok := cfg.OldVoters[id]; ok {
			t.Errorf("%s is both an old voter and a learner: %+v", id, cfg)
		}
	}
}

// TestConfigForChangeNeverProducesABothGroupsMember runs the invariant over
// every branch of configForChange and over a promotion, which are all the places
// a configuration is built.
func TestConfigForChangeNeverProducesABothGroupsMember(t *testing.T) {
	base := &ClusterConfig{Voters: map[string]string{"n1": "a1", "n2": "a2"}}
	withLearner := &ClusterConfig{
		Voters:   map[string]string{"n1": "a1", "n2": "a2"},
		Learners: map[string]string{"n3": "a3"},
	}

	cases := []struct {
		name    string
		current *ClusterConfig
		add     bool
		nodeID  string
		addr    string
	}{
		{"add a new server", base, true, "n3", "a3"},
		{"re-address a voter", base, true, "n2", addrN2Moved},
		{"remove a voter", base, false, "n2", ""},
		{"remove a learner", withLearner, false, "n3", ""},
		{"remove a voter beside a learner", withLearner, false, "n2", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := configForChange(tc.current, tc.add, tc.nodeID, tc.addr)
			if err != nil {
				t.Fatalf("configForChange: %v", err)
			}
			assertNeverBothVoterAndLearner(t, cfg)
		})
	}
}

// TestLearnersDoNotChangeQuorum is the safety core of the whole feature: a
// learner is invisible to the quorum arithmetic, whether or not it agrees.
func TestLearnersDoNotChangeQuorum(t *testing.T) {
	voters := map[string]string{"n1": "a1", "n2": "a2", "n3": "a3"}
	withLearner := &ClusterConfig{
		Voters:   voters,
		Learners: map[string]string{"n4": "a4", "n5": "a5"},
	}
	withoutLearner := &ClusterConfig{Voters: voters}

	cases := []struct {
		name  string
		agree map[string]bool
		want  bool
	}{
		{"two voters agree", map[string]bool{"n1": true, "n2": true}, true},
		{"one voter agrees", map[string]bool{"n1": true}, false},
		{"one voter and both learners agree", map[string]bool{"n1": true, "n4": true, "n5": true}, false},
		{"only learners agree", map[string]bool{"n4": true, "n5": true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := withLearner.QuorumReached(tc.agree)
			if got != tc.want {
				t.Errorf("QuorumReached(%v) = %v, want %v", tc.agree, got, tc.want)
			}
			if bare := withoutLearner.QuorumReached(tc.agree); bare != got {
				t.Errorf("the learners changed the answer: with=%v without=%v", got, bare)
			}
		})
	}

	// The same for the joint case: learners must not shift either majority.
	joint := &ClusterConfig{
		Voters:    map[string]string{"n1": "a1", "n2": "a2", "n3": "a3", "n4": "a4"},
		OldVoters: voters,
		Learners:  map[string]string{"n5": "a5"},
	}
	agree := map[string]bool{"n1": true, "n2": true, "n3": true, "n5": true}
	if !joint.QuorumReached(agree) {
		t.Errorf("three of three old and three of four new voters is a joint quorum: %v", agree)
	}
	if joint.QuorumReached(map[string]bool{"n1": true, "n5": true}) {
		t.Error("a single voter plus a learner must not form a joint quorum")
	}
}

// TestLearnerIsNotPromotedBeforeItCatchesUp and the test after it are the two
// halves of the promotion rule, driven directly so the decision is observed
// rather than waited for.
func TestLearnerIsNotPromotedBeforeItCatchesUp(t *testing.T) {
	rs := newLeaderWithLearner(t, "n2")
	defer rs.Stop()
	defer rs.mu.Unlock()

	// One entry short of the leader's log.
	rs.leader.MatchIndex["n2"] = rs.lastAbsLogIndex() - 1
	before := len(rs.persistent.Log)

	rs.promoteCaughtUpLearnerLocked()

	if len(rs.persistent.Log) != before {
		t.Fatalf("a learner that is still behind was promoted: log grew from %d to %d",
			before, len(rs.persistent.Log))
	}
	if cfg := rs.persistent.Config; !cfg.IsLearner("n2") || cfg.IsVoter("n2") {
		t.Errorf("the configuration changed: %+v", cfg)
	}
}

// TestCaughtUpLearnerIsPromotedThroughJointConsensus covers both steps: the
// promotion is a C_old,new append (the voter set is changing now, so joint
// consensus applies again), and once that commits the ordinary transition
// finishes it off with C_new.
func TestCaughtUpLearnerIsPromotedThroughJointConsensus(t *testing.T) {
	rs := newLeaderWithLearner(t, "n2")
	defer rs.Stop()
	defer rs.mu.Unlock()

	rs.leader.MatchIndex["n2"] = rs.lastAbsLogIndex()
	rs.promoteCaughtUpLearnerLocked()

	joint := rs.persistent.Config
	if !joint.IsJoint() {
		t.Fatalf("promotion did not append a joint configuration: %+v", joint)
	}
	if !joint.IsVoter("n2") {
		t.Errorf("the promoted learner is not in the new voter set: %+v", joint.Voters)
	}
	if joint.IsLearner("n2") {
		t.Errorf("the promoted learner is still a learner: %+v", joint.Learners)
	}
	if joint.OldVoters["n1"] != "a1" || len(joint.OldVoters) != 1 {
		t.Errorf("the old voter set is not the pre-promotion one: %+v", joint.OldVoters)
	}
	if joint.Voters["n2"] != "a-n2" {
		t.Errorf("the learner's address was lost in the promotion: %+v", joint.Voters)
	}
	assertNeverBothVoterAndLearner(t, joint)

	// Committing the joint configuration is what lets C_new be appended.
	rs.volatile.CommitIndex = rs.lastAbsLogIndex()
	rs.advanceConfigChangeLocked()

	final := rs.persistent.Config
	if final.IsJoint() {
		t.Fatalf("the transition did not reach C_new: %+v", final)
	}
	if !final.IsVoter("n2") || !final.IsVoter("n1") || len(final.Voters) != 2 {
		t.Errorf("C_new is not the two-voter configuration: %+v", final.Voters)
	}
	if len(final.Learners) != 0 {
		t.Errorf("C_new still carries learners: %+v", final.Learners)
	}
}

// TestSecondChangeDuringCatchUpRejected keeps §6's "one change at a time" rule
// honest across the new phase: an unfinished addition is a change in flight,
// even though the configuration it appended is neither joint nor uncommitted.
// Removing the learner is the one exception — it is how the addition is
// abandoned.
func TestSecondChangeDuringCatchUpRejected(t *testing.T) {
	rs := newLeaderWithLearner(t, "n2")
	defer rs.Stop()
	// ProposeConfigChange takes rs.mu itself.
	rs.mu.Unlock()

	if _, err := rs.ProposeConfigChange(true, "n3", "a3"); !errors.Is(err, ErrConfigChangeInProgress) {
		t.Errorf("second add: got %v, want ErrConfigChangeInProgress", err)
	}
	if _, err := rs.ProposeConfigChange(false, "n1", ""); !errors.Is(err, ErrConfigChangeInProgress) {
		t.Errorf("voter removal during catch-up: got %v, want ErrConfigChangeInProgress", err)
	}

	cfg, err := rs.ProposeConfigChange(false, "n2", "")
	if err != nil {
		t.Fatalf("removing the learner should be allowed: %v", err)
	}
	if cfg.IsJoint() {
		t.Errorf("removing a learner does not change the voter set and must not be joint: %+v", cfg)
	}
	if cfg.IsLearner("n2") || len(cfg.Learners) != 0 {
		t.Errorf("the learner is still in the configuration: %+v", cfg.Learners)
	}
	if len(cfg.Voters) != 1 || !cfg.IsVoter("n1") {
		t.Errorf("removing a learner changed the voter set: %+v", cfg.Voters)
	}
}

// TestLearnerRefusesToVoteAndToCampaign covers the node-side half: a server that
// learns from a configuration entry that it is a learner neither grants votes
// nor stands for election.
func TestLearnerRefusesToVoteAndToCampaign(t *testing.T) {
	rs := NewRaftState("n2", []string{"n1"}, make(chan ApplyMsg, 16))
	defer rs.Stop()

	admitted := &ClusterConfig{
		Voters:   map[string]string{"n1": "a1"},
		Learners: map[string]string{"n2": "a2"},
	}
	reply := &AppendEntriesReply{}
	rs.AppendEntries(&AppendEntriesArgs{
		Term: configEntryTerm, LeaderID: "n1", PrevLogIndex: 0, PrevLogTerm: 0,
		Entries: []LogEntry{configEntry(t, admitted)},
	}, reply)
	if !reply.Success {
		t.Fatalf("setup AppendEntries rejected: %+v", reply)
	}

	if rs.IsVoter() {
		t.Error("a learner must not report itself a voter; that is what stops it campaigning")
	}
	if !rs.GetClusterConfig().IsLearner("n2") {
		t.Fatalf("the configuration entry did not make this node a learner: %+v", rs.GetClusterConfig())
	}

	// The request is made in the *current* term on purpose: a higher term would
	// be swallowed by §6's disruption check (this node has just heard from n1),
	// and the refusal under test would never be reached. In this term the vote
	// would otherwise be granted — VotedFor is nil and the candidate's log is
	// ahead — so what refuses it is the learner rule and nothing else.
	voteReply := &RequestVoteReply{}
	rs.RequestVote(&RequestVoteArgs{
		Term:         configEntryTerm,
		CandidateID:  "n3",
		LastLogIndex: 99,
		LastLogTerm:  99,
	}, voteReply)

	if voteReply.VoteGranted {
		t.Error("a non-voting member granted a vote")
	}
	if voteReply.Term != configEntryTerm {
		t.Errorf("reply term %d, want %d", voteReply.Term, configEntryTerm)
	}
	if votedFor := rs.GetVotedFor(); votedFor != nil {
		t.Errorf("a refused vote must not spend VotedFor: %v", *votedFor)
	}
}

// TestTruncatingTheLearnerEntryRemovesTheLearner checks that a learner is
// configuration state like any other: it is derived from the log, so an entry
// removed by a conflicting AppendEntries takes the learner with it (§6).
func TestTruncatingTheLearnerEntryRemovesTheLearner(t *testing.T) {
	rs := NewRaftState("n2", []string{"n1", "n2", "n3"}, make(chan ApplyMsg, 16))
	defer rs.Stop()

	admitted := &ClusterConfig{
		Voters:   map[string]string{"n1": "a1", "n2": "a2", "n3": "a3"},
		Learners: map[string]string{"n4": addrN4},
	}
	reply := &AppendEntriesReply{}
	rs.AppendEntries(&AppendEntriesArgs{
		Term: configEntryTerm, LeaderID: "n1", PrevLogIndex: 0, PrevLogTerm: 0,
		Entries: []LogEntry{configEntry(t, admitted)},
	}, reply)
	if !reply.Success {
		t.Fatalf("setup AppendEntries rejected: %+v", reply)
	}
	if !rs.GetClusterConfig().IsLearner("n4") {
		t.Fatal("the learner configuration did not take effect on append")
	}

	// A new leader overwrites index 1 with an entry of a later term.
	reply = &AppendEntriesReply{}
	rs.AppendEntries(&AppendEntriesArgs{
		Term: configEntryTerm + 1, LeaderID: "n3", PrevLogIndex: 0, PrevLogTerm: 0,
		Entries: []LogEntry{{Term: configEntryTerm + 1, Index: 1, Command: "cmd", Type: entryTypeCommand}},
	}, reply)
	if !reply.Success {
		t.Fatalf("conflicting AppendEntries rejected: %+v", reply)
	}

	config := rs.GetClusterConfig()
	if config.IsLearner("n4") || config.Contains("n4") {
		t.Errorf("truncating the entry did not remove the learner: %+v", config)
	}
	if len(config.Voters) != 3 {
		t.Errorf("expected the original three voters after the revert: %+v", config.Voters)
	}
}

// TestInstallSnapshotCarriesLearners: a snapshot subsumes the configuration
// entries below its boundary, learners included, so the configuration it ships
// has to carry them or a follower that installs one forgets a catch-up in
// progress.
func TestInstallSnapshotCarriesLearners(t *testing.T) {
	rs := NewRaftState("n2", []string{"n1", "n2"}, make(chan ApplyMsg, 16))
	defer rs.Stop()

	shipped := &ClusterConfig{
		Voters:   map[string]string{"n1": "a1", "n2": "a2"},
		Learners: map[string]string{"n3": "a3"},
	}
	reply := &InstallSnapshotReply{}
	rs.InstallSnapshot(&InstallSnapshotArgs{
		Term:              configEntryTerm,
		LeaderID:          "n1",
		LastIncludedIndex: 7,
		LastIncludedTerm:  configEntryTerm,
		Data:              []byte("payload"),
		Done:              true,
		Config:            shipped,
	}, reply)

	config := rs.GetClusterConfig()
	if !config.IsLearner("n3") {
		t.Fatalf("the installed snapshot dropped the learner: %+v", config)
	}
	if config.IsVoter("n3") {
		t.Errorf("the learner arrived as a voter: %+v", config.Voters)
	}

	rs.mu.RLock()
	boundary := rs.persistent.SnapshotConfig
	rs.mu.RUnlock()
	if !boundary.IsLearner("n3") {
		t.Errorf("the snapshot boundary configuration lost the learner: %+v", boundary)
	}
}

// TestLearnerConfigEncodeRoundTrip pins the wire form: learners travel in the
// configuration entry itself, so an omitted field must decode to no learners
// rather than to something the receiver has to guess at.
func TestLearnerConfigEncodeRoundTrip(t *testing.T) {
	original := &ClusterConfig{
		Voters:   map[string]string{"n1": "a1", "n2": "a2"},
		Learners: map[string]string{"n3": "addr-n3"},
	}
	encoded, err := original.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := decodeClusterConfig(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !decoded.IsLearner("n3") || decoded.Learners["n3"] != "addr-n3" {
		t.Errorf("round trip lost the learner: %+v", decoded)
	}
	if decoded.IsJoint() {
		t.Errorf("a learner configuration is not joint: %+v", decoded)
	}

	// A configuration written before learners existed has none.
	plain, err := decodeClusterConfig(`{"voters":{"n1":"a1"}}`)
	if err != nil {
		t.Fatalf("decode without learners: %v", err)
	}
	if len(plain.Learners) != 0 {
		t.Errorf("a configuration with no learners decoded to %+v", plain.Learners)
	}
}

// TestAddedServerIsCaughtUpThenPromoted is the end-to-end case on a real
// cluster: the newcomer is admitted as a learner, is replicated to, and the
// leader promotes it on its own once it has caught up — no second API call.
func TestAddedServerIsCaughtUpThenPromoted(t *testing.T) {
	cluster := newTestCluster(t)
	defer cluster.stopAll()

	original := []string{"n1", "n2", "n3"}
	for _, id := range original {
		cluster.start(id, original)
	}
	leader := cluster.waitLeader()

	newcomer := cluster.start("n4", original)
	if newcomer.GetClusterConfig().Contains("n4") {
		t.Fatal("a server started with the existing cluster's peer list is not in the configuration yet")
	}

	cluster.proposeWhenReady(leader, true, "n4", addrN4)

	// The leader finishes the addition by itself: every node ends up with n4 as a
	// voter and no learners left.
	cluster.waitConfig("n1", "n2", "n3", "n4")
	cluster.waitFor("no learner to remain anywhere", func() bool {
		for _, node := range cluster.nodes {
			if len(node.GetClusterConfig().Learners) != 0 {
				return false
			}
		}
		return true
	})
	if config := newcomer.GetClusterConfig(); !config.IsVoter("n4") {
		t.Errorf("the newcomer does not consider itself a voter: %+v", config)
	}
}
