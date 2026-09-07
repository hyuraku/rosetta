package raft

import (
	"sync"
	"testing"
	"time"
)

// White-box tests for the cluster configuration as persistent state
// (KNOWN_ISSUES.md R14, paper §6): how a configuration entry is encoded, when it
// takes effect, what happens when it is truncated or compacted away, and what a
// restart recovers.
//
// Everything here drives RaftState's RPC handlers directly, so nothing depends
// on network or election timing.

const (
	// configTestTimeout bounds every "wait until this settles" poll in the
	// membership tests.
	configTestTimeout = 5 * time.Second
)

// memPersister is an in-memory Persister that keeps the last successfully saved
// state, so a test can build a node, take its "disk" and restart from it.
type memPersister struct {
	mu    sync.Mutex
	saved PersistentState
}

func (p *memPersister) SaveRaftState(state *PersistentState) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.saved = clonePersistent(state)
	return nil
}

func (p *memPersister) LoadRaftState() (*PersistentState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	loaded := clonePersistent(&p.saved)
	return &loaded, nil
}

// clonePersistent deep-copies the parts a test inspects. SaveRaftState is handed
// a pointer straight at rs.persistent, so keeping the argument would alias live
// state — including the configuration maps.
func clonePersistent(state *PersistentState) PersistentState {
	clone := *state
	clone.Log = append(make([]LogEntry, 0, len(state.Log)), state.Log...)
	if state.VotedFor != nil {
		votedFor := *state.VotedFor
		clone.VotedFor = &votedFor
	}
	clone.Config = state.Config.Clone()
	clone.SnapshotConfig = state.SnapshotConfig.Clone()
	return clone
}

// configEntryTerm is the term every configuration entry these tests build is
// stamped with; the term itself is never what is under test here.
const configEntryTerm = 2

// addrN4 is the address the tests attach to the added server, checked to make
// sure the address travels with the configuration and not just the node ID.
const addrN4 = "a4"

// configEntry builds the log entry a leader would append for cfg. Every test
// here puts it at index 1, the first entry of the log.
func configEntry(t *testing.T, cfg *ClusterConfig) LogEntry {
	t.Helper()
	encoded, err := cfg.encode()
	if err != nil {
		t.Fatalf("encode configuration: %v", err)
	}
	return LogEntry{Term: configEntryTerm, Index: 1, Command: encoded, Type: entryTypeConfig}
}

