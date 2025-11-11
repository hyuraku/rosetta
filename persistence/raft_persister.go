package persistence

import (
	"rosetta/raft"
)

// RaftPersister implements the raft.Persister interface
type RaftPersister struct {
	storage Storage
}

// NewRaftPersister creates a new Raft persister
func NewRaftPersister(storage Storage) *RaftPersister {
	return &RaftPersister{
		storage: storage,
	}
}

// SaveRaftState saves the Raft persistent state
func (rp *RaftPersister) SaveRaftState(state *raft.PersistentState) error {
	return rp.storage.SaveRaftState(state)
}

// LoadRaftState loads the Raft persistent state
func (rp *RaftPersister) LoadRaftState() (*raft.PersistentState, error) {
	return rp.storage.LoadRaftState()
}
