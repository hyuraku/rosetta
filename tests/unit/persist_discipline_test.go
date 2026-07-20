package unit

import (
	"errors"
	"testing"

	"rosetta/raft"
)

// errPersist is returned by the fault-injecting persisters below.
var errPersist = errors.New("injected persist failure")

// faultySavePersister loads cleanly (as a brand-new node would) but always
// fails to save. It lets us exercise the "persist before responding" discipline
// on the RPC path without touching a real disk.
type faultySavePersister struct{}

func (faultySavePersister) SaveRaftState(*raft.PersistentState) error {
	return errPersist
}

func (faultySavePersister) LoadRaftState() (*raft.PersistentState, error) {
	// A fresh node: no prior state, no error.
	return &raft.PersistentState{Log: make([]raft.LogEntry, 0)}, nil
}

// faultyLoadPersister simulates a corrupt / unreadable on-disk state file.
type faultyLoadPersister struct{}

func (faultyLoadPersister) SaveRaftState(*raft.PersistentState) error { return nil }

func (faultyLoadPersister) LoadRaftState() (*raft.PersistentState, error) {
	return nil, errPersist
}

// Requirement ③: a node must refuse to start when it cannot load its persistent
// state, rather than silently resetting to term 0 (which could cause a double
// vote in a term it already acted in). This test is complete and demonstrates
// the expected pattern.
func TestNewRaftState_RefusesStartupOnLoadFailure(t *testing.T) {
	applyCh := make(chan raft.ApplyMsg, 10)
	peers := []string{"node1", "node2", "node3"}

	rs, err := raft.NewRaftStateWithPersister("node1", peers, applyCh, faultyLoadPersister{})
	if err == nil {
		t.Fatal("expected NewRaftStateWithPersister to fail when persistent state cannot be loaded, got nil error")
	}
	if rs != nil {
		t.Fatalf("expected nil RaftState on load failure, got %+v", rs)
	}
}

// Requirements ① and ②: RequestVote must persist the vote before replying, and
// when that persist fails it must NOT tell the candidate the vote was granted.
//
// The node starts fresh (term 0, no vote) but its persister always fails to
// save. A candidate at term 1 with an up-to-date log requests a vote.
func TestRequestVote_RefusesVoteWhenPersistFails(t *testing.T) {
	applyCh := make(chan raft.ApplyMsg, 10)
	peers := []string{"node1", "node2", "node3"}

	rs, err := raft.NewRaftStateWithPersister("node1", peers, applyCh, faultySavePersister{})
	if err != nil {
		t.Fatalf("unexpected setup error: %v", err)
	}

	args := &raft.RequestVoteArgs{
		Term:         1,
		CandidateID:  "node2",
		LastLogIndex: 0,
		LastLogTerm:  0,
	}
	reply := &raft.RequestVoteReply{}

	rs.RequestVote(args, reply)

	// The vote was not durably stored, so we must not acknowledge it: a crash
	// here would erase the record and let us vote again in term 1, risking two
	// leaders. Hence VoteGranted must be false.
	if reply.VoteGranted {
		t.Fatal("vote was granted even though the persist failed; this can cause a double vote after a crash")
	}

	// We still adopted the candidate's higher term in memory and report it, so
	// the candidate learns it is not behind. Term must reflect the bumped value.
	if reply.Term != 1 {
		t.Fatalf("expected reply.Term == 1 (adopted candidate's term), got %d", reply.Term)
	}

	// Safe direction: in memory VotedFor stays set to the candidate. It only
	// ever *prevents* further votes this term, never enables an extra one, so
	// keeping it set after a failed persist cannot cause a double vote. A later
	// successful persist reconciles memory and disk.
	if got := rs.GetVotedFor(); got == nil || *got != "node2" {
		t.Fatalf("expected in-memory VotedFor to remain \"node2\" (safe direction), got %v", got)
	}
}
