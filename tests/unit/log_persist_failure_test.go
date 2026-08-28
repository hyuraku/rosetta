package unit

import (
	"errors"
	"sync"
	"testing"
	"time"

	"rosetta/raft"
)

// togglePersister saves successfully until failing is set, so a test can build a
// log normally and then fail exactly the write it cares about.
type togglePersister struct {
	mu      sync.Mutex
	failing bool
	saves   int
}

func (p *togglePersister) setFailing(failing bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failing = failing
}

func (p *togglePersister) SaveRaftState(*raft.PersistentState) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.saves++
	if p.failing {
		return errPersist
	}
	return nil
}

func (p *togglePersister) LoadRaftState() (*raft.PersistentState, error) {
	return &raft.PersistentState{Log: make([]raft.LogEntry, 0)}, nil
}

func newTogglingState(t *testing.T, peers []string) (*raft.RaftState, *togglePersister) {
	t.Helper()

	applyCh := make(chan raft.ApplyMsg, 16)
	drain(applyCh)
	persister := &togglePersister{}

	rs, err := raft.NewRaftStateWithPersister(nodeID1, peers, applyCh, persister)
	if err != nil {
		t.Fatalf("unexpected setup error: %v", err)
	}
	return rs, persister
}

// C3: a leader counts its own log when advancing the commit index, so an entry
// that never reached stable storage could be committed and then lost on restart.
// AppendLogEntry must report the failure and leave no trace of the entry.
func TestAppendLogEntry_ReportsPersistFailureAndRollsBack(t *testing.T) {
	rs, persister := newTogglingState(t, []string{nodeID1})

	for i := 0; i < 2; i++ {
		if _, err := rs.AppendLogEntry("cmd", "command"); err != nil {
			t.Fatalf("setup AppendLogEntry: %v", err)
		}
	}

	persister.setFailing(true)
	index, err := rs.AppendLogEntry("doomed", "command")
	if err == nil {
		t.Fatal("AppendLogEntry returned nil error even though the entry was never persisted")
	}
	if !errors.Is(err, errPersist) {
		t.Errorf("error does not wrap the storage failure: %v", err)
	}
	if index != 0 {
		t.Errorf("index on failure = %d, want 0 (no index was assigned)", index)
	}

	// The append must be rolled back: memory and disk have to agree, otherwise
	// the un-persisted entry could still be replicated and committed.
	if last := rs.GetLastLogIndex(); last != 2 {
		t.Fatalf("last log index after failed append = %d, want 2 (append rolled back)", last)
	}
	if entry := rs.GetLogEntry(3); entry != nil {
		t.Fatalf("entry 3 still present after failed append: %+v", entry)
	}

	// Index 3 is still free, so a later successful append reuses it.
	persister.setFailing(false)
	index, err = rs.AppendLogEntry("retry", "command")
	if err != nil {
		t.Fatalf("AppendLogEntry after recovery: %v", err)
	}
	if index != 3 {
		t.Errorf("index after recovery = %d, want 3", index)
	}
}

// C3: a truncation that survives only in memory would come back as the discarded
// suffix after a restart, so TruncateLogAfter must restore the log it failed to
// shorten and report the error.
func TestTruncateLogAfter_ReportsPersistFailureAndRestoresLog(t *testing.T) {
	rs, persister := newTogglingState(t, []string{nodeID1})

	for i := 0; i < 3; i++ {
		if _, err := rs.AppendLogEntry("cmd", "command"); err != nil {
			t.Fatalf("setup AppendLogEntry: %v", err)
		}
	}

	persister.setFailing(true)
	err := rs.TruncateLogAfter(1)
	if err == nil {
		t.Fatal("TruncateLogAfter returned nil error even though the shortened log was never persisted")
	}
	if !errors.Is(err, errPersist) {
		t.Errorf("error does not wrap the storage failure: %v", err)
	}
	if last := rs.GetLastLogIndex(); last != 3 {
		t.Fatalf("last log index after failed truncation = %d, want 3 (truncation rolled back)", last)
	}
	for i := 1; i <= 3; i++ {
		entry := rs.GetLogEntry(i)
		if entry == nil {
			t.Fatalf("entry %d lost after a failed truncation", i)
		}
		if entry.Index != i {
			t.Errorf("entry at %d has Index %d", i, entry.Index)
		}
	}

	// Once storage recovers the same truncation succeeds and reports no error.
	persister.setFailing(false)
	if err := rs.TruncateLogAfter(1); err != nil {
		t.Fatalf("TruncateLogAfter after recovery: %v", err)
	}
	if last := rs.GetLastLogIndex(); last != 1 {
		t.Errorf("last log index after successful truncation = %d, want 1", last)
	}
}

// C3, production path: Start() goes through AppendLogEntry, so a leader that
// cannot persist must tell its caller the command was not started instead of
// reporting an index the cluster may never see.
func TestStart_ReportsPersistFailure(t *testing.T) {
	transport := raft.NewMockTransport()
	applyCh := make(chan raft.ApplyMsg, 16)
	drain(applyCh)
	persister := &togglePersister{}

	node, err := raft.NewRaftNodeWithPersister(nodeID1, []string{nodeID1}, transport, applyCh, persister)
	if err != nil {
		t.Fatalf("unexpected setup error: %v", err)
	}
	transport.RegisterNode(nodeID1, node)
	defer node.Kill()

	if !pollUntil(2*time.Second, node.IsLeader) {
		t.Fatal("single-node cluster did not elect a leader")
	}
	lastBefore := node.GetLogLength()

	persister.setFailing(true)
	index, _, isLeader, err := node.Start("doomed command")
	if !isLeader {
		t.Fatal("Start reported not-leader; the node is the leader, storage merely failed")
	}
	if err == nil {
		t.Fatal("Start returned nil error even though the command was never persisted")
	}
	if index > 0 {
		t.Errorf("Start returned index %d for a command that was rolled back", index)
	}
	if last := node.GetLogLength(); last != lastBefore {
		t.Errorf("log grew to %d after a failed Start, want %d", last, lastBefore)
	}
}
