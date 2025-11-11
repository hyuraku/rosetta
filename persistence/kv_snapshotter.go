package persistence

import (
	"encoding/json"
	"fmt"
)

// KVSnapshotter implements the kvstore.Snapshotter interface
type KVSnapshotter struct {
	storage Storage
}

// NewKVSnapshotter creates a new KV snapshotter
func NewKVSnapshotter(storage Storage) *KVSnapshotter {
	return &KVSnapshotter{
		storage: storage,
	}
}

// SaveSnapshot saves the KV store data as a snapshot
func (ks *KVSnapshotter) SaveSnapshot(data map[string]string, lastIncludedIndex, lastIncludedTerm int) error {
	// Marshal the data
	dataBytes, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to marshal snapshot data: %w", err)
	}

	snapshot := &Snapshot{
		LastIncludedIndex: lastIncludedIndex,
		LastIncludedTerm:  lastIncludedTerm,
		Data:              dataBytes,
	}

	return ks.storage.SaveSnapshot(snapshot)
}

// LoadSnapshot loads the KV store data from a snapshot
func (ks *KVSnapshotter) LoadSnapshot() (data map[string]string, lastIncludedIndex, lastIncludedTerm int, err error) {
	snapshot, err := ks.storage.LoadSnapshot()
	if err != nil {
		return nil, 0, 0, err
	}

	if snapshot == nil {
		return nil, 0, 0, nil
	}

	// Unmarshal the data
	var kvData map[string]string
	if err := json.Unmarshal(snapshot.Data, &kvData); err != nil {
		return nil, 0, 0, fmt.Errorf("failed to unmarshal snapshot data: %w", err)
	}

	return kvData, snapshot.LastIncludedIndex, snapshot.LastIncludedTerm, nil
}
