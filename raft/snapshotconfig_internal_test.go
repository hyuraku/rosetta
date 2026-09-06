package raft

import (
	"testing"
)

// White-box tests for the cluster configuration traveling with a snapshot
// (KNOWN_ISSUES.md R14, paper §6/§7).
//
// A snapshot subsumes the log prefix below its boundary, configuration entries
// included. A follower that installs one has therefore just discarded every
// trace of the configuration it is meant to be part of and — having discarded
// that prefix — can never be told again by ordinary replication. So the snapshot
// has to bring the configuration with it.

// TestInstallSnapshotCarriesTheConfiguration covers the receive path, including
// what a restart afterwards recovers.
func TestInstallSnapshotCarriesTheConfiguration(t *testing.T) {
	persister := &memPersister{}
	rs, err := NewRaftStateWithPersister("n2", []string{"n1", "n2"},
		make(chan ApplyMsg, 16), persister)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}

	shipped := &ClusterConfig{Voters: map[string]string{"n1": "a1", "n2": "a2", "n3": "a3"}}
	reply := &InstallSnapshotReply{}
	rs.InstallSnapshot(&InstallSnapshotArgs{
		Term:              2,
		LeaderID:          "n1",
		LastIncludedIndex: 7,
		LastIncludedTerm:  2,
		Data:              []byte("payload"),
		Done:              true,
		Config:            shipped,
	}, reply)

	if config := rs.GetClusterConfig(); !config.IsVoter("n3") || len(config.Voters) != 3 {
		t.Fatalf("the installed snapshot did not bring its configuration: %+v", config.Voters)
	}
	rs.Stop()

	restarted, err := NewRaftStateWithPersister("n2", []string{"n1", "n2"},
		make(chan ApplyMsg, 16), persister)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer restarted.Stop()

	if config := restarted.GetClusterConfig(); !config.IsVoter("n3") || len(config.Voters) != 3 {
		t.Errorf("restart after InstallSnapshot lost the configuration: %+v", config.Voters)
	}
}

// TestInstallSnapshotWithoutConfigurationKeepsOurs checks the backward-compatible
// case: a sender that attaches no configuration must leave the receiver's alone
// rather than blanking it.
func TestInstallSnapshotWithoutConfigurationKeepsOurs(t *testing.T) {
	rs := NewRaftState("n2", []string{"n1", "n2", "n3"}, make(chan ApplyMsg, 16))
	defer rs.Stop()

	reply := &InstallSnapshotReply{}
	rs.InstallSnapshot(&InstallSnapshotArgs{
		Term:              2,
		LeaderID:          "n1",
		LastIncludedIndex: 7,
		LastIncludedTerm:  2,
		Data:              []byte("payload"),
		Done:              true,
	}, reply)

	config := rs.GetClusterConfig()
	if len(config.Voters) != 3 || !config.IsVoter("n2") {
		t.Errorf("a snapshot with no configuration changed ours: %+v", config.Voters)
	}
}

// TestSnapshotShipsTheBoundaryConfiguration checks the send side, and
// specifically that the configuration shipped is the one in effect at the
// snapshot's boundary rather than the leader's current one. The leader's entries
// above the boundary are replayed to the follower afterwards, so seeding it with
// a newer configuration would place that configuration at a log position it was
// never agreed at — and if the follower later truncated those entries, it would
// have nothing correct to revert to.
func TestSnapshotShipsTheBoundaryConfiguration(t *testing.T) {
	rs := NewRaftState("n1", []string{"n1", "n2"}, make(chan ApplyMsg, 16))
	defer rs.Stop()

	atBoundary := &ClusterConfig{Voters: map[string]string{"n1": "a1", "n2": "a2"}}
	newer := &ClusterConfig{Voters: map[string]string{"n1": "a1", "n2": "a2", "n3": "a3"}}

	rs.mu.Lock()
	rs.persistent.CurrentTerm = 3
	boundaryIndex, err := rs.appendConfigEntryLocked(atBoundary)
	if err != nil {
		rs.mu.Unlock()
		t.Fatalf("append boundary configuration: %v", err)
	}
	if _, err := rs.appendConfigEntryLocked(newer); err != nil {
		rs.mu.Unlock()
		t.Fatalf("append newer configuration: %v", err)
	}
	rs.mu.Unlock()
	rs.SetState(Leader)

	transport := &captureTransport{}
	rs.SetSnapshotter(&generationSnapshotter{current: &SnapshotData{
		LastIncludedIndex: boundaryIndex,
		LastIncludedTerm:  3,
		Data:              []byte("payload"),
	}})
	rs.sendSnapshotToPeer(transport, "n2", 3, boundaryIndex+1, rs.snapshotter)

	sent := transport.sent()
	if len(sent) == 0 {
		t.Fatal("no InstallSnapshot was sent")
	}
	shipped := sent[len(sent)-1].Config
	if shipped == nil {
		t.Fatal("InstallSnapshot carried no configuration")
	}
	if shipped.IsVoter("n3") {
		t.Errorf("shipped the leader's current configuration instead of the boundary's: %+v", shipped.Voters)
	}
	if !shipped.IsVoter("n1") || !shipped.IsVoter("n2") || len(shipped.Voters) != 2 {
		t.Errorf("shipped configuration is not the boundary's: %+v", shipped.Voters)
	}
}
