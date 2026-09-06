package unit

import (
	"sync"

	"rosetta/persistence"
	"rosetta/raft"
)

// fakeStorage is an in-memory persistence.Storage with per-file fault
// injection. Both durable files a node recovers from live behind one Storage,
// which is what lets a test fail exactly one of them and inspect the crash
// state that leaves behind (KNOWN_ISSUES.md R3).
type fakeStorage struct {
	mu           sync.Mutex
	state        *raft.PersistentState
	snapshot     *persistence.Snapshot
	failState    int
	failSnapshot int
}

// failNextStateSaves makes the next n SaveRaftState calls fail with errPersist.
func (s *fakeStorage) failNextStateSaves(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failState = n
}

// failNextSnapshotSaves makes the next n SaveSnapshot calls fail with errPersist.
func (s *fakeStorage) failNextSnapshotSaves(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failSnapshot = n
}

func (s *fakeStorage) SaveRaftState(state *raft.PersistentState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failState > 0 {
		s.failState--
		return errPersist
	}
	clone := clonePersistentState(state)
	s.state = &clone
	return nil
}

func (s *fakeStorage) LoadRaftState() (*raft.PersistentState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == nil {
		return &raft.PersistentState{Log: make([]raft.LogEntry, 0)}, nil
	}
	clone := clonePersistentState(s.state)
	return &clone, nil
}

func (s *fakeStorage) SaveSnapshot(snapshot *persistence.Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failSnapshot > 0 {
		s.failSnapshot--
		return errPersist
	}
	clone := *snapshot
	clone.Data = append([]byte(nil), snapshot.Data...)
	s.snapshot = &clone
	return nil
}

func (s *fakeStorage) LoadSnapshot() (*persistence.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshot == nil {
		return nil, nil
	}
	clone := *s.snapshot
	clone.Data = append([]byte(nil), s.snapshot.Data...)
	return &clone, nil
}

func (s *fakeStorage) Close() error { return nil }

// savedSnapshot returns the snapshot currently "on disk", or nil.
func (s *fakeStorage) savedSnapshot() *persistence.Snapshot {
	snapshot, _ := s.LoadSnapshot()
	return snapshot
}

// savedState returns the raft state currently "on disk".
func (s *fakeStorage) savedState() *raft.PersistentState {
	state, _ := s.LoadRaftState()
	return state
}

// seedSnapshot writes a snapshot directly, bypassing fault injection, so a test
// can construct a specific on-disk starting point.
func (s *fakeStorage) seedSnapshot(index, term int, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot = &persistence.Snapshot{
		LastIncludedIndex: index,
		LastIncludedTerm:  term,
		Data:              append([]byte(nil), data...),
	}
}

// seedState writes a raft state directly, bypassing fault injection.
func (s *fakeStorage) seedState(state raft.PersistentState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	clone := clonePersistentState(&state)
	s.state = &clone
}

var _ persistence.Storage = (*fakeStorage)(nil)