// TestClusterConfigEncodeRoundTrip pins the wire form of a configuration entry.
// The entry travels as a JSON *string* in LogEntry.Command, like every other
// command here, so that the HTTP transport's round trip through interface{}
// hands the receiver the same Go type the leader appended.
func TestClusterConfigEncodeRoundTrip(t *testing.T) {
	original := &ClusterConfig{
		Voters:    map[string]string{"n1": "a1", "n2": "a2", "n3": "a3"},
		OldVoters: map[string]string{"n1": "a1", "n2": "a2"},
	}

	encoded, err := original.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := decodeClusterConfig(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !decoded.IsJoint() || len(decoded.Voters) != 3 || decoded.Voters["n3"] != "a3" {
		t.Errorf("round trip changed the configuration: %+v", decoded)
	}

	// The same bytes as a []byte, which is what a hand-built entry may carry.
	if _, err := decodeClusterConfig([]byte(encoded)); err != nil {
		t.Errorf("decoding []byte: %v", err)
	}
	// And a command that is not a configuration at all must be reported, not
	// silently treated as an empty cluster.
	if _, err := decodeClusterConfig(42); err == nil {
		t.Error("decoding a non-string command should fail")
	}
	if _, err := decodeClusterConfig(`{"old_voters":{}}`); err == nil {
		t.Error("decoding a configuration with no voters should fail")
	}
}

// TestConfigurationTakesEffectOnAppend covers §6's central rule: a server uses
// the latest configuration in its log whether or not that entry is committed. So
// merely receiving the entry has to change what this node believes the cluster
// is.
func TestConfigurationTakesEffectOnAppend(t *testing.T) {
	rs := NewRaftState("n2", []string{"n1", "n2", "n3"}, make(chan ApplyMsg, 16))
	defer rs.Stop()

	if rs.GetClusterConfig().IsVoter("n4") {
		t.Fatal("n4 is not in the initial configuration")
	}

	added := &ClusterConfig{Voters: map[string]string{"n1": "a1", "n2": "a2", "n3": "a3", "n4": addrN4}}
	reply := &AppendEntriesReply{}
	rs.AppendEntries(&AppendEntriesArgs{
		Term: 2, LeaderID: "n1", PrevLogIndex: 0, PrevLogTerm: 0,
		Entries: []LogEntry{configEntry(t, added)},
	}, reply)
	if !reply.Success {
		t.Fatalf("AppendEntries rejected: %+v", reply)
	}

	config := rs.GetClusterConfig()
	if !config.IsVoter("n4") || config.Voters["n4"] != addrN4 {
		t.Errorf("the configuration entry did not take effect on append: %+v", config.Voters)
	}
	if rs.GetCommitIndex() >= 1 {
		t.Fatal("test precondition: the entry must still be uncommitted")
	}
}

// TestTruncatedConfigurationEntryReverts covers the other half of that rule:
// removing a configuration entry from the log reverts to the previous
// configuration. A conflicting AppendEntries from a new leader is what removes
// it in practice.
func TestTruncatedConfigurationEntryReverts(t *testing.T) {
	rs := NewRaftState("n2", []string{"n1", "n2", "n3"}, make(chan ApplyMsg, 16))
	defer rs.Stop()

	added := &ClusterConfig{Voters: map[string]string{"n1": "a1", "n2": "a2", "n3": "a3", "n4": addrN4}}
	reply := &AppendEntriesReply{}
	rs.AppendEntries(&AppendEntriesArgs{
		Term: 2, LeaderID: "n1", PrevLogIndex: 0, PrevLogTerm: 0,
		Entries: []LogEntry{configEntry(t, added)},
	}, reply)
	if !reply.Success {
		t.Fatalf("setup AppendEntries rejected: %+v", reply)
	}

	// A new leader overwrites index 1 with an entry of a later term.
	reply = &AppendEntriesReply{}
	rs.AppendEntries(&AppendEntriesArgs{
		Term: 3, LeaderID: "n3", PrevLogIndex: 0, PrevLogTerm: 0,
		Entries: []LogEntry{{Term: 3, Index: 1, Command: "cmd", Type: entryTypeCommand}},
	}, reply)
	if !reply.Success {
		t.Fatalf("conflicting AppendEntries rejected: %+v", reply)
	}

	config := rs.GetClusterConfig()
	if config.IsVoter("n4") {
		t.Errorf("truncating the configuration entry did not revert the configuration: %+v", config.Voters)
	}
	if len(config.Voters) != 3 {
		t.Errorf("expected the original three voters after the revert, got %+v", config.Voters)
	}
}

// TestConfigurationSurvivesRestartFromLog checks that a node interrupted in the
// middle of a configuration change comes back with the joint configuration still
// in effect. It has to be recovered from the log, not from the peer list the
// process was started with — which is why the restart below deliberately passes
// a stale one.
func TestConfigurationSurvivesRestartFromLog(t *testing.T) {
	persister := &memPersister{}
	rs, err := NewRaftStateWithPersister("n2", []string{"n1", "n2", "n3"},
		make(chan ApplyMsg, 16), persister)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}

	joint := &ClusterConfig{
		Voters:    map[string]string{"n1": "a1", "n2": "a2", "n3": "a3", "n4": addrN4},
		OldVoters: map[string]string{"n1": "a1", "n2": "a2", "n3": "a3"},
	}
	reply := &AppendEntriesReply{}
	rs.AppendEntries(&AppendEntriesArgs{
		Term: 2, LeaderID: "n1", PrevLogIndex: 0, PrevLogTerm: 0,
		Entries: []LogEntry{configEntry(t, joint)},
	}, reply)
	if !reply.Success {
		t.Fatalf("setup AppendEntries rejected: %+v", reply)
	}
	rs.Stop()

	restarted, err := NewRaftStateWithPersister("n2", []string{"n1", "n2", "n3"},
		make(chan ApplyMsg, 16), persister)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer restarted.Stop()

	config := restarted.GetClusterConfig()
	if !config.IsJoint() {
		t.Fatalf("restart lost the joint configuration: %+v", config)
	}
	if !config.IsVoter("n4") || config.Voters["n4"] != addrN4 {
		t.Errorf("restart lost the new voter and its address: %+v", config.Voters)
	}
}

