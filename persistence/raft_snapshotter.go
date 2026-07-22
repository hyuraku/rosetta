package persistence

import "rosetta/raft"

// RaftSnapshotter is the production implementation of raft.Snapshotter. It is
// wired into the RaftNode via SetSnapshotter so a leader can serve
// InstallSnapshot RPCs to lagging followers.
//
// It shares the same Storage as KVSnapshotter, so the bytes it ships are exactly
// the snapshot the kvstore already persisted (V2 JSON: KVData + Sessions). That
// symmetry is what lets a follower's kvstore parse the InstallSnapshot payload.
type RaftSnapshotter struct {
	storage Storage
}

// Compile-time assertion that RaftSnapshotter satisfies the raft interface.
var _ raft.Snapshotter = (*RaftSnapshotter)(nil)

// NewRaftSnapshotter creates a raft.Snapshotter backed by the given Storage.
func NewRaftSnapshotter(storage Storage) *RaftSnapshotter {
	return &RaftSnapshotter{storage: storage}
}

// ReadSnapshot returns the current persisted snapshot bytes, or (nil, nil) when
// no snapshot has been taken yet. The leader calls this when sending an
// InstallSnapshot RPC to a lagging follower.
func (rs *RaftSnapshotter) ReadSnapshot() ([]byte, error) {
	snapshot, err := rs.storage.LoadSnapshot()
	if err != nil || snapshot == nil {
		return nil, err
	}
	return snapshot.Data, nil
}

// CreateSnapshot returns the state machine snapshot up to the given index. In
// production the kvstore persists its snapshot (via KVSnapshotter) before
// triggering log compaction, so the already-persisted bytes are the snapshot;
// this returns them rather than re-marshaling the state machine.
func (rs *RaftSnapshotter) CreateSnapshot(lastIncludedIndex, lastIncludedTerm int) ([]byte, error) {
	return rs.ReadSnapshot()
}

// InstallSnapshot persists a snapshot received from a leader. The production
// RPC path installs snapshots through the state machine's apply channel (which
// persists via KVSnapshotter), so this method is provided for interface
// completeness and writes the bytes to the same Storage.
func (rs *RaftSnapshotter) InstallSnapshot(data []byte, lastIncludedIndex, lastIncludedTerm int) error {
	return rs.storage.SaveSnapshot(&Snapshot{
		LastIncludedIndex: lastIncludedIndex,
		LastIncludedTerm:  lastIncludedTerm,
		Data:              data,
	})
}
