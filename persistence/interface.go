package persistence

import "rosetta/raft"

// Storage defines the interface for persistent storage
type Storage interface {
	// SaveRaftState saves the persistent Raft state (term, votedFor, log)
	SaveRaftState(state *raft.PersistentState) error

	// LoadRaftState loads the persistent Raft state
	LoadRaftState() (*raft.PersistentState, error)

	// SaveSnapshot saves a snapshot of the state machine
	SaveSnapshot(snapshot *Snapshot) error

	// LoadSnapshot loads the latest snapshot
	LoadSnapshot() (*Snapshot, error)

	// Close closes the storage
	Close() error
}

// Snapshot represents a point-in-time snapshot of the state machine
type Snapshot struct {
	// LastIncludedIndex is the last log index included in the snapshot
	LastIncludedIndex int `json:"last_included_index"`

	// LastIncludedTerm is the term of the last log entry included
	LastIncludedTerm int `json:"last_included_term"`

	// Data contains the serialized state machine data
	Data []byte `json:"data"`
}