// TestConfigurationSurvivesCompactionAndRestart covers the self-compaction path.
// Once log compaction discards the configuration entry, the configuration has to
// come from the snapshot boundary recorded alongside it — immediately, and again
// after a restart.
func TestConfigurationSurvivesCompactionAndRestart(t *testing.T) {
	persister := &memPersister{}
	rs, err := NewRaftStateWithPersister("n2", []string{"n1", "n2"},
		make(chan ApplyMsg, 16), persister)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}

	grown := &ClusterConfig{Voters: map[string]string{"n1": "a1", "n2": "a2", "n3": "a3"}}
	reply := &AppendEntriesReply{}
	rs.AppendEntries(&AppendEntriesArgs{
		Term: 2, LeaderID: "n1", PrevLogIndex: 0, PrevLogTerm: 0,
		Entries: []LogEntry{
			configEntry(t, grown),
			// One ordinary entry after it, so the compaction below does not empty
			// the log entirely.
			{Term: 2, Index: 2, Command: "cmd", Type: entryTypeCommand},
		},
	}, reply)
	if !reply.Success {
		t.Fatalf("setup AppendEntries rejected: %+v", reply)
	}

	// Compact past the configuration entry: it is gone from the log now.
	if err := rs.TruncateLogTo(1); err != nil {
		t.Fatalf("TruncateLogTo: %v", err)
	}
	if config := rs.GetClusterConfig(); !config.IsVoter("n3") {
		t.Fatalf("compaction lost the configuration: %+v", config.Voters)
	}
	rs.Stop()

	restarted, err := NewRaftStateWithPersister("n2", []string{"n1", "n2"},
		make(chan ApplyMsg, 16), persister)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer restarted.Stop()

	config := restarted.GetClusterConfig()
	if !config.IsVoter("n3") || len(config.Voters) != 3 {
		t.Errorf("restart after compaction lost the configuration: %+v", config.Voters)
	}
}

// TestConfigurationEntriesReachTheStateMachine checks the applier contract for
// configuration entries: they are delivered flagged rather than dropped, because
// the state machine has to move its applied index over every committed index or
// the ReadIndex catch-up wait never completes.
func TestConfigurationEntriesReachTheStateMachine(t *testing.T) {
	applyCh := make(chan ApplyMsg, 16)
	rs := NewRaftState("n2", []string{"n1", "n2"}, applyCh)
	defer rs.Stop()

	grown := &ClusterConfig{Voters: map[string]string{"n1": "a1", "n2": "a2", "n3": "a3"}}
	reply := &AppendEntriesReply{}
	rs.AppendEntries(&AppendEntriesArgs{
		Term: 2, LeaderID: "n1", PrevLogIndex: 0, PrevLogTerm: 0,
		Entries:      []LogEntry{configEntry(t, grown)},
		LeaderCommit: 1,
	}, reply)
	if !reply.Success {
		t.Fatalf("AppendEntries rejected: %+v", reply)
	}

	select {
	case msg := <-applyCh:
		if !msg.ConfigChange {
			t.Errorf("configuration entry was delivered unflagged: %+v", msg)
		}
		if msg.CommandIndex != 1 {
			t.Errorf("delivered index %d, want 1", msg.CommandIndex)
		}
	case <-time.After(configTestTimeout):
		t.Fatal("configuration entry never reached the state machine; the applied index would stall")
	}
}
